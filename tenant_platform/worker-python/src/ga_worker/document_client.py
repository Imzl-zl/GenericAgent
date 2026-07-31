"""Bounded loopback client for typed document operations and artifacts."""

from __future__ import annotations

import hashlib
import json
import re
import time
from dataclasses import dataclass
from email.message import Message
from typing import Any

import requests

from ga_worker.document_config import RuntimeDocumentGateway

MAX_ARTIFACT_BYTES = 8 * 1024 * 1024
MAX_ERROR_BODY_BYTES = 16 * 1024
HTTP_TIMEOUT = (2.0, 30.0)
_TERMINAL_COMMAND_STATUSES = frozenset({"succeeded", "failed", "expired"})
_VALID_COMMAND_STATUSES = frozenset({"pending", "executing"}) | _TERMINAL_COMMAND_STATUSES
_SHA256_RE = re.compile(r"^[a-f0-9]{64}$")


class DocumentGatewayError(RuntimeError):
    def __init__(self, message: str, *, code: str = "DOCUMENT_GATEWAY_ERROR") -> None:
        super().__init__(message)
        self.code = code


@dataclass(frozen=True)
class DocumentArtifactDownload:
    file_name: str
    media_type: str
    content: bytes
    sha256: str


class DocumentGatewayClient:
    def __init__(
        self,
        gateway: RuntimeDocumentGateway,
        *,
        session: Any | None = None,
        poll_interval_seconds: float = 0.25,
        command_timeout_seconds: float = 120.0,
    ) -> None:
        if poll_interval_seconds <= 0 or command_timeout_seconds <= 0:
            raise ValueError("document poll interval and timeout must be positive")
        self._base_url = gateway.base_url.rstrip("/")
        self._token = gateway.capability_token
        if session is None:
            transport = requests.Session()
            transport.trust_env = False
            self._session = transport
        else:
            self._session = session
        self._poll_interval = poll_interval_seconds
        self._command_timeout = command_timeout_seconds

    def export_docx(
        self,
        *,
        task_id: str,
        request_id: str,
        output_name: str,
        title: str,
        content: str,
    ) -> DocumentArtifactDownload:
        self.submit(task_id, "export_docx", request_id, "export_docx", {
            "output_name": output_name,
            "title": title,
            "content": content,
        })
        self.wait_for_command(task_id, "export_docx", request_id)
        return self.download_artifact(task_id, "export_docx", request_id)

    def submit(
        self,
        task_id: str,
        tool_name: str,
        request_id: str,
        operation: str,
        parameters: dict[str, Any],
    ) -> dict[str, Any]:
        return self._json_request("POST", "commands", json_body={
            "task_id": _required_text(task_id, "task_id", 128),
            "tool_name": _required_text(tool_name, "tool_name", 128),
            "request_id": _required_text(request_id, "request_id", 256),
            "operation": {
                "schema_version": 1,
                "operation": _required_text(operation, "operation", 128),
                "parameters": parameters,
            },
        }, expected_status=202)

    def status(self, task_id: str, tool_name: str, request_id: str) -> dict[str, Any]:
        return self._json_request("GET", "status", params={
            "task_id": _required_text(task_id, "task_id", 128),
            "tool_name": _required_text(tool_name, "tool_name", 128),
            "request_id": _required_text(request_id, "request_id", 256),
        })

    def wait_for_command(self, task_id: str, tool_name: str, request_id: str) -> dict[str, Any]:
        deadline = time.monotonic() + self._command_timeout
        while True:
            payload = self.status(task_id, tool_name, request_id)
            command = payload.get("command")
            status = command.get("status") if isinstance(command, dict) else None
            if status not in _VALID_COMMAND_STATUSES:
                raise DocumentGatewayError("document gateway returned an invalid command status")
            if status == "succeeded":
                return payload
            if status in {"failed", "expired"}:
                raise DocumentGatewayError(f"document command failed with status {status}")
            if time.monotonic() >= deadline:
                raise DocumentGatewayError("document command timed out")
            time.sleep(self._poll_interval)

    def download_artifact(self, task_id: str, tool_name: str, request_id: str) -> DocumentArtifactDownload:
        response = self._request("GET", "artifact", params={
            "task_id": _required_text(task_id, "task_id", 128),
            "tool_name": _required_text(tool_name, "tool_name", 128),
            "request_id": _required_text(request_id, "request_id", 256),
        }, stream=True)
        try:
            if response.status_code != 200:
                raise self._response_error(response)
            length = _content_length(response.headers.get("Content-Length"))
            if length <= 0 or length > MAX_ARTIFACT_BYTES:
                raise DocumentGatewayError("document artifact is empty or too large")
            media_type = _required_text(response.headers.get("Content-Type", "").split(";", 1)[0], "artifact media type", 128)
            file_name = _artifact_file_name(response.headers.get("Content-Disposition", ""))
            expected_digest = response.headers.get("X-Content-SHA256", "").strip()
            if not _SHA256_RE.fullmatch(expected_digest):
                raise DocumentGatewayError("document artifact digest is invalid")
            chunks: list[bytes] = []
            size = 0
            digest = hashlib.sha256()
            for chunk in response.iter_content(chunk_size=64 * 1024):
                if not chunk:
                    continue
                size += len(chunk)
                if size > MAX_ARTIFACT_BYTES or size > length:
                    raise DocumentGatewayError("document artifact is too large")
                digest.update(chunk)
                chunks.append(bytes(chunk))
            if size != length:
                raise DocumentGatewayError("document artifact size does not match Content-Length")
            actual_digest = digest.hexdigest()
            if actual_digest != expected_digest:
                raise DocumentGatewayError("document artifact digest mismatch")
            return DocumentArtifactDownload(file_name, media_type, b"".join(chunks), actual_digest)
        finally:
            response.close()

    def close(self, task_id: str) -> dict[str, Any]:
        try:
            return self._json_request("POST", "close", json_body={
                "task_id": _required_text(task_id, "task_id", 128),
                "tool_name": "document_job_close",
            })
        except DocumentGatewayError as exc:
            if exc.code == "DOCUMENT_JOB_NOT_FOUND":
                return {"job": {"status": "absent", "commands_closed": True}}
            raise

    def close_transport(self) -> None:
        close = getattr(self._session, "close", None)
        if callable(close):
            close()

    def _json_request(
        self,
        method: str,
        endpoint: str,
        *,
        json_body: dict[str, Any] | None = None,
        params: dict[str, str] | None = None,
        expected_status: int = 200,
    ) -> dict[str, Any]:
        response = self._request(method, endpoint, json_body=json_body, params=params)
        try:
            if response.status_code != expected_status:
                raise self._response_error(response)
            payload = response.json()
            if not isinstance(payload, dict):
                raise DocumentGatewayError("document gateway returned invalid JSON")
            return payload
        except ValueError as exc:
            raise DocumentGatewayError("document gateway returned invalid JSON") from exc
        finally:
            response.close()

    def _request(
        self,
        method: str,
        endpoint: str,
        *,
        json_body: dict[str, Any] | None = None,
        params: dict[str, str] | None = None,
        stream: bool = False,
    ) -> Any:
        try:
            return self._session.request(
                method,
                f"{self._base_url}/{endpoint}",
                headers={"Authorization": f"Bearer {self._token}", "Accept": "application/json" if not stream else "application/octet-stream"},
                json=json_body,
                params=params,
                timeout=HTTP_TIMEOUT,
                allow_redirects=False,
                stream=stream,
            )
        except requests.RequestException as exc:
            raise DocumentGatewayError(f"document gateway request failed: {exc}") from exc

    @staticmethod
    def _response_error(response: Any) -> DocumentGatewayError:
        code = f"HTTP_{response.status_code}"
        message = "document gateway request failed"
        try:
            payload = response.json()
            if isinstance(payload, dict):
                raw_code = payload.get("code")
                raw_message = payload.get("message")
                if isinstance(raw_code, str) and raw_code:
                    code = raw_code[:128]
                if isinstance(raw_message, str) and raw_message:
                    message = raw_message[:1024]
        except (ValueError, TypeError):
            pass
        return DocumentGatewayError(f"{code}: {message}", code=code)


def _required_text(value: Any, name: str, maximum: int) -> str:
    if not isinstance(value, str) or not value.strip() or len(value.strip().encode("utf-8")) > maximum or "\x00" in value:
        raise DocumentGatewayError(f"{name} is invalid")
    return value.strip()


def _content_length(raw: Any) -> int:
    try:
        return int(str(raw), 10)
    except (TypeError, ValueError) as exc:
        raise DocumentGatewayError("document artifact Content-Length is invalid") from exc


def _artifact_file_name(content_disposition: str) -> str:
    message = Message()
    message["content-disposition"] = content_disposition
    file_name = message.get_filename()
    if not isinstance(file_name, str):
        raise DocumentGatewayError("document artifact file name is missing")
    file_name = file_name.strip()
    if (
        not file_name
        or len(file_name.encode("utf-8")) > 255
        or file_name in {".", ".."}
        or "/" in file_name
        or "\\" in file_name
        or any(ord(char) < 32 or ord(char) == 127 for char in file_name)
        or not file_name.lower().endswith(".docx")
    ):
        raise DocumentGatewayError("document artifact file name is invalid")
    return file_name
