from __future__ import annotations

import hashlib
import json
from pathlib import Path

import pytest

from ga_worker.mcp_config import MCPConfigError, load_runtime_mcp_snapshot


def _write_runtime_config(config_root: Path, mcp: dict | None) -> None:
    placeholder = "0" * 64
    document = {
        "_platform_runtime": {
            "credential_generation": 1,
            "config_checksum": placeholder,
            "routing_snapshot_id": "providers-1",
        },
    }
    if mcp is not None:
        document["_platform_mcp"] = mcp
    encoded = json.dumps(document, ensure_ascii=False, separators=(",", ":"), sort_keys=True) + "\n"
    checksum = hashlib.sha256(encoded.encode()).hexdigest()
    (config_root / "mykey.runtime.json").write_bytes(
        encoded.replace(placeholder, checksum, 1).encode("utf-8")
    )


def test_load_runtime_mcp_snapshot_validates_signed_config(tmp_path: Path):
    _write_runtime_config(tmp_path, {
        "snapshot_id": "sha256:abc",
        "servers": [{
            "server_id": "exa",
            "name": "Exa",
            "url": "https://mcp.exa.ai/mcp",
            "timeout_seconds": 30,
        }],
    })

    snapshot = load_runtime_mcp_snapshot(tmp_path)

    assert snapshot.snapshot_id == "sha256:abc"
    assert len(snapshot.servers) == 1
    assert snapshot.servers[0].server_id == "exa"
    assert snapshot.servers[0].url == "https://mcp.exa.ai/mcp"


def test_missing_mcp_section_is_an_empty_catalog(tmp_path: Path):
    _write_runtime_config(tmp_path, None)
    snapshot = load_runtime_mcp_snapshot(tmp_path)
    assert snapshot.snapshot_id == "disabled"
    assert snapshot.servers == ()


def test_duplicate_or_invalid_server_is_rejected(tmp_path: Path):
    server = {
        "server_id": "bad id", "name": "Bad", "url": "https://example.com/mcp",
        "timeout_seconds": 30,
    }
    _write_runtime_config(tmp_path, {"snapshot_id": "x", "servers": [server, server]})
    with pytest.raises(MCPConfigError, match="server_id"):
        load_runtime_mcp_snapshot(tmp_path)


@pytest.mark.parametrize("unsupported", [
    {"headers": {}},
    {"max_response_bytes": 1024},
])
def test_runtime_mcp_rejects_unsupported_server_fields(tmp_path: Path, unsupported: dict):
    server = {
        "server_id": "exa",
        "name": "Exa",
        "url": "https://mcp.exa.ai/mcp",
        "timeout_seconds": 30,
        **unsupported,
    }
    _write_runtime_config(tmp_path, {"snapshot_id": "x", "servers": [server]})

    with pytest.raises(MCPConfigError, match="unknown fields"):
        load_runtime_mcp_snapshot(tmp_path)
