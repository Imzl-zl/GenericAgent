from __future__ import annotations

import hashlib
import json
from pathlib import Path

import pytest

from ga_worker.document_config import DocumentConfigError, load_runtime_document_gateway


def _write_runtime_config(config_root: Path, document_gateway: dict | None) -> None:
    placeholder = "0" * 64
    document = {
        "_platform_runtime": {
            "credential_generation": 1,
            "config_checksum": placeholder,
            "routing_snapshot_id": "providers-1",
        },
    }
    if document_gateway is not None:
        document["_platform_document"] = document_gateway
    encoded = json.dumps(document, ensure_ascii=False, separators=(",", ":"), sort_keys=True) + "\n"
    checksum = hashlib.sha256(encoded.encode()).hexdigest()
    (config_root / "mykey.runtime.json").write_bytes(
        encoded.replace(placeholder, checksum, 1).encode("utf-8")
    )


def test_missing_document_gateway_is_disabled(tmp_path: Path):
    _write_runtime_config(tmp_path, None)
    assert load_runtime_document_gateway(tmp_path) is None


def test_load_runtime_document_gateway_validates_signed_loopback_config(tmp_path: Path):
    _write_runtime_config(tmp_path, {
        "base_url": "http://127.0.0.1:8080/document-gateway/",
        "capability_token": "document-capability-token",
        "session_key": "personal:42",
        "workspace_id": "11111111-1111-1111-1111-111111111111",
    })

    gateway = load_runtime_document_gateway(tmp_path)

    assert gateway is not None
    assert gateway.base_url == "http://127.0.0.1:8080/document-gateway"
    assert gateway.capability_token == "document-capability-token"
    assert gateway.session_key == "personal:42"
    assert gateway.workspace_id == "11111111-1111-1111-1111-111111111111"


@pytest.mark.parametrize("mutate,match", [
    (lambda g: {**g, "base_url": "https://example.com"}, "loopback"),
    (lambda g: {**g, "base_url": "http://127.0.0.1:8080/path?x=1"}, "query"),
    (lambda g: {**g, "base_url": "http://127.0.0.1:99999/path"}, "port"),
    (lambda g: {**g, "base_url": "http://127.0.0.1:/path"}, "port"),
    (lambda g: {**g, "capability_token": ""}, "capability_token"),
    (lambda g: {**g, "workspace_id": "not-a-uuid"}, "workspace_id"),
    (lambda g: {**g, "database_url": "postgres://secret"}, "unknown fields"),
    (lambda g: {**g, "runtime_binary": "docker"}, "unknown fields"),
])
def test_invalid_document_gateway_is_rejected(tmp_path: Path, mutate, match: str):
    valid = {
        "base_url": "http://127.0.0.1:8080/document-gateway",
        "capability_token": "document-capability-token",
        "session_key": "personal:42",
        "workspace_id": "11111111-1111-1111-1111-111111111111",
    }
    _write_runtime_config(tmp_path, mutate(valid))

    with pytest.raises(DocumentConfigError, match=match):
        load_runtime_document_gateway(tmp_path)
