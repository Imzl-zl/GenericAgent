from __future__ import annotations

from dataclasses import dataclass

import pytest

from ga_worker.mcp_client import MCPServerConfig, MCPTool
from ga_worker.mcp_config import MCPRuntimeSnapshot
from ga_worker.mcp_runtime import MCPRuntimeError, close_mcp_clients, initialize_mcp_catalog


@dataclass
class FakeClient:
    config: MCPServerConfig
    closed: bool = False

    def initialize(self):
        return (
            MCPTool(
                name="search",
                ga_name=f"{self.config.server_id}__search",
                description="Search",
                input_schema={"type": "object", "properties": {}},
            ),
        )

    def call_tool(self, name, arguments):
        return "ok"

    def close(self):
        self.closed = True


def test_initialize_mcp_catalog_builds_schema_and_dispatch_bindings():
    snapshot = MCPRuntimeSnapshot(
        snapshot_id="sha256:1",
        servers=(
            MCPServerConfig("exa", "Exa", "https://mcp.exa.ai/mcp"),
            MCPServerConfig("docs", "Docs", "https://docs.example/mcp"),
        ),
    )

    catalog, clients = initialize_mcp_catalog(snapshot, client_factory=FakeClient)

    assert list(catalog) == ["exa__search", "docs__search"]
    assert catalog["exa__search"]["tool_name"] == "search"
    assert catalog["exa__search"]["schema"]["function"]["name"] == "exa__search"
    assert len(clients) == 2
    close_mcp_clients(clients)
    assert all(client.closed for client in clients)


def test_initialization_failure_closes_clients_already_created():
    created: list[FakeClient] = []

    class FailingClient(FakeClient):
        def initialize(self):
            if self.config.server_id == "bad":
                raise RuntimeError("boom")
            return super().initialize()

    def factory(config):
        client = FailingClient(config)
        created.append(client)
        return client

    snapshot = MCPRuntimeSnapshot(
        snapshot_id="sha256:2",
        servers=(
            MCPServerConfig("ok", "OK", "https://ok.example/mcp"),
            MCPServerConfig("bad", "Bad", "https://bad.example/mcp"),
        ),
    )

    with pytest.raises(MCPRuntimeError, match="bad"):
        initialize_mcp_catalog(snapshot, client_factory=factory)

    assert len(created) == 2
    assert all(client.closed for client in created)
