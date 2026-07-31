from __future__ import annotations

import hashlib
import json
import sys
import os
import types
from dataclasses import dataclass
from pathlib import Path
from typing import Any

import pytest

from ga_worker.document_client import (
    DocumentArtifactDownload,
    DocumentGatewayClient,
    DocumentGatewayError,
)
from ga_worker.document_config import RuntimeDocumentGateway
from ga_worker.document_instrument import install_document_tools
from ga_worker.legacy_instrument import apply_tool_policy, restore_tool_schema
from ga_worker.limits import ToolPolicy
from ga_worker.session_files import ensure_session_sandbox, persist_document_artifact


class FakeResponse:
    def __init__(self, status: int, *, json_body: Any = None, content: bytes = b"", headers: dict[str, str] | None = None):
        self.status_code = status
        self._json_body = json_body
        self._content = content
        self.headers = headers or {}

    def json(self) -> Any:
        if self._json_body is None:
            raise ValueError("not json")
        return self._json_body

    def iter_content(self, chunk_size: int):
        for offset in range(0, len(self._content), chunk_size):
            yield self._content[offset:offset + chunk_size]

    def close(self) -> None:
        pass


class FakeSession:
    def __init__(self, responses: list[Any]):
        self.responses = list(responses)
        self.calls: list[dict[str, Any]] = []

    def request(self, method: str, url: str, **kwargs: Any) -> FakeResponse:
        self.calls.append({"method": method, "url": url, **kwargs})
        if not self.responses:
            raise AssertionError("unexpected HTTP request")
        response = self.responses.pop(0)
        if isinstance(response, BaseException):
            raise response
        return response


def gateway() -> RuntimeDocumentGateway:
    return RuntimeDocumentGateway(
        base_url="http://127.0.0.1:8080/v1/document",
        capability_token="document-token",
        session_key="personal:42",
        workspace_id="11111111-1111-1111-1111-111111111111",
    )


def test_default_transport_does_not_trust_proxy_environment():
    client = DocumentGatewayClient(gateway())
    try:
        assert client._session.trust_env is False
    finally:
        client.close_transport()


def test_export_docx_submits_polls_and_downloads_without_closing_job(monkeypatch: pytest.MonkeyPatch):
    artifact = b"complete-docx"
    digest = hashlib.sha256(artifact).hexdigest()
    http = FakeSession([
        FakeResponse(202, json_body={"job": {"job_id": "job-1", "status": "queued"}, "command": {"command_id": "cmd-1", "status": "pending"}}),
        FakeResponse(200, json_body={"job": {"job_id": "job-1", "status": "running"}, "command": {"command_id": "cmd-1", "status": "executing"}}),
        FakeResponse(200, json_body={"job": {"job_id": "job-1", "status": "running"}, "command": {"command_id": "cmd-1", "status": "succeeded"}}),
        FakeResponse(200, content=artifact, headers={
            "Content-Type": "application/vnd.openxmlformats-officedocument.wordprocessingml.document",
            "Content-Length": str(len(artifact)),
            "Content-Disposition": 'attachment; filename="report.docx"',
            "X-Content-SHA256": digest,
        }),
    ])
    client = DocumentGatewayClient(gateway(), session=http, poll_interval_seconds=0.001, command_timeout_seconds=1)
    monkeypatch.setattr("ga_worker.document_client.time.sleep", lambda _: None)

    download = client.export_docx(
        task_id="task-1", request_id="call-1", output_name="report.docx", title="Q2", content="hello",
    )
    assert download == DocumentArtifactDownload(
        file_name="report.docx",
        media_type="application/vnd.openxmlformats-officedocument.wordprocessingml.document",
        content=artifact,
        sha256=digest,
    )
    assert [call["url"].rsplit("/", 1)[-1] for call in http.calls] == ["commands", "status", "status", "artifact"]
    assert all(call["headers"]["Authorization"] == "Bearer document-token" for call in http.calls)
    submitted = http.calls[0]["json"]
    assert submitted == {
        "task_id": "task-1", "tool_name": "export_docx", "request_id": "call-1",
        "operation": {"schema_version": 1, "operation": "export_docx", "parameters": {"output_name": "report.docx", "title": "Q2", "content": "hello"}},
    }


def test_export_docx_allows_multiple_commands_before_close(monkeypatch: pytest.MonkeyPatch):
    artifact = b"docx"
    digest = hashlib.sha256(artifact).hexdigest()
    artifact_response = lambda name: FakeResponse(200, content=artifact, headers={
        "Content-Type": "application/vnd.openxmlformats-officedocument.wordprocessingml.document",
        "Content-Length": str(len(artifact)),
        "Content-Disposition": f'attachment; filename="{name}"',
        "X-Content-SHA256": digest,
    })
    http = FakeSession([
        FakeResponse(202, json_body={"job": {"status": "queued"}, "command": {"status": "pending"}}),
        FakeResponse(200, json_body={"job": {"status": "running"}, "command": {"status": "succeeded"}}),
        artifact_response("first.docx"),
        FakeResponse(202, json_body={"job": {"status": "running"}, "command": {"status": "pending"}}),
        FakeResponse(200, json_body={"job": {"status": "running"}, "command": {"status": "succeeded"}}),
        artifact_response("second.docx"),
    ])
    client = DocumentGatewayClient(gateway(), session=http, poll_interval_seconds=0.001, command_timeout_seconds=1)
    monkeypatch.setattr("ga_worker.document_client.time.sleep", lambda _: None)

    first = client.export_docx(task_id="task-1", request_id="call-1", output_name="first.docx", title="", content="first")
    second = client.export_docx(task_id="task-1", request_id="call-2", output_name="second.docx", title="", content="second")

    assert (first.file_name, second.file_name) == ("first.docx", "second.docx")
    assert [call["url"].rsplit("/", 1)[-1] for call in http.calls] == [
        "commands", "status", "artifact", "commands", "status", "artifact",
    ]


def test_export_docx_failure_leaves_job_open_for_terminal_cleanup(monkeypatch: pytest.MonkeyPatch):
    http = FakeSession([
        FakeResponse(202, json_body={"job": {"job_id": "job-1", "status": "queued"}, "command": {"command_id": "cmd-1", "status": "pending"}}),
        FakeResponse(200, json_body={"job": {"job_id": "job-1", "status": "running"}, "command": {"command_id": "cmd-1", "status": "failed"}}),
    ])
    client = DocumentGatewayClient(gateway(), session=http, poll_interval_seconds=0.001, command_timeout_seconds=1)
    monkeypatch.setattr("ga_worker.document_client.time.sleep", lambda _: None)
    with pytest.raises(DocumentGatewayError, match="command failed"):
        client.export_docx(task_id="task-1", request_id="call-1", output_name="report.docx", title="", content="hello")
    assert http.calls[-1]["url"].endswith("/status")


def test_close_treats_authorized_missing_job_as_idempotent_success():
    http = FakeSession([FakeResponse(404, json_body={
        "code": "DOCUMENT_JOB_NOT_FOUND",
        "message": "document job or command not found",
        "trace_id": "trace-1",
    })])
    client = DocumentGatewayClient(gateway(), session=http)

    result = client.close("task-1")

    assert result["job"] == {"status": "absent", "commands_closed": True}


def test_download_rejects_oversize_digest_mismatch_and_unsafe_name():
    body = b"docx"
    good_digest = hashlib.sha256(body).hexdigest()
    cases = [
        ({"Content-Type": "application/octet-stream", "Content-Length": str(8 * 1024 * 1024 + 1), "Content-Disposition": 'attachment; filename="a.docx"', "X-Content-SHA256": good_digest}, body, "too large"),
        ({"Content-Type": "application/octet-stream", "Content-Length": str(len(body)), "Content-Disposition": 'attachment; filename="a.docx"', "X-Content-SHA256": "0" * 64}, body, "digest"),
        ({"Content-Type": "application/octet-stream", "Content-Length": str(len(body)), "Content-Disposition": 'attachment; filename="../a.docx"', "X-Content-SHA256": good_digest}, body, "file name"),
    ]
    for headers, content, match in cases:
        client = DocumentGatewayClient(gateway(), session=FakeSession([FakeResponse(200, content=content, headers=headers)]))
        with pytest.raises(DocumentGatewayError, match=match):
            client.download_artifact("task-1", "export_docx", "call-1")


def test_document_instrument_uses_gateway_and_session_output(tmp_path: Path, monkeypatch: pytest.MonkeyPatch):
    class StepOutcome:
        def __init__(self, data, next_prompt=None, should_exit=False):
            self.data, self.next_prompt, self.should_exit = data, next_prompt, should_exit

    monkeypatch.setitem(sys.modules, "agent_loop", types.SimpleNamespace(StepOutcome=StepOutcome))
    artifact = b"gateway-docx"
    digest = hashlib.sha256(artifact).hexdigest()

    class FakeClient:
        def __init__(self):
            self.calls: list[tuple[str, str]] = []

        def export_docx(self, **kwargs):
            self.calls.append((kwargs["task_id"], kwargs["request_id"]))
            return DocumentArtifactDownload("report.docx", "application/docx", artifact, digest)

    class FakeHandler:
        pass

    ga_mod = types.SimpleNamespace(GenericAgentHandler=FakeHandler)
    agentmain_mod = types.SimpleNamespace(TOOLS_SCHEMA=[{"type": "function", "function": {"name": "file_read"}}])
    overlay = tmp_path / "runtime" / "session" / "legacy-overlay"
    overlay.mkdir(parents=True)
    client = FakeClient()
    session = types.SimpleNamespace(
        overlay_dir=overlay, session_key="personal:42", agent=types.SimpleNamespace(handler=None),
        generated_output_files=[], document_client=client, document_open_task_id=None,
    )
    mods = {"ga": ga_mod, "agentmain": agentmain_mod}
    unwrap = install_document_tools(session, "task-1", mods)
    policy = ToolPolicy(
        version="docs.v1",
        allowed_tools=frozenset({"export_docx", "document_job_submit", "document_job_close"}),
    )
    previous = apply_tool_policy(policy, mods)
    try:
        assert [schema["function"]["name"] for schema in agentmain_mod.TOOLS_SCHEMA] == [
            "export_docx", "document_job_submit", "document_job_close",
        ]
        response = types.SimpleNamespace(tool_calls=[types.SimpleNamespace(id="provider-call-1")])
        gen = FakeHandler().do_export_docx(
            {"path": "outputs/report.docx", "content": "hello", "_index": 0, "_tool_num": 1},
            response,
        )
        chunks = []
        try:
            while True:
                chunks.append(next(gen))
        except StopIteration as stopped:
            outcome = stopped.value
        assert outcome.data == {"status": "success", "path": "outputs/report.docx"}
        assert session.generated_output_files == ["outputs/report.docx"]
        sandbox = tmp_path / "runtime" / "session_files" / hashlib.sha256(b"personal:42").hexdigest()
        assert (sandbox / "outputs" / "report.docx").read_bytes() == artifact
        assert client.calls[0][0] == "task-1"
        assert client.calls[0][1].startswith("worker:")
        assert any("outputs/report.docx" in chunk for chunk in chunks)
    finally:
        restore_tool_schema(previous, mods)
        unwrap()


def test_export_handler_marks_job_open_before_submit_response(tmp_path: Path, monkeypatch: pytest.MonkeyPatch):
    class StepOutcome:
        def __init__(self, data, next_prompt=None, should_exit=False):
            self.data = data

    monkeypatch.setitem(sys.modules, "agent_loop", types.SimpleNamespace(StepOutcome=StepOutcome))

    class LostResponseClient:
        def export_docx(self, **kwargs):
            raise DocumentGatewayError("document gateway request failed: response lost")

    class FakeHandler:
        pass

    ga_mod = types.SimpleNamespace(GenericAgentHandler=FakeHandler)
    agentmain_mod = types.SimpleNamespace(TOOLS_SCHEMA=[])
    overlay = tmp_path / "runtime" / "session" / "legacy-overlay"
    overlay.mkdir(parents=True)
    session = types.SimpleNamespace(
        overlay_dir=overlay, session_key="personal:42", agent=types.SimpleNamespace(handler=None),
        generated_output_files=[], document_client=LostResponseClient(), document_open_task_id=None,
    )
    mods = {"ga": ga_mod, "agentmain": agentmain_mod}
    unwrap = install_document_tools(session, "task-1", mods)
    previous = apply_tool_policy(ToolPolicy(version="docs.v1", allowed_tools=frozenset({"export_docx"})), mods)
    try:
        response = types.SimpleNamespace(tool_calls=[types.SimpleNamespace(id="provider-call-lost")])
        generator = FakeHandler().do_export_docx({"content": "hello", "_index": 0}, response)
        while True:
            try:
                next(generator)
            except StopIteration as stopped:
                assert stopped.value.data["status"] == "error"
                break
        assert session.document_open_task_id == "task-1"
    finally:
        restore_tool_schema(previous, mods)
        unwrap()


def test_document_instrument_requires_capability_and_exact_policy(tmp_path: Path):
    class FakeHandler:
        pass

    ga_mod = types.SimpleNamespace(GenericAgentHandler=FakeHandler)
    agentmain_mod = types.SimpleNamespace(TOOLS_SCHEMA=[])
    overlay = tmp_path / "runtime" / "session" / "legacy-overlay"
    overlay.mkdir(parents=True)
    session = types.SimpleNamespace(
        overlay_dir=overlay, session_key="personal:42", agent=types.SimpleNamespace(handler=None),
        generated_output_files=[], document_client=None,
    )
    mods = {"ga": ga_mod, "agentmain": agentmain_mod}
    unwrap = install_document_tools(session, "task-1", mods)
    try:
        assert not hasattr(FakeHandler, "do_export_docx")
        assert not hasattr(agentmain_mod, "_tenant_custom_tools_schema")
    finally:
        unwrap()

    session.document_client = object()
    unwrap = install_document_tools(session, "task-1", mods)
    previous = apply_tool_policy(ToolPolicy(version="deny", allowed_tools=frozenset({"file_read"})), mods)
    try:
        assert agentmain_mod.TOOLS_SCHEMA == []
    finally:
        restore_tool_schema(previous, mods)
        unwrap()


def test_persist_document_artifact_is_atomic_bounded_and_never_overwrites(tmp_path):
    root = ensure_session_sandbox(tmp_path, "personal:42")
    first = b"first-docx"
    first_digest = hashlib.sha256(first).hexdigest()
    rel = persist_document_artifact(root, "report.docx", first, first_digest)
    assert rel == "outputs/report.docx"
    assert (root / rel).read_bytes() == first
    if os.name != "nt":
        assert (root / rel).stat().st_mode & 0o777 == 0o640
    assert persist_document_artifact(root, "report.docx", first, first_digest) == rel

    second = b"second-docx"
    second_digest = hashlib.sha256(second).hexdigest()
    second_rel = persist_document_artifact(root, "report.docx", second, second_digest)
    assert second_rel == f"outputs/report-{second_digest[:12]}.docx"
    assert (root / rel).read_bytes() == first
    assert (root / second_rel).read_bytes() == second

    with pytest.raises(ValueError, match="file name"):
        persist_document_artifact(root, "../escape.docx", first, first_digest)
    with pytest.raises(ValueError, match="digest"):
        persist_document_artifact(root, "bad.docx", first, "0" * 64)


def test_persist_document_artifact_rejects_symlinked_outputs(tmp_path):
    root = ensure_session_sandbox(tmp_path, "personal:42")
    outside = tmp_path / "outside"
    outside.mkdir()
    outputs = root / "outputs"
    outputs.rmdir()
    try:
        outputs.symlink_to(outside, target_is_directory=True)
    except OSError:
        pytest.skip("directory symlinks are unavailable")
    content = b"docx"
    with pytest.raises(ValueError, match="outputs"):
        persist_document_artifact(root, "report.docx", content, hashlib.sha256(content).hexdigest())
    assert list(outside.iterdir()) == []


def test_persist_document_artifact_rejects_outputs_directory_swap(tmp_path: Path, monkeypatch):
    if os.name == "nt":
        pytest.skip("directory-fd race test requires POSIX")
    root = ensure_session_sandbox(tmp_path, "personal:42")
    outputs = root / "outputs"
    moved = root / "outputs-moved"
    outside = tmp_path / "outside"
    outside.mkdir()
    original_open = os.open
    swapped = False

    def racing_open(path, flags, mode=0o777, *, dir_fd=None):
        nonlocal swapped
        if not swapped and Path(path).name.startswith(".ga-document-"):
            swapped = True
            outputs.rename(moved)
            outputs.symlink_to(outside, target_is_directory=True)
        return original_open(path, flags, mode, dir_fd=dir_fd)

    monkeypatch.setattr(os, "open", racing_open)
    content = b"docx"
    with pytest.raises(ValueError, match="outputs directory"):
        persist_document_artifact(root, "report.docx", content, hashlib.sha256(content).hexdigest())
    assert list(outside.iterdir()) == []


def test_gateway_errors_are_explicit_and_bounded():
    error_body = {
        "code": "DOCUMENT_STATE_CONFLICT",
        "message": "document request conflicts with current state",
        "trace_id": "trace-1",
    }
    client = DocumentGatewayClient(gateway(), session=FakeSession([FakeResponse(409, json_body=error_body)]))
    with pytest.raises(DocumentGatewayError, match="DOCUMENT_STATE_CONFLICT"):
        client.status("task-1", "export_docx", "call-1")
