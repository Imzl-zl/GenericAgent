"""Load administrator-managed global MCP servers from signed runtime config."""

from __future__ import annotations

import json
from dataclasses import dataclass
from pathlib import Path
from urllib.parse import urlparse

from ga_worker.credential_config import CredentialConfigError, load_runtime_document
from ga_worker.mcp_client import MCPServerConfig


class MCPConfigError(ValueError):
    pass


@dataclass(frozen=True)
class MCPRuntimeSnapshot:
    snapshot_id: str
    servers: tuple[MCPServerConfig, ...]
    # proxy 非空时, http server 一律经 Platform 受控 MCP proxy 访问
    # (key 平台侧持有 + 配额计量): server_id → URL 映射即白名单。
    # stdio server 是沙箱内进程宿主, 不携带 proxy 字段(proxy 仅对 http
    # transport 生效, 混布快照不会因 stdio 行携带 proxy 而校验失败)。
    proxy: "MCPRuntimeProxy | None" = None


@dataclass(frozen=True)
class MCPRuntimeProxy:
    base_url: str
    capability_token: str

    def validate(self) -> None:
        parsed = urlparse(self.base_url)
        if parsed.scheme not in ("http", "https") or not parsed.netloc:
            raise ValueError("proxy base_url must be an absolute http or https URL")
        if parsed.username or parsed.password or parsed.fragment:
            raise ValueError("proxy base_url must contain no credentials or fragment")
        if not self.capability_token.strip():
            raise ValueError("capability_token is required")


def load_runtime_mcp_snapshot(config_root: Path) -> MCPRuntimeSnapshot:
    root = Path(config_root)
    try:
        _, document = load_runtime_document(root)
    except (CredentialConfigError, OSError, json.JSONDecodeError) as exc:
        raise MCPConfigError(f"cannot load signed runtime MCP config: {exc}") from exc

    raw = document.get("_platform_mcp")
    if raw is None:
        return MCPRuntimeSnapshot(snapshot_id="disabled", servers=())
    if not isinstance(raw, dict):
        raise MCPConfigError("_platform_mcp must be an object")
    snapshot_id = raw.get("snapshot_id")
    servers = raw.get("servers")
    if not isinstance(snapshot_id, str) or not snapshot_id.strip():
        raise MCPConfigError("_platform_mcp.snapshot_id is required")
    if not isinstance(servers, list):
        raise MCPConfigError("_platform_mcp.servers must be an array")

    proxy = None
    raw_proxy = raw.get("proxy")
    if raw_proxy is not None:
        if not isinstance(raw_proxy, dict):
            raise MCPConfigError("_platform_mcp.proxy must be an object")
        unknown = set(raw_proxy) - {"base_url", "capability_token"}
        if unknown:
            raise MCPConfigError(f"_platform_mcp.proxy contains unknown fields: {sorted(unknown)}")
        proxy = MCPRuntimeProxy(
            base_url=raw_proxy.get("base_url", ""),
            capability_token=raw_proxy.get("capability_token", ""),
        )
        try:
            proxy.validate()
        except (TypeError, ValueError) as exc:
            raise MCPConfigError(f"invalid MCP proxy: {exc}") from exc

    parsed: list[MCPServerConfig] = []
    seen: set[str] = set()
    for index, item in enumerate(servers):
        if not isinstance(item, dict):
            raise MCPConfigError(f"MCP server at index {index} must be an object")
        unknown = set(item) - {
            "server_id", "name", "url", "timeout_seconds",
            "transport", "command", "args",
        }
        if unknown:
            raise MCPConfigError(f"MCP server at index {index} contains unknown fields: {sorted(unknown)}")
        raw_args = item.get("args")
        if raw_args is None:
            args: tuple[str, ...] = ()
        elif isinstance(raw_args, list) and all(isinstance(arg, str) for arg in raw_args):
            args = tuple(raw_args)
        else:
            raise MCPConfigError(f"MCP server at index {index}: args must be an array of strings")
        transport = item.get("transport", "http")
        if not isinstance(transport, str):
            raise MCPConfigError(f"MCP server at index {index}: transport must be a string")
        # proxy 只服务 http transport: stdio 为沙箱内进程宿主, 携带 proxy
        # 字段会触发 MCPServerConfig.validate() 的 stdio 拒绝分支。
        use_proxy = bool(proxy) and transport == "http"
        config = MCPServerConfig(
            server_id=item.get("server_id", ""),
            name=item.get("name", ""),
            url=item.get("url", ""),
            timeout_seconds=item.get("timeout_seconds", 30),
            proxy_base_url=proxy.base_url if use_proxy else "",
            capability_token=proxy.capability_token if use_proxy else "",
            transport=transport,
            command=item.get("command", ""),
            args=args,
        )
        try:
            config.validate()
        except (TypeError, ValueError) as exc:
            raise MCPConfigError(f"invalid MCP server at index {index}: {exc}") from exc
        if config.server_id in seen:
            raise MCPConfigError(f"duplicate MCP server_id: {config.server_id}")
        seen.add(config.server_id)
        parsed.append(config)
    return MCPRuntimeSnapshot(snapshot_id=snapshot_id, servers=tuple(parsed), proxy=proxy)
