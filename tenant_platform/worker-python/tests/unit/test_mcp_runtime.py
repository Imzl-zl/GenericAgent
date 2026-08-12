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


def test_default_factory_dispatches_stdio_server(tmp_path):
    # 默认 client_factory 按 transport 分发: stdio 走 MCPStdioClient,
    # 沙箱内进程宿主完整走通 initialize → catalog。
    import sys

    from ga_worker.mcp_client import MCPStdioClient

    script = tmp_path / "fake_stdio_server.py"
    script.write_text(
        "import json, sys\n"
        "def send(m): sys.stdout.write(json.dumps(m) + '\\n'); sys.stdout.flush()\n"
        "for line in sys.stdin:\n"
        "    m = json.loads(line.strip())\n"
        "    if m.get('method') == 'initialize':\n"
        "        send({'jsonrpc':'2.0','id':m['id'],'result':{'protocolVersion':'2024-11-05','capabilities':{'tools':{}},'serverInfo':{'name':'fake','version':'1'}}})\n"
        "    elif m.get('method') == 'tools/list':\n"
        "        send({'jsonrpc':'2.0','id':m['id'],'result':{'tools':[{'name':'ping','inputSchema':{'type':'object'}}]}})\n"
    )
    snapshot = MCPRuntimeSnapshot(
        snapshot_id="sha256:3",
        servers=(
            MCPServerConfig(
                "serena", "Serena", "", timeout_seconds=5,
                transport="stdio", command=sys.executable, args=(str(script),),
            ),
        ),
    )

    catalog, clients = initialize_mcp_catalog(snapshot)

    assert isinstance(clients[0], MCPStdioClient)
    assert list(catalog) == ["serena__ping"]
    close_mcp_clients(clients)
    assert clients[0]._process is None or clients[0]._process.poll() is not None


def test_spawn_failure_is_wrapped_as_runtime_error():
    # stdio 子进程 spawn 失败(命令不存在)必须包装为 MCPRuntimeError,
    # 否则 session_lifecycle 的捕获列表接不住会直接崩会话启动。
    snapshot = MCPRuntimeSnapshot(
        snapshot_id="sha256:4",
        servers=(
            MCPServerConfig(
                "ghost", "Ghost", "", timeout_seconds=1,
                transport="stdio", command="definitely-not-a-real-command-xyz",
            ),
        ),
    )

    with pytest.raises(MCPRuntimeError, match="ghost"):
        initialize_mcp_catalog(snapshot)
