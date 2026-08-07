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


def test_runtime_mcp_parses_proxy_and_rewrites_dial_url(tmp_path: Path):
    _write_runtime_config(tmp_path, {
        "snapshot_id": "sha256:abc",
        "servers": [{
            "server_id": "exa",
            "name": "Exa",
            "url": "https://mcp.exa.ai/mcp",
            "timeout_seconds": 30,
        }],
        "proxy": {
            "base_url": "http://platform:8082",
            "capability_token": "token-1",
        },
    })

    snapshot = load_runtime_mcp_snapshot(tmp_path)

    assert snapshot.proxy is not None
    assert snapshot.proxy.base_url == "http://platform:8082"
    assert snapshot.proxy.capability_token == "token-1"
    server = snapshot.servers[0]
    assert server.dial_url == "http://platform:8082/v1/worker/mcp/exa"
    assert server.auth_headers() == {"Authorization": "Bearer token-1"}


def test_runtime_mcp_proxy_without_servers_is_still_valid(tmp_path: Path):
    # 代理配置随快照下发; 无 server 时不影响解析(签发侧不会同时出现)。
    _write_runtime_config(tmp_path, {
        "snapshot_id": "sha256:abc",
        "servers": [],
        "proxy": {
            "base_url": "http://platform:8082",
            "capability_token": "token-1",
        },
    })
    snapshot = load_runtime_mcp_snapshot(tmp_path)
    assert snapshot.servers == ()
    assert snapshot.proxy is not None


def test_runtime_mcp_rejects_invalid_proxy(tmp_path: Path):
    cases = [
        {"base_url": "", "capability_token": "t"},
        {"base_url": "http://platform:8082", "capability_token": ""},
        {"base_url": "http://user:pass@platform", "capability_token": "t"},
        {"base_url": "not-a-url", "capability_token": "t"},
        {"base_url": "http://platform:8082", "capability_token": "t", "extra": 1},
    ]
    for proxy in cases:
        _write_runtime_config(tmp_path, {
            "snapshot_id": "x",
            "servers": [],
            "proxy": proxy,
        })
        with pytest.raises(MCPConfigError):
            load_runtime_mcp_snapshot(tmp_path)
