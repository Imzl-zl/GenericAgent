from __future__ import annotations

import json
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from typing import Any
from urllib.parse import urlsplit

import pytest
import requests

from ga_worker.mcp_client import MCPClientError, MCPHTTPClient, MCPServerConfig, MCPTool


class _MCPHandler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"
    requests: list[dict[str, Any]] = []
    initialize_session_id = "session-123"
    trailing_notification = False

    def log_message(self, format: str, *args: object) -> None:
        return

    def do_POST(self) -> None:
        length = int(self.headers.get("Content-Length", "0"))
        payload = json.loads(self.rfile.read(length) or b"{}")
        self.__class__.requests.append({
            "path": self.path,
            "payload": payload,
            "session_id": self.headers.get("Mcp-Session-Id", ""),
            "accept": self.headers.get("Accept", ""),
            "authorization": self.headers.get("Authorization", ""),
            "protocol_version": self.headers.get("MCP-Protocol-Version", ""),
        })
        method = payload.get("method")
        if method == "initialize":
            self._send_sse({
                "jsonrpc": "2.0",
                "id": payload["id"],
                "result": {
                    "protocolVersion": "2024-11-05",
                    "capabilities": {"tools": {}},
                    "serverInfo": {"name": "fake", "version": "1.0"},
                },
            }, session_id=self.__class__.initialize_session_id)
            return
        if method == "notifications/initialized":
            self.send_response(202)
            self.send_header("Content-Length", "0")
            self.end_headers()
            return
        if method == "tools/list":
            self._send_json({
                "jsonrpc": "2.0",
                "id": payload["id"],
                "result": {
                    "tools": [{
                        "name": "web_search",
                        "description": "Search the web",
                        "inputSchema": {
                            "type": "object",
                            "properties": {"query": {"type": "string"}},
                            "required": ["query"],
                        },
                    }],
                },
            })
            return
        if method == "tools/call":
            args = payload["params"]["arguments"]
            messages = [{
                "jsonrpc": "2.0",
                "id": payload["id"],
                "result": {
                    "content": [{"type": "text", "text": f"result:{args['query']}"}],
                    "isError": False,
                },
            }]
            if self.__class__.trailing_notification:
                messages.append({"jsonrpc": "2.0", "method": "notifications/tools/list_changed"})
            self._send_sse_messages(messages)
            return
        self._send_json({
            "jsonrpc": "2.0",
            "id": payload.get("id"),
            "error": {"code": -32601, "message": "unknown method"},
        })

    def _send_json(self, payload: dict[str, Any]) -> None:
        body = json.dumps(payload).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def _send_sse(self, payload: dict[str, Any], *, session_id: str = "") -> None:
        self._send_sse_messages([payload], session_id=session_id)

    def _send_sse_messages(self, payloads: list[dict[str, Any]], *, session_id: str = "") -> None:
        body = "".join(
            "event: message\ndata: " + json.dumps(payload) + "\n\n"
            for payload in payloads
        ).encode()
        self.send_response(200)
        self.send_header("Content-Type", "text/event-stream")
        if session_id:
            self.send_header("Mcp-Session-Id", session_id)
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)


def test_server_config_exposes_only_supported_runtime_fields():
    assert set(MCPServerConfig.__dataclass_fields__) == {
        "server_id", "name", "url", "timeout_seconds",
        "proxy_base_url", "capability_token",
    }


@pytest.fixture
def mcp_server():
    _MCPHandler.requests = []
    _MCPHandler.initialize_session_id = "session-123"
    _MCPHandler.trailing_notification = False
    server = ThreadingHTTPServer(("127.0.0.1", 0), _MCPHandler)
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    try:
        yield f"http://127.0.0.1:{server.server_port}/mcp"
    finally:
        server.shutdown()
        server.server_close()
        thread.join(timeout=2)


def test_initialize_list_and_call_support_json_and_sse(mcp_server: str):
    client = MCPHTTPClient(MCPServerConfig(
        server_id="exa",
        name="Exa",
        url=mcp_server,
        timeout_seconds=2,
    ))

    tools = client.initialize()

    assert [tool.name for tool in tools] == ["web_search"]
    assert tools[0].ga_name == "exa__web_search"
    assert tools[0].schema["function"]["parameters"]["required"] == ["query"]
    assert client.call_tool("web_search", {"query": "GenericAgent"}) == "result:GenericAgent"

    assert [entry["payload"]["method"] for entry in _MCPHandler.requests] == [
        "initialize", "notifications/initialized", "tools/list", "tools/call",
    ]
    assert _MCPHandler.requests[0]["session_id"] == ""
    assert all(entry["session_id"] == "session-123" for entry in _MCPHandler.requests[1:])
    assert all("text/event-stream" in entry["accept"] for entry in _MCPHandler.requests)
    assert all(entry["authorization"] == "" for entry in _MCPHandler.requests)
    assert _MCPHandler.requests[0]["protocol_version"] == ""
    assert all(entry["protocol_version"] == "2024-11-05" for entry in _MCPHandler.requests[1:])


def test_proxy_configured_dials_proxy_and_sends_capability(mcp_server: str):
    # 经 Platform MCP proxy 访问: 拨号地址改写为 /v1/worker/mcp/{server_id},
    # 并携带短期 capability(代理侧剥离 Authorization 后再转发上游)。
    origin = "{scheme}://{netloc}".format(
        scheme=urlsplit(mcp_server).scheme, netloc=urlsplit(mcp_server).netloc
    )
    client = MCPHTTPClient(MCPServerConfig(
        server_id="exa",
        name="Exa",
        url=mcp_server,
        timeout_seconds=2,
        proxy_base_url=origin,
        capability_token="cap-token-1",
    ))

    tools = client.initialize()

    assert [tool.name for tool in tools] == ["web_search"]
    assert all(entry["path"].startswith("/v1/worker/mcp/exa") for entry in _MCPHandler.requests)
    assert all(entry["authorization"] == "Bearer cap-token-1" for entry in _MCPHandler.requests)


def test_proxy_configured_without_token_is_rejected():
    config = MCPServerConfig(
        server_id="exa", name="Exa", url="https://mcp.exa.ai/mcp",
        proxy_base_url="http://platform:8082", capability_token="",
    )
    with pytest.raises(ValueError, match="capability_token"):
        config.validate()
    assert config.dial_url == "http://platform:8082/v1/worker/mcp/exa"
    assert config.auth_headers() == {}

def test_stateless_server_receives_protocol_version_and_sse_response_is_selected_by_id(mcp_server: str):
    _MCPHandler.initialize_session_id = ""
    _MCPHandler.trailing_notification = True
    client = MCPHTTPClient(MCPServerConfig(
        server_id="stateless", name="Stateless", url=mcp_server, timeout_seconds=2,
    ))

    client.initialize()
    result = client.call_tool("web_search", {"query": "GA"})

    assert result == "result:GA"
    assert all(
        entry["protocol_version"] == "2024-11-05"
        for entry in _MCPHandler.requests[1:]
    )


def test_first_call_tool_shares_one_deadline_with_initialization(monkeypatch: pytest.MonkeyPatch):
    client = MCPHTTPClient(MCPServerConfig(
        server_id="exa", name="Exa", url="https://example.com/mcp", timeout_seconds=2,
    ))
    deadlines: list[float | None] = []

    def initialize(*, deadline: float | None = None):
        deadlines.append(deadline)
        client._tools = {
            "search": MCPTool("search", "exa__search", "Search", {"type": "object"}),
        }
        client._initialized = True
        return client.tools

    def request(*args: Any, deadline: float | None = None, **kwargs: Any):
        deadlines.append(deadline)
        return {
            "jsonrpc": "2.0", "id": 1,
            "result": {"content": [{"type": "text", "text": "ok"}]},
        }

    monkeypatch.setattr(client, "initialize", initialize)
    monkeypatch.setattr(client, "_request", request)

    assert client.call_tool("search", {}) == "ok"
    assert len(deadlines) == 2
    assert deadlines[0] is not None
    assert deadlines[0] == deadlines[1]


def test_mcp_error_is_not_reported_as_success(mcp_server: str, monkeypatch: pytest.MonkeyPatch):
    client = MCPHTTPClient(MCPServerConfig(
        server_id="exa", name="Exa", url=mcp_server, timeout_seconds=2,
    ))
    client.initialize()

    monkeypatch.setattr(client, "_request", lambda *args, **kwargs: {
        "jsonrpc": "2.0", "id": 99,
        "result": {"content": [{"type": "text", "text": "failed"}], "isError": True},
    })

    with pytest.raises(MCPClientError, match="failed"):
        client.call_tool("web_search", {"query": "x"})


def test_tools_list_follows_pagination_cursor(mcp_server: str, monkeypatch: pytest.MonkeyPatch):
    client = MCPHTTPClient(MCPServerConfig(
        server_id="paged", name="Paged", url=mcp_server, timeout_seconds=2,
    ))
    monkeypatch.setattr(client, "_initialize_protocol", lambda **kwargs: None)
    params_seen: list[dict[str, Any]] = []

    def request(method: str, params: dict[str, Any], **kwargs: Any):
        params_seen.append(params)
        if len(params_seen) == 1:
            return {
                "jsonrpc": "2.0", "id": 1,
                "result": {
                    "tools": [{"name": "one", "inputSchema": {"type": "object"}}],
                    "nextCursor": "page-2",
                },
            }
        return {
            "jsonrpc": "2.0", "id": 2,
            "result": {"tools": [{"name": "two", "inputSchema": {"type": "object"}}]},
        }

    monkeypatch.setattr(client, "_request", request)
    tools = client.initialize()
    assert [tool.name for tool in tools] == ["one", "two"]
    assert params_seen == [{}, {"cursor": "page-2"}]


def test_tools_list_rejects_unbounded_unique_pagination(mcp_server: str, monkeypatch: pytest.MonkeyPatch):
    client = MCPHTTPClient(MCPServerConfig(
        server_id="paged", name="Paged", url=mcp_server, timeout_seconds=2,
    ))
    monkeypatch.setattr(client, "_initialize_protocol", lambda **kwargs: None)
    calls = 0

    def request(method: str, params: dict[str, Any], **kwargs: Any):
        nonlocal calls
        calls += 1
        if calls > 40:
            raise AssertionError("tools/list pagination was not bounded")
        return {
            "jsonrpc": "2.0", "id": calls,
            "result": {
                "tools": [{"name": f"tool_{calls}", "inputSchema": {"type": "object"}}],
                "nextCursor": f"page-{calls + 1}",
            },
        }

    monkeypatch.setattr(client, "_request", request)
    with pytest.raises(MCPClientError, match="too many tools/list pages"):
        client.initialize()


def test_request_enforces_total_response_deadline():
    body = json.dumps({"jsonrpc": "2.0", "id": 1, "result": {}}).encode()

    class SlowResponse:
        status_code = 200
        headers = {"Content-Type": "application/json"}

        def iter_content(self, chunk_size: int):
            time.sleep(0.02)
            yield body

        def close(self):
            return None

    class FakeSession:
        def post(self, *args: Any, **kwargs: Any):
            return SlowResponse()

        def close(self):
            return None

    client = MCPHTTPClient(
        MCPServerConfig("slow", "Slow", "https://slow.example/mcp", timeout_seconds=0.005),
        session=FakeSession(),
    )

    with pytest.raises(MCPClientError, match="deadline"):
        client._request("tools/list", {})


def test_expired_deadline_seals_client(monkeypatch: pytest.MonkeyPatch):
    client = MCPHTTPClient(MCPServerConfig(
        "expired", "Expired", "https://example.com/mcp", timeout_seconds=1,
    ))
    monkeypatch.setattr(client, "_operation_deadline", lambda: time.monotonic() - 1)

    with pytest.raises(MCPClientError, match="deadline"):
        client._request("tools/list", {})
    with pytest.raises(MCPClientError, match="closed"):
        client._request("tools/list", {})


def test_requests_timeout_seals_client():
    class TimeoutSession:
        def post(self, *args: Any, **kwargs: Any):
            raise requests.Timeout("read timed out")

        def close(self):
            return None

    client = MCPHTTPClient(
        MCPServerConfig("timeout", "Timeout", "https://example.com/mcp", timeout_seconds=1),
        session=TimeoutSession(),
    )

    with pytest.raises(MCPClientError, match="deadline"):
        client._request("tools/list", {})
    with pytest.raises(MCPClientError, match="closed"):
        client._request("tools/list", {})


def test_request_deadline_interrupts_slow_drip_response():
    body = json.dumps({"jsonrpc": "2.0", "id": 1, "result": {}}).encode()

    class DripHandler(BaseHTTPRequestHandler):
        protocol_version = "HTTP/1.1"

        def log_message(self, format: str, *args: object) -> None:
            return

        def do_POST(self) -> None:
            length = int(self.headers.get("Content-Length", "0"))
            self.rfile.read(length)
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            try:
                for byte in body:
                    self.wfile.write(bytes([byte]))
                    self.wfile.flush()
                    time.sleep(0.01)
            except (BrokenPipeError, ConnectionResetError):
                pass

    server = ThreadingHTTPServer(("127.0.0.1", 0), DripHandler)
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    client = MCPHTTPClient(MCPServerConfig(
        "drip", "Drip", f"http://127.0.0.1:{server.server_port}/mcp", timeout_seconds=0.05,
    ))
    started = time.monotonic()
    try:
        with pytest.raises(MCPClientError, match="deadline"):
            client._request("tools/list", {})
        assert time.monotonic() - started < 0.2
    finally:
        client.close()
        server.shutdown()
        server.server_close()
        thread.join(timeout=2)


def test_request_deadline_interrupts_slow_response_headers():
    body = json.dumps({"jsonrpc": "2.0", "id": 1, "result": {}}).encode()
    raw_headers = (
        b"HTTP/1.1 200 OK\r\n"
        b"Content-Type: application/json\r\n"
        + f"Content-Length: {len(body)}\r\n\r\n".encode()
    )

    class HeaderDripHandler(BaseHTTPRequestHandler):
        def log_message(self, format: str, *args: object) -> None:
            return

        def do_POST(self) -> None:
            length = int(self.headers.get("Content-Length", "0"))
            self.rfile.read(length)
            try:
                for byte in raw_headers:
                    self.wfile.write(bytes([byte]))
                    self.wfile.flush()
                    time.sleep(0.005)
                self.wfile.write(body)
                self.wfile.flush()
            except (BrokenPipeError, ConnectionResetError):
                pass

    server = ThreadingHTTPServer(("127.0.0.1", 0), HeaderDripHandler)
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    client = MCPHTTPClient(MCPServerConfig(
        "headers", "Headers", f"http://127.0.0.1:{server.server_port}/mcp", timeout_seconds=0.05,
    ))
    started = time.monotonic()
    try:
        with pytest.raises(MCPClientError, match="deadline"):
            client._request("tools/list", {})
        assert time.monotonic() - started < 0.2
        with pytest.raises(MCPClientError, match="closed"):
            client._request("tools/list", {})
    finally:
        client.close()
        server.shutdown()
        server.server_close()
        thread.join(timeout=2)


def test_tool_names_are_safely_namespaced_and_invalid_schema_is_rejected(
    mcp_server: str, monkeypatch: pytest.MonkeyPatch,
):
    client = MCPHTTPClient(MCPServerConfig(
        server_id="bad id", name="Bad", url=mcp_server, timeout_seconds=2,
    ))
    with pytest.raises(ValueError, match="server_id"):
        client.initialize()

    valid = MCPHTTPClient(MCPServerConfig(
        server_id="valid", name="Valid", url=mcp_server, timeout_seconds=2,
    ))
    monkeypatch.setattr(valid, "_initialize_protocol", lambda **kwargs: None)
    monkeypatch.setattr(valid, "_request", lambda *args, **kwargs: {
        "jsonrpc": "2.0", "id": 1,
        "result": {"tools": [{
            "name": "docs/search-v1-" + "x" * 80,
            "inputSchema": {"type": "object"},
        }]},
    })
    tools = valid.initialize()
    assert tools[0].name.startswith("docs/search-v1")
    assert tools[0].ga_name.startswith("valid__docs_search_v1_")
    assert len(tools[0].ga_name) <= 64

    invalid = MCPHTTPClient(MCPServerConfig(
        server_id="schema", name="Schema", url=mcp_server, timeout_seconds=2,
    ))
    monkeypatch.setattr(invalid, "_initialize_protocol", lambda **kwargs: None)
    monkeypatch.setattr(invalid, "_request", lambda *args, **kwargs: {
        "jsonrpc": "2.0", "id": 1,
        "result": {"tools": [{"name": "bad", "inputSchema": {"type": "array"}}]},
    })
    with pytest.raises(MCPClientError, match="inputSchema"):
        invalid.initialize()
