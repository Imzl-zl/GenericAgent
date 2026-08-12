"""MCP clients for tenant Worker tools: Streamable HTTP (via Platform proxy)
and stdio (process hosted inside the Worker sandbox, mainstream mcp.json
compatible)."""

from __future__ import annotations

import hashlib
import json
import queue
import re
import subprocess
import threading
import time
from dataclasses import dataclass
from typing import Any
from urllib.parse import urlparse

import requests

MCP_PROTOCOL_VERSION = "2024-11-05"
DEFAULT_MAX_RESPONSE_BYTES = 256 * 1024
MAX_MCP_TOOL_PAGES = 32
MAX_MCP_TOOLS = 1024
MAX_MCP_CATALOG_BYTES = 1024 * 1024
_NAME_RE = re.compile(r"^[A-Za-z0-9_]{1,64}$")
MAX_MCP_STDIO_STDERR_LINES = 32


class MCPClientError(RuntimeError):
    pass


class MCPDeadlineExceeded(MCPClientError):
    pass


@dataclass(frozen=True)
class MCPServerConfig:
    server_id: str
    name: str
    url: str
    timeout_seconds: float = 30.0
    # proxy_base_url/capability_token 非空时, http 请求经 Platform 受控 MCP
    # proxy 转发(server_id → URL 映射即白名单): 拨号地址改写为
    # {proxy_base_url}/v1/worker/mcp/{server_id}, 并携带短期 capability。
    proxy_base_url: str = ""
    capability_token: str = ""
    # transport: "http"(Streamable HTTP) | "stdio"(Worker 沙箱内进程宿主)。
    # stdio 服务器用 command/args 拉起, 不需要 url/proxy/headers。
    transport: str = "http"
    command: str = ""
    args: tuple[str, ...] = ()

    def validate(self) -> None:
        if not _NAME_RE.fullmatch(self.server_id or ""):
            raise ValueError("server_id must contain only letters, digits, or underscores")
        if self.timeout_seconds <= 0:
            raise ValueError("timeout_seconds must be positive")
        if self.transport not in ("http", "stdio"):
            raise ValueError("transport must be 'http' or 'stdio'")
        if not isinstance(self.url, str):
            raise ValueError("url must be a string")
        if not isinstance(self.command, str):
            raise ValueError("command must be a string")
        if self.transport == "stdio":
            if self.url:
                raise ValueError("url must be empty for stdio transport")
            if not self.command.strip():
                raise ValueError("command is required for stdio transport")
            if self.proxy_base_url:
                raise ValueError("proxy_base_url is not supported for stdio transport")
            return
        if self.command or self.args:
            raise ValueError("command/args are only valid for stdio transport")
        parsed = urlparse(self.url)
        if parsed.scheme not in ("http", "https") or not parsed.netloc:
            raise ValueError("url must be an absolute http or https URL")
        if parsed.username or parsed.password or parsed.fragment:
            raise ValueError("url must contain no credentials or fragment")
        if self.proxy_base_url:
            parsed_proxy = urlparse(self.proxy_base_url)
            if parsed_proxy.scheme not in ("http", "https") or not parsed_proxy.netloc:
                raise ValueError("proxy_base_url must be an absolute http or https URL")
            if parsed_proxy.username or parsed_proxy.password or parsed_proxy.fragment:
                raise ValueError("proxy_base_url must contain no credentials or fragment")
        if self.proxy_base_url and not self.capability_token.strip():
            raise ValueError("capability_token is required when proxy_base_url is set")

    @property
    def dial_url(self) -> str:
        """实际拨号地址: 配置 proxy 时改写为 Platform MCP proxy 端点。"""
        if self.proxy_base_url:
            return "{}/v1/worker/mcp/{}".format(
                self.proxy_base_url.rstrip("/"), self.server_id
            )
        return self.url

    def auth_headers(self) -> dict[str, str]:
        if self.capability_token:
            return {"Authorization": f"Bearer {self.capability_token}"}
        return {}


@dataclass(frozen=True)
class MCPTool:
    name: str
    ga_name: str
    description: str
    input_schema: dict[str, Any]

    @property
    def schema(self) -> dict[str, Any]:
        return {
            "type": "function",
            "function": {
                "name": self.ga_name,
                "description": self.description,
                "parameters": self.input_schema,
            },
        }


class MCPClientBase:
    """Shared JSON-RPC 2.0 MCP client logic (protocol negotiation, catalog
    discovery with pagination, tool calls, deadlines).

    Subclasses implement `_request` (transport-specific) and `close`.
    """

    def __init__(self, config: MCPServerConfig):
        self.config = config
        self._protocol_version = MCP_PROTOCOL_VERSION
        self._next_id = 1
        self._lock = threading.Lock()
        self._tools: dict[str, MCPTool] = {}
        self._protocol_negotiated = False
        self._initialized = False
        self._closed = False

    @property
    def tools(self) -> tuple[MCPTool, ...]:
        return tuple(self._tools.values())

    def initialize(self, *, deadline: float | None = None) -> tuple[MCPTool, ...]:
        self.config.validate()
        with self._lock:
            if self._initialized:
                return self.tools
            active_deadline = deadline if deadline is not None else self._operation_deadline()
            self._initialize_protocol(deadline=active_deadline)
            discovered: dict[str, MCPTool] = {}
            ga_names: set[str] = set()
            cursor = ""
            seen_cursors: set[str] = set()
            page_count = 0
            catalog_bytes = 0
            tool_count = 0
            while True:
                page_count += 1
                if page_count > MAX_MCP_TOOL_PAGES:
                    raise MCPClientError(f"too many tools/list pages (max {MAX_MCP_TOOL_PAGES})")
                params = {"cursor": cursor} if cursor else {}
                response = self._request("tools/list", params, deadline=active_deadline)
                result = self._result(response, "tools/list")
                raw_tools = result.get("tools")
                if not isinstance(raw_tools, list):
                    raise MCPClientError("tools/list result must contain a tools array")
                tool_count += len(raw_tools)
                if tool_count > MAX_MCP_TOOLS:
                    raise MCPClientError(f"MCP catalog exceeds {MAX_MCP_TOOLS} tools")
                catalog_bytes += len(json.dumps(raw_tools, ensure_ascii=False).encode("utf-8"))
                if catalog_bytes > MAX_MCP_CATALOG_BYTES:
                    raise MCPClientError(f"MCP catalog exceeds {MAX_MCP_CATALOG_BYTES} bytes")
                for raw in raw_tools:
                    tool = self._parse_tool(raw)
                    if tool.name in discovered or tool.ga_name in ga_names:
                        raise MCPClientError(f"duplicate MCP tool name: {tool.name}")
                    discovered[tool.name] = tool
                    ga_names.add(tool.ga_name)
                next_cursor = result.get("nextCursor")
                if next_cursor in (None, ""):
                    break
                if not isinstance(next_cursor, str) or next_cursor in seen_cursors:
                    raise MCPClientError("tools/list returned an invalid or repeated nextCursor")
                seen_cursors.add(next_cursor)
                cursor = next_cursor
            self._tools = discovered
            self._initialized = True
            return self.tools

    def _initialize_protocol(self, *, deadline: float) -> None:
        response = self._request("initialize", {
            "protocolVersion": MCP_PROTOCOL_VERSION,
            "capabilities": {},
            "clientInfo": {"name": "genericagent-worker", "version": "1.0"},
        }, deadline=deadline)
        result = self._result(response, "initialize")
        version = result.get("protocolVersion")
        if not isinstance(version, str) or not version:
            raise MCPClientError("initialize result missing protocolVersion")
        self._protocol_version = version
        self._protocol_negotiated = True
        self._request("notifications/initialized", None, notification=True, deadline=deadline)

    def call_tool(self, tool_name: str, arguments: dict[str, Any]) -> str:
        deadline = self._operation_deadline()
        if not self._initialized:
            self.initialize(deadline=deadline)
        if tool_name not in self._tools:
            raise MCPClientError(f"unknown MCP tool: {tool_name}")
        if not isinstance(arguments, dict):
            raise MCPClientError("tool arguments must be an object")
        with self._lock:
            response = self._request("tools/call", {
                "name": tool_name,
                "arguments": arguments,
            }, deadline=deadline)
        result = self._result(response, f"tools/call {tool_name}")
        text = _content_to_text(result)
        if result.get("isError") is True:
            raise MCPClientError(text or f"MCP tool {tool_name} failed")
        return text

    def close(self) -> None:
        self._closed = True

    def _parse_tool(self, raw: Any) -> MCPTool:
        if not isinstance(raw, dict):
            raise MCPClientError("tool definition must be an object")
        name = raw.get("name")
        if not isinstance(name, str) or not name.strip() or len(name) > 256:
            raise MCPClientError(f"invalid MCP tool name: {name!r}")
        ga_name = _namespaced_tool_name(self.config.server_id, name)
        schema = raw.get("inputSchema", {"type": "object", "properties": {}})
        if not isinstance(schema, dict) or schema.get("type", "object") != "object":
            raise MCPClientError(f"MCP tool {name} inputSchema must describe an object")
        schema = dict(schema)
        schema.setdefault("type", "object")
        schema.setdefault("properties", {})
        description = raw.get("description")
        if not isinstance(description, str) or not description.strip():
            description = f"MCP tool {name} from {self.config.name or self.config.server_id}"
        return MCPTool(name=name, ga_name=ga_name, description=description, input_schema=schema)

    def _request(
        self,
        method: str,
        params: dict[str, Any] | None,
        *,
        notification: bool = False,
        deadline: float | None = None,
    ) -> dict[str, Any]:
        raise NotImplementedError

    def _operation_deadline(self) -> float:
        return time.monotonic() + self.config.timeout_seconds

    @staticmethod
    def _remaining_timeout(deadline: float, operation: str) -> float:
        remaining = deadline - time.monotonic()
        if remaining <= 0:
            raise MCPDeadlineExceeded(f"MCP {operation} deadline exceeded")
        return remaining

    @staticmethod
    def _result(response: dict[str, Any], operation: str) -> dict[str, Any]:
        error = response.get("error")
        if isinstance(error, dict):
            raise MCPClientError(f"MCP {operation} failed: {error.get('message') or error}")
        result = response.get("result")
        if not isinstance(result, dict):
            raise MCPClientError(f"MCP {operation} response missing result")
        return result


class MCPHTTPClient(MCPClientBase):
    """Streamable HTTP MCP client (MCP 2024-11-05), usually dialed through the
    Platform MCP proxy so credentials stay platform-side and quotas are
    enforced per call."""

    def __init__(self, config: MCPServerConfig, *, session: requests.Session | None = None):
        super().__init__(config)
        self._http = session or requests.Session()
        self._session_id = ""

    def close(self) -> None:
        super().close()
        self._http.close()

    def _request(
        self,
        method: str,
        params: dict[str, Any] | None,
        *,
        notification: bool = False,
        deadline: float | None = None,
    ) -> dict[str, Any]:
        if self._closed:
            raise MCPClientError("MCP client is closed")
        request_id: int | None = None
        payload: dict[str, Any] = {"jsonrpc": "2.0", "method": method}
        if not notification:
            request_id = self._next_id
            self._next_id += 1
            payload["id"] = request_id
        if params is not None:
            payload["params"] = params

        headers = {
            "Content-Type": "application/json",
            "Accept": "application/json, text/event-stream",
        }
        # 经 Platform MCP proxy 访问时携带短期 capability(不外泄给第三方 MCP
        # Server——平台代理侧会剥离 Authorization 再转发上游)。
        headers.update(self.config.auth_headers())
        if self._session_id:
            headers["Mcp-Session-Id"] = self._session_id
        if self._protocol_negotiated:
            headers["MCP-Protocol-Version"] = self._protocol_version
        active_deadline = deadline if deadline is not None else self._operation_deadline()
        try:
            self._remaining_timeout(active_deadline, method)
        except MCPDeadlineExceeded:
            self.close()
            raise
        outcomes: queue.Queue[tuple[dict[str, Any] | None, Exception | None]] = queue.Queue(maxsize=1)

        def _deliver(result=None, error=None):
            # 审查: 主线程可能已因 deadline 超时离开——maxsize=1 的 put 在满时
            # 永久阻塞会泄漏 daemon 线程; put_nowait 在满时立即丢弃, 线程退出。
            try:
                outcomes.put_nowait((result, error))
            except queue.Full:
                pass

        def perform() -> None:
            try:
                result = self._perform_http_request(
                    method=method,
                    payload=payload,
                    headers=headers,
                    request_id=request_id,
                    notification=notification,
                    deadline=active_deadline,
                )
                _deliver(result=result)
            except Exception as exc:
                _deliver(error=exc)

        request_thread = threading.Thread(
            target=perform,
            name=f"mcp-http-{self.config.server_id}",
            daemon=True,
        )
        request_thread.start()
        try:
            wait_timeout = self._remaining_timeout(active_deadline, method)
        except MCPDeadlineExceeded:
            self.close()
            raise
        try:
            result, request_error = outcomes.get(timeout=wait_timeout)
        except queue.Empty as exc:
            self.close()
            raise MCPDeadlineExceeded(f"MCP {method} deadline exceeded") from exc
        if request_error is not None:
            if isinstance(request_error, MCPDeadlineExceeded):
                self.close()
            raise request_error
        if result is None:
            raise MCPClientError(f"MCP {method} returned no result")
        return result

    def _perform_http_request(
        self,
        *,
        method: str,
        payload: dict[str, Any],
        headers: dict[str, str],
        request_id: int | None,
        notification: bool,
        deadline: float,
    ) -> dict[str, Any]:
        request_timeout = self._remaining_timeout(deadline, method)
        try:
            response = self._http.post(
                self.config.dial_url,
                headers=headers,
                json=payload,
                timeout=request_timeout,
                stream=True,
                allow_redirects=False,
            )
        except requests.Timeout as exc:
            raise MCPDeadlineExceeded(f"MCP {method} deadline exceeded") from exc
        except requests.RequestException as exc:
            raise MCPClientError(f"MCP {method} request failed: {exc}") from exc
        try:
            if response.status_code >= 400:
                raise MCPClientError(f"MCP {method} returned HTTP {response.status_code}")
            session_id = response.headers.get("Mcp-Session-Id", "").strip()
            if session_id:
                self._session_id = session_id
            body = _read_bounded(response, DEFAULT_MAX_RESPONSE_BYTES, deadline=deadline)
            if notification and not body.strip():
                return {}
            message = _decode_response(
                response.headers.get("Content-Type", ""), body, request_id=request_id,
            )
            if request_id is not None and message.get("id") != request_id:
                raise MCPClientError(
                    f"MCP {method} response id mismatch: expected {request_id}, got {message.get('id')!r}"
                )
            return message
        finally:
            response.close()


class MCPStdioClient(MCPClientBase):
    """stdio transport MCP client (MCP 2024-11-05): spawns the configured
    command inside the Worker sandbox and speaks newline-delimited JSON-RPC
    over stdin/stdout — the mainstream mcp.json format (e.g. serena).

    The process inherits the Worker environment (PATH, cwd) so bare command
    names like `serena` resolve like they do for local clients. stdio calls
    do not traverse the Platform MCP proxy, so they are not metered per call;
    quota still gates catalog inclusion at snapshot issuance time.
    """

    def __init__(self, config: MCPServerConfig, *, cwd: str | None = None):
        super().__init__(config)
        self._cwd = cwd
        self._inbox: queue.Queue[Any] = queue.Queue()
        self._stderr_lines: list[str] = []
        self._process: subprocess.Popen | None = None
        self._reader_thread: threading.Thread | None = None
        self._stderr_thread: threading.Thread | None = None
        self._spawn()

    def _spawn(self) -> None:
        command = self.config.command
        args = list(self.config.args)
        try:
            self._process = subprocess.Popen(
                [command, *args],
                stdin=subprocess.PIPE,
                stdout=subprocess.PIPE,
                stderr=subprocess.PIPE,
                text=True,
                encoding="utf-8",
                errors="replace",
                bufsize=1,
                cwd=self._cwd,
            )
        except OSError as exc:
            raise MCPClientError(
                f"MCP stdio spawn failed for {self.config.server_id}: {exc}"
            ) from exc
        self._reader_thread = threading.Thread(
            target=self._read_loop,
            name=f"mcp-stdio-{self.config.server_id}",
            daemon=True,
        )
        self._reader_thread.start()
        self._stderr_thread = threading.Thread(
            target=self._stderr_loop,
            name=f"mcp-stderr-{self.config.server_id}",
            daemon=True,
        )
        self._stderr_thread.start()

    def _read_loop(self) -> None:
        # 逐行读取 stdout, 解析为 JSON-RPC 消息放入 inbox; EOF 放 None 哨兵。
        assert self._process is not None
        try:
            for line in self._process.stdout:
                line = line.strip()
                if not line:
                    continue
                try:
                    message = json.loads(line)
                except json.JSONDecodeError as exc:
                    self._inbox.put(MCPClientError(f"MCP stdio server sent invalid JSON: {exc}"))
                    return
                if not isinstance(message, dict):
                    self._inbox.put(MCPClientError("MCP stdio server message must be a JSON object"))
                    return
                self._inbox.put(message)
            self._inbox.put(None)
        except Exception as exc:  # pragma: no cover - defensive
            self._inbox.put(exc)

    def _stderr_loop(self) -> None:
        # 排空 stderr 防止管道写满阻塞子进程; 只保留最近 N 行供诊断。
        assert self._process is not None
        try:
            for line in self._process.stderr:
                stripped = line.rstrip("\n")
                if len(self._stderr_lines) >= MAX_MCP_STDIO_STDERR_LINES:
                    self._stderr_lines.pop(0)
                self._stderr_lines.append(stripped)
        except Exception:  # pragma: no cover - defensive
            pass

    def _stderr_diagnostics(self, message: str) -> str:
        if self._stderr_lines:
            return message + ": " + " | ".join(self._stderr_lines[-5:])
        return message

    def _write_message(self, payload: dict[str, Any], deadline: float) -> None:
        self._remaining_timeout(deadline, "stdio write")
        assert self._process is not None
        try:
            assert self._process.stdin is not None
            self._process.stdin.write(json.dumps(payload, ensure_ascii=False) + "\n")
            self._process.stdin.flush()
        except BrokenPipeError as exc:
            raise MCPClientError(
                self._stderr_diagnostics("MCP stdio process closed stdin")
            ) from exc
        except OSError as exc:
            raise MCPClientError(f"MCP stdio write failed: {exc}") from exc

    def _read_message(self, deadline: float) -> dict[str, Any]:
        wait = self._remaining_timeout(deadline, "stdio read")
        try:
            item = self._inbox.get(timeout=wait)
        except queue.Empty as exc:
            raise MCPDeadlineExceeded("MCP stdio read deadline exceeded") from exc
        if item is None:
            raise MCPClientError(self._stderr_diagnostics("MCP stdio process exited unexpectedly"))
        if isinstance(item, Exception):
            raise item
        return item

    def _request(
        self,
        method: str,
        params: dict[str, Any] | None,
        *,
        notification: bool = False,
        deadline: float | None = None,
    ) -> dict[str, Any]:
        if self._closed:
            raise MCPClientError("MCP client is closed")
        request_id: int | None = None
        payload: dict[str, Any] = {"jsonrpc": "2.0", "method": method}
        if not notification:
            request_id = self._next_id
            self._next_id += 1
            payload["id"] = request_id
        if params is not None:
            payload["params"] = params
        active_deadline = deadline if deadline is not None else self._operation_deadline()
        try:
            try:
                self._write_message(payload, active_deadline)
            except (MCPDeadlineExceeded, MCPClientError):
                self.close()
                raise
            if notification:
                return {}
            while True:
                message = self._read_message(active_deadline)
                message_id = message.get("id")
                if "method" in message and message_id is not None:
                    # server → client 请求(sampling/roots 等): 客户端 initialize
                    # 声明无 capabilities, 返回 method-not-found 后继续等响应。
                    self._write_message({
                        "jsonrpc": "2.0",
                        "id": message_id,
                        "error": {"code": -32601, "message": "method not supported"},
                    }, active_deadline)
                    continue
                if message_id == request_id:
                    return message
                if "method" in message:
                    continue  # server 通知(log 等), 跳过
                raise MCPClientError(
                    f"MCP {method} response id mismatch: expected {request_id}, got {message_id!r}"
                )
        except (MCPDeadlineExceeded, MCPClientError):
            # 协议失步/超时/进程退出后客户端不可复用: 封口并回收子进程。
            self.close()
            raise

    def close(self) -> None:
        if self._closed:
            return
        super().close()
        process = self._process
        self._process = None
        if process is None:
            return
        if process.stdin is not None:
            try:
                process.stdin.close()
            except OSError:
                pass
        if process.poll() is None:
            process.terminate()
            try:
                process.wait(timeout=5)
            except subprocess.TimeoutExpired:
                process.kill()
                try:
                    process.wait(timeout=5)
                except subprocess.TimeoutExpired:
                    pass
        for thread in (self._reader_thread, self._stderr_thread):
            if thread is not None and thread.is_alive():
                thread.join(timeout=2)


def _namespaced_tool_name(server_id: str, remote_name: str) -> str:
    prefix = f"{server_id}__"
    cleaned = re.sub(r"[^A-Za-z0-9_]", "_", remote_name).strip("_") or "tool"
    direct = prefix + cleaned
    if direct == prefix + remote_name and len(direct) <= 64 and _NAME_RE.fullmatch(direct):
        return direct
    suffix = "_" + hashlib.sha256(remote_name.encode("utf-8")).hexdigest()[:8]
    available = 64 - len(prefix) - len(suffix)
    if available < 1:
        raise MCPClientError(f"MCP server id leaves no room for tool name: {server_id!r}")
    return prefix + cleaned[:available].rstrip("_") + suffix


def _read_bounded(response: requests.Response, limit: int, *, deadline: float) -> bytes:
    body = bytearray()
    try:
        # A one-byte read prevents urllib3 from buffering toward a larger chunk
        # while a peer drips data quickly enough to reset the socket idle timeout.
        for chunk in response.iter_content(chunk_size=1):
            if time.monotonic() >= deadline:
                raise MCPDeadlineExceeded("MCP response deadline exceeded")
            if not chunk:
                continue
            if len(body) + len(chunk) > limit:
                raise MCPClientError(f"MCP response exceeds {limit} bytes")
            body.extend(chunk)
    except requests.Timeout as exc:
        raise MCPDeadlineExceeded("MCP response deadline exceeded") from exc
    except requests.RequestException as exc:
        raise MCPClientError(f"cannot read MCP response: {exc}") from exc
    return bytes(body)


def _decode_response(
    content_type: str,
    body: bytes,
    *,
    request_id: int | None = None,
) -> dict[str, Any]:
    try:
        text = body.decode("utf-8")
    except UnicodeDecodeError as exc:
        raise MCPClientError("MCP response is not UTF-8") from exc
    try:
        if "text/event-stream" in content_type.lower():
            messages: list[dict[str, Any]] = []
            data_lines: list[str] = []
            for line in text.splitlines() + [""]:
                if line == "":
                    if data_lines:
                        messages.append(json.loads("\n".join(data_lines)))
                        data_lines = []
                    continue
                if line.startswith("data:"):
                    data_lines.append(line[5:].lstrip())
            if not messages:
                raise MCPClientError("MCP SSE response contains no message event")
            if request_id is not None:
                for message in messages:
                    if message.get("id") == request_id:
                        return message
                raise MCPClientError(f"MCP SSE response contains no message for id {request_id}")
            return messages[-1]
        decoded = json.loads(text)
        if not isinstance(decoded, dict):
            raise MCPClientError("MCP response must be a JSON object")
        return decoded
    except json.JSONDecodeError as exc:
        raise MCPClientError(f"invalid MCP JSON response: {exc}") from exc


def _content_to_text(result: dict[str, Any]) -> str:
    parts: list[str] = []
    content = result.get("content")
    if isinstance(content, list):
        for item in content:
            if isinstance(item, dict) and item.get("type") == "text" and isinstance(item.get("text"), str):
                parts.append(item["text"])
            elif item is not None:
                parts.append(json.dumps(item, ensure_ascii=False))
    structured = result.get("structuredContent")
    if structured is not None:
        parts.append(json.dumps(structured, ensure_ascii=False))
    return "\n".join(part for part in parts if part).strip()
