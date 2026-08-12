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


def test_runtime_mcp_parses_stdio_server(tmp_path: Path):
    _write_runtime_config(tmp_path, {
        "snapshot_id": "sha256:abc",
        "servers": [{
            "server_id": "serena",
            "name": "Serena",
            "url": "",
            "timeout_seconds": 60,
            "transport": "stdio",
            "command": "serena",
            "args": ["start-mcp-server", "--context=agent"],
        }],
    })

    snapshot = load_runtime_mcp_snapshot(tmp_path)

    server = snapshot.servers[0]
    assert server.transport == "stdio"
    assert server.command == "serena"
    assert server.args == ("start-mcp-server", "--context=agent")


def test_runtime_mcp_omitted_stdio_fields_default_to_http(tmp_path: Path):
    _write_runtime_config(tmp_path, {
        "snapshot_id": "sha256:abc",
        "servers": [{
            "server_id": "exa",
            "name": "Exa",
            "url": "https://mcp.exa.ai/mcp",
            "timeout_seconds": 30,
        }],
    })

    server = load_runtime_mcp_snapshot(tmp_path).servers[0]

    assert server.transport == "http"
    assert server.command == ""
    assert server.args == ()


@pytest.mark.parametrize("bad_args", [42, "start-mcp-server", [1, 2]])
def test_runtime_mcp_rejects_non_string_args(tmp_path: Path, bad_args):
    _write_runtime_config(tmp_path, {
        "snapshot_id": "x",
        "servers": [{
            "server_id": "serena",
            "name": "Serena",
            "url": "",
            "transport": "stdio",
            "command": "serena",
            "args": bad_args,
        }],
    })

    with pytest.raises(MCPConfigError, match="args must be an array"):
        load_runtime_mcp_snapshot(tmp_path)


def test_runtime_mcp_rejects_stdio_without_command(tmp_path: Path):
    _write_runtime_config(tmp_path, {
        "snapshot_id": "x",
        "servers": [{
            "server_id": "serena",
            "name": "Serena",
            "url": "",
            "transport": "stdio",
        }],
    })

    with pytest.raises(MCPConfigError, match="command is required"):
        load_runtime_mcp_snapshot(tmp_path)


def test_runtime_mcp_mixed_transport_with_proxy_only_affects_http(tmp_path: Path):
    # 回归(推送审查): proxy 只对 http server 生效——混布快照里 stdio 行若
    # 携带 proxy_base_url 会触发 MCPServerConfig.validate() 的 stdio 拒绝
    # 分支, 导致整个快照加载失败(StartSession 直接挂)。compose 部署
    # (MCPProxyBaseURL 非空)下这是 stdio 恢复的默认配置形态。
    _write_runtime_config(tmp_path, {
        "snapshot_id": "sha256:abc",
        "proxy": {
            "base_url": "http://platform:8082",
            "capability_token": "jwt-token",
        },
        "servers": [{
            "server_id": "exa",
            "name": "Exa",
            "url": "https://mcp.exa.ai/mcp",
            "timeout_seconds": 30,
        }, {
            "server_id": "serena",
            "name": "Serena",
            "url": "",
            "timeout_seconds": 60,
            "transport": "stdio",
            "command": "serena",
            "args": ["start-mcp-server"],
        }],
    })

    snapshot = load_runtime_mcp_snapshot(tmp_path)
    by_id = {server.server_id: server for server in snapshot.servers}

    http_server = by_id["exa"]
    assert http_server.proxy_base_url == "http://platform:8082"
    assert http_server.capability_token == "jwt-token"

    stdio_server = by_id["serena"]
    assert stdio_server.transport == "stdio"
    assert stdio_server.proxy_base_url == ""
    assert stdio_server.capability_token == ""
    assert stdio_server.command == "serena"


def test_runtime_mcp_stdio_only_with_proxy_keeps_stdio_proxy_free(tmp_path: Path):
    _write_runtime_config(tmp_path, {
        "snapshot_id": "sha256:def",
        "proxy": {
            "base_url": "http://platform:8082",
            "capability_token": "jwt-token",
        },
        "servers": [{
            "server_id": "serena",
            "name": "Serena",
            "url": "",
            "timeout_seconds": 60,
            "transport": "stdio",
            "command": "serena",
        }],
    })

    server = load_runtime_mcp_snapshot(tmp_path).servers[0]

    assert server.transport == "stdio"
    assert server.proxy_base_url == ""
    assert server.capability_token == ""
