"""Build and own a worker-lifetime MCP tool catalog."""

from __future__ import annotations

from typing import Any, Callable

from ga_worker.mcp_client import MCPClientBase, MCPHTTPClient, MCPServerConfig, MCPStdioClient
from ga_worker.mcp_config import MCPRuntimeSnapshot


class MCPRuntimeError(RuntimeError):
    pass


def _default_client_factory(config: MCPServerConfig) -> MCPClientBase:
    """按 transport 选择客户端: http 走 Platform proxy, stdio 沙箱内进程宿主。"""
    if config.transport == "stdio":
        return MCPStdioClient(config)
    return MCPHTTPClient(config)


def initialize_mcp_catalog(
    snapshot: MCPRuntimeSnapshot,
    *,
    client_factory: Callable[[MCPServerConfig], Any] | None = None,
) -> tuple[dict[str, dict[str, Any]], list[Any]]:
    if client_factory is None:
        client_factory = _default_client_factory
    catalog: dict[str, dict[str, Any]] = {}
    clients: list[Any] = []
    try:
        for server in snapshot.servers:
            # MCPStdioClient 构造时即 spawn 子进程(可能失败), 必须与
            # initialize 一样包装为 MCPRuntimeError, 否则 session_lifecycle
            # 的 (MCPConfigError, MCPRuntimeError, ValueError) 捕获不到。
            try:
                client = client_factory(server)
            except Exception as exc:
                raise MCPRuntimeError(
                    f"MCP server {server.server_id} initialization failed: {exc}"
                ) from exc
            clients.append(client)
            try:
                tools = client.initialize()
            except Exception as exc:
                raise MCPRuntimeError(f"MCP server {server.server_id} initialization failed: {exc}") from exc
            for tool in tools:
                if tool.ga_name in catalog:
                    raise MCPRuntimeError(f"duplicate global MCP tool: {tool.ga_name}")
                catalog[tool.ga_name] = {
                    "client": client,
                    "tool_name": tool.name,
                    "schema": tool.schema,
                }
        return catalog, clients
    except Exception:
        close_mcp_clients(clients)
        raise


def close_mcp_clients(clients: list[Any] | tuple[Any, ...]) -> None:
    for client in clients:
        try:
            client.close()
        except Exception:
            pass
