"""Build and own a worker-lifetime MCP tool catalog."""

from __future__ import annotations

from typing import Any, Callable

from ga_worker.mcp_client import MCPHTTPClient, MCPServerConfig
from ga_worker.mcp_config import MCPRuntimeSnapshot


class MCPRuntimeError(RuntimeError):
    pass


def initialize_mcp_catalog(
    snapshot: MCPRuntimeSnapshot,
    *,
    client_factory: Callable[[MCPServerConfig], Any] = MCPHTTPClient,
) -> tuple[dict[str, dict[str, Any]], list[Any]]:
    catalog: dict[str, dict[str, Any]] = {}
    clients: list[Any] = []
    try:
        for server in snapshot.servers:
            client = client_factory(server)
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
