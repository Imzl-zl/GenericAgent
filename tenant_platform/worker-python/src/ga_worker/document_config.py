"""Load Worker document gateway capability from signed runtime config."""

from __future__ import annotations

import json
from dataclasses import dataclass
from pathlib import Path
from typing import Any
from urllib.parse import urlparse
from uuid import UUID
import ipaddress

from ga_worker.credential_config import CredentialConfigError, load_runtime_document


class DocumentConfigError(ValueError):
    pass


@dataclass(frozen=True)
class RuntimeDocumentGateway:
    base_url: str
    capability_token: str
    session_key: str
    workspace_id: str


def load_runtime_document_gateway(config_root: Path) -> RuntimeDocumentGateway | None:
    root = Path(config_root)
    try:
        _, document = load_runtime_document(root)
    except (CredentialConfigError, OSError, json.JSONDecodeError) as exc:
        raise DocumentConfigError(f"cannot load signed runtime document config: {exc}") from exc

    raw = document.get("_platform_document")
    if raw is None:
        return None
    if not isinstance(raw, dict):
        raise DocumentConfigError("_platform_document must be an object")
    unknown = set(raw) - {
        "base_url", "capability_token", "session_key", "workspace_id",
    }
    if unknown:
        raise DocumentConfigError(f"_platform_document contains unknown fields: {sorted(unknown)}")

    gateway = RuntimeDocumentGateway(
        base_url=_string_field(raw, "base_url"),
        capability_token=_string_field(raw, "capability_token"),
        session_key=_string_field(raw, "session_key"),
        workspace_id=_string_field(raw, "workspace_id"),
    )
    _validate_gateway(gateway)
    return RuntimeDocumentGateway(
        base_url=gateway.base_url.rstrip("/"),
        capability_token=gateway.capability_token,
        session_key=gateway.session_key,
        workspace_id=gateway.workspace_id,
    )


def _string_field(raw: dict[str, Any], name: str) -> str:
    value = raw.get(name)
    if not isinstance(value, str) or not value.strip():
        raise DocumentConfigError(f"_platform_document.{name} is required")
    return value.strip()


def _validate_gateway(gateway: RuntimeDocumentGateway) -> None:
    parsed = urlparse(gateway.base_url)
    if parsed.scheme != "http" or not parsed.netloc:
        raise DocumentConfigError("_platform_document.base_url must use http loopback")
    if parsed.username or parsed.password:
        raise DocumentConfigError("_platform_document.base_url must not contain userinfo")
    if parsed.query or parsed.fragment:
        raise DocumentConfigError("_platform_document.base_url must not contain query or fragment")
    try:
        port = parsed.port
    except ValueError as exc:
        raise DocumentConfigError("_platform_document.base_url port is invalid") from exc
    if parsed.netloc.endswith(":") or (port is not None and not 1 <= port <= 65535):
        raise DocumentConfigError("_platform_document.base_url port is invalid")
    host = parsed.hostname or ""
    if host.lower() != "localhost":
        try:
            if not ipaddress.ip_address(host).is_loopback:
                raise DocumentConfigError("_platform_document.base_url must be loopback")
        except ValueError as exc:
            raise DocumentConfigError("_platform_document.base_url must be loopback") from exc
    if len(gateway.capability_token) > 4096:
        raise DocumentConfigError("_platform_document.capability_token is too large")
    if "\x00" in gateway.session_key or len(gateway.session_key) > 256:
        raise DocumentConfigError("_platform_document.session_key is invalid")
    try:
        UUID(gateway.workspace_id)
    except ValueError as exc:
        raise DocumentConfigError("_platform_document.workspace_id must be a UUID") from exc
