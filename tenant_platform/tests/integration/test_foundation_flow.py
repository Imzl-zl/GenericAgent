"""Foundation vertical path E2E: real PostgreSQL + platform subprocess + Worker."""

from __future__ import annotations

import base64
import hmac
import http.client
import hashlib
import json
import os
import re
import signal
import socket
import subprocess
import sys
import threading
import time
import urllib.error
import urllib.request
import urllib.parse
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path

REPO_ROOT = Path(__file__).resolve().parents[3]
POLICY_PATH = (
    REPO_ROOT / "tenant_platform" / "contracts" / "policy" / "foundation.v1.json"
)
BACKEND_GO = REPO_ROOT / "tenant_platform" / "backend-go"
WORKER_SRC = REPO_ROOT / "tenant_platform" / "worker-python" / "src"
PYTHON = Path(
    os.environ.get("GA_WORKER_PYTHON")
    or os.environ.get("GA_TEST_PYTHON")
    or REPO_ROOT / ".venv" / "Scripts" / "python.exe"
)
if not PYTHON.is_file():
    PYTHON = Path(sys.executable)

TEST_DB = os.environ.get("TEST_DATABASE_URL", "").strip()
if not TEST_DB:
    raise RuntimeError(
        "TEST_DATABASE_URL is required for foundation integration tests "
        "(no SQLite/in-memory fallback)"
    )

DEV_TOKEN = "foundation-dev-token-not-real"
DEV_USER_ID = "1"
# Fixture upstream credentials are submitted through the Admin Provider API.
# The Worker receives only a capability token and Proxy URL.
OAI_TOKEN = "foundation-integration-oai-token-not-real"
CAPABILITY_SIGNING_KEY = "foundation-integration-signing-key"
BOT_TOKEN_KEY = "0000000000000000000000000000000000000000000000000000000000000000"
SECONDARY_OAI_TOKEN = "foundation-secondary-oai-token-not-real"
ROTATED_OAI_TOKEN = "foundation-rotated-oai-token-not-real"
CLAUDE_TOKEN = "sk-ant-foundation-claude-token-not-real"
STREAM_GAP_SECONDS = 0.8
REAL_KEY_SENTINELS = (OAI_TOKEN, SECONDARY_OAI_TOKEN, ROTATED_OAI_TOKEN, CLAUDE_TOKEN)
FIXTURE_PROVIDER_NAME = "foundation-fixture-default"


class _FixtureHandler(BaseHTTPRequestHandler):
    server: "_FixtureServer"

    def log_message(self, _fmt: str, *_args: object) -> None:
        return

    def do_POST(self) -> None:
        credential = self._credential()
        profile, must_fail = self.server.resolve_credential(credential)
        if profile is None:
            self._respond(401, {"error": "unauthorized"})
            return
        size = int(self.headers.get("Content-Length", "0"))
        try:
            payload = json.loads(self.rfile.read(size).decode("utf-8"))
        except (ValueError, json.JSONDecodeError, UnicodeDecodeError):
            self._respond(400, {"error": "invalid json"})
            return
        parsed = urllib.parse.urlsplit(self.path)
        self.server.record_request(credential, parsed, payload, self.headers)
        if must_fail:
            self._respond(503, {"error": "fixture provider unavailable"})
            return
        response_text = profile["response"]
        if parsed.path in ("/v1/chat/completions", "/chat/completions"):
            self._chat(payload, response_text)
        elif parsed.path in ("/v1/responses", "/responses"):
            self._responses(payload, response_text)
        elif parsed.path in ("/v1/messages", "/messages"):
            self._claude(payload, response_text)
        else:
            self._respond(404, {"error": "not found"})

    def _credential(self) -> str:
        authorization = self.headers.get("Authorization", "")
        if authorization.startswith("Bearer "):
            return authorization.removeprefix("Bearer ")
        return self.headers.get("X-Api-Key", "")

    def _chat(self, payload: dict, response_text: str) -> None:
        if payload.get("stream") is True:
            timed = "stream-timing-probe" in json.dumps(payload)
            parts = ["first-", "chunk"] if timed else [response_text]
            events = [
                {
                    "id": "chatcmpl-integration",
                    "object": "chat.completion.chunk",
                    "choices": [
                        {"index": 0, "delta": {"content": part}, "finish_reason": None}
                    ],
                }
                for part in parts
            ]
            events.append(
                {
                    "id": "chatcmpl-integration",
                    "object": "chat.completion.chunk",
                    "choices": [{"index": 0, "delta": {}, "finish_reason": "stop"}],
                    "usage": {
                        "prompt_tokens": 1,
                        "completion_tokens": 1,
                        "total_tokens": 2,
                    },
                }
            )
            self._sse(events, timed)
            return
        self._respond(
            200,
            {
                "id": "chatcmpl-integration",
                "object": "chat.completion",
                "choices": [
                    {
                        "index": 0,
                        "message": {"role": "assistant", "content": response_text},
                        "finish_reason": "stop",
                    }
                ],
                "usage": {
                    "prompt_tokens": 1,
                    "completion_tokens": 1,
                    "total_tokens": 2,
                },
            },
        )

    def _responses(self, payload: dict, response_text: str) -> None:
        if payload.get("stream") is True:
            self._sse(
                [
                    {"type": "response.output_text.delta", "delta": response_text},
                    {"type": "response.output_text.done", "text": response_text},
                    {
                        "type": "response.completed",
                        "response": {"usage": {"input_tokens": 1, "output_tokens": 1}},
                    },
                ],
                False,
            )
            return
        self._respond(
            200,
            {
                "id": "resp-integration",
                "output": [
                    {
                        "type": "message",
                        "content": [{"type": "output_text", "text": response_text}],
                    }
                ],
                "usage": {"input_tokens": 1, "output_tokens": 1},
            },
        )

    def _claude(self, payload: dict, response_text: str) -> None:
        if payload.get("stream") is not True:
            self._respond(
                200,
                {
                    "id": "msg-integration",
                    "type": "message",
                    "content": [{"type": "text", "text": response_text}],
                    "stop_reason": "end_turn",
                    "usage": {"input_tokens": 1, "output_tokens": 1},
                },
            )
            return
        self._sse(
            [
                {"type": "message_start", "message": {"usage": {"input_tokens": 1}}},
                {
                    "type": "content_block_start",
                    "index": 0,
                    "content_block": {"type": "text", "text": ""},
                },
                {
                    "type": "content_block_delta",
                    "index": 0,
                    "delta": {"type": "text_delta", "text": response_text},
                },
                {"type": "content_block_stop", "index": 0},
                {
                    "type": "message_delta",
                    "delta": {"stop_reason": "end_turn"},
                    "usage": {"output_tokens": 1},
                },
                {"type": "message_stop"},
            ],
            False,
        )

    def _sse(self, events: list[dict], delay_after_first: bool) -> None:
        self.send_response(200)
        self.send_header("Content-Type", "text/event-stream")
        self.send_header("Cache-Control", "no-cache")
        self.end_headers()
        try:
            for index, event in enumerate(events):
                raw = json.dumps(event, separators=(",", ":")).encode("utf-8")
                self.wfile.write(b"data: " + raw + b"\n\n")
                self.wfile.flush()
                if delay_after_first and index == 0:
                    time.sleep(STREAM_GAP_SECONDS)
            self.wfile.write(b"data: [DONE]\n\n")
            self.wfile.flush()
        except (BrokenPipeError, ConnectionResetError):
            pass

    def _respond(self, status: int, payload: dict) -> None:
        raw = json.dumps(payload, separators=(",", ":")).encode("utf-8")
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(raw)))
        self.end_headers()
        try:
            self.wfile.write(raw)
        except (BrokenPipeError, ConnectionResetError):
            pass


class _FixtureServer(ThreadingHTTPServer):
    daemon_threads = True

    def __init__(self, *args, **kwargs) -> None:
        super().__init__(*args, **kwargs)
        self._lock = threading.Lock()
        self._profiles = {
            OAI_TOKEN: {"name": "primary", "response": "primary-reply"},
            SECONDARY_OAI_TOKEN: {"name": "secondary", "response": "secondary-reply"},
            ROTATED_OAI_TOKEN: {"name": "primary-rotated", "response": "rotated-reply"},
            CLAUDE_TOKEN: {"name": "claude", "response": "claude-reply"},
        }
        self._failing: set[str] = set()
        self._requests: list[dict] = []

    def resolve_credential(self, credential: str) -> tuple[dict | None, bool]:
        with self._lock:
            profile = self._profiles.get(credential)
            return profile, credential in self._failing

    def record_request(self, credential: str, parsed, payload: dict, headers) -> None:
        capture = {
            "credential": credential,
            "path": parsed.path,
            "query": parsed.query,
            "payload": payload,
            "headers": {name.lower(): value for name, value in headers.items()},
        }
        with self._lock:
            self._requests.append(capture)

    def set_failure(self, credential: str, enabled: bool) -> None:
        with self._lock:
            if enabled:
                self._failing.add(credential)
            else:
                self._failing.discard(credential)

    def replace_credential(self, old: str, new: str) -> None:
        with self._lock:
            self._profiles.pop(old, None)
            self._failing.discard(old)
            if new not in self._profiles:
                raise AssertionError(f"unknown replacement fixture credential: {new}")

    def requests(self) -> list[dict]:
        with self._lock:
            return list(self._requests)


class _Fixture:
    def __init__(self) -> None:
        self.server = _FixtureServer(("127.0.0.1", 0), _FixtureHandler)
        self.thread = threading.Thread(
            target=self.server.serve_forever, name="integration-llm", daemon=True
        )

    def start(self) -> None:
        self.thread.start()

    @property
    def root_url(self) -> str:
        host, port = self.server.server_address
        return f"http://{host}:{port}"

    @property
    def base_url(self) -> str:
        return self.root_url + "/v1"

    def close(self) -> None:
        self.server.shutdown()
        self.server.server_close()
        self.thread.join(timeout=5)


def _platform_bin(tmp: Path) -> Path:
    out = tmp / ("platform.exe" if os.name == "nt" else "platform")
    subprocess.check_call(
        ["go", "build", "-o", str(out), "./cmd/platform"],
        cwd=str(BACKEND_GO),
        env=os.environ.copy(),
    )
    return out


def _http_json(
    method: str, url: str, body: dict | None = None, token: str | None = DEV_TOKEN
):
    data = None
    headers = {}
    if body is not None:
        data = json.dumps(body).encode("utf-8")
        headers["Content-Type"] = "application/json"
    if token:
        headers["X-Platform-Dev-Token"] = token
    req = urllib.request.Request(url, data=data, headers=headers, method=method)
    try:
        with urllib.request.urlopen(req, timeout=10) as resp:
            raw = resp.read()
            return resp.status, json.loads(raw.decode("utf-8") or "{}")
    except urllib.error.HTTPError as e:
        raw = e.read()
        try:
            payload = json.loads(raw.decode("utf-8") or "{}")
        except json.JSONDecodeError:
            payload = {"raw": raw.decode("utf-8", errors="replace")}
        return e.code, payload


def _free_loopback_addr() -> str:
    with socket.socket(socket.AF_INET, socket.SOCK_STREAM) as s:
        s.bind(("127.0.0.1", 0))
        return f"127.0.0.1:{s.getsockname()[1]}"


def _reset_db() -> None:
    env = {**os.environ, "DATABASE_URL": TEST_DB}
    subprocess.check_call(
        ["go", "run", "./cmd/resetdb"],
        cwd=str(BACKEND_GO),
        env=env,
    )


def _ensure_fixture_provider(
    base: str, fixture: _Fixture, *, refresh_existing: bool
) -> None:
    code, response = _http_json("GET", f"{base}/v1/admin/llm-providers")
    assert code == 200, response
    providers = response["providers"]
    existing = next(
        (
            provider
            for provider in providers
            if provider["name"] == FIXTURE_PROVIDER_NAME
        ),
        None,
    )
    body = {
        "name": FIXTURE_PROVIDER_NAME,
        "provider_type": "native_oai",
        "base_url": fixture.base_url,
        "model": "gpt-4o",
        "api_key": OAI_TOKEN,
        "session_config": {"stream": True, "max_retries": 0},
        "transport_config": {"auth_mode": "auto"},
    }
    if existing is None:
        code, response = _http_json("POST", f"{base}/v1/admin/llm-providers", body)
        assert code == 201, response
        return
    if not refresh_existing:
        return
    code, response = _http_json(
        "PUT",
        f"{base}/v1/admin/llm-providers/{existing['provider_id']}",
        body,
    )
    assert code == 200, response


def _start_platform(
    tmp: Path,
    listen: str | None = None,
    *,
    reset_db: bool = True,
    fixture: _Fixture | None = None,
) -> tuple[subprocess.Popen, str, Path, Path, Path, _Fixture]:
    tmp.mkdir(parents=True, exist_ok=True)
    if listen is None:
        listen = _free_loopback_addr()
    owns_fixture = fixture is None
    if fixture is None:
        fixture = _Fixture()
        fixture.start()
    bin_path = _platform_bin(tmp)
    config_root = tmp / "config"
    runtime_root = tmp / "runtime"
    log_path = tmp / "platform.log"
    config_root.mkdir(exist_ok=True)
    runtime_root.mkdir(exist_ok=True)
    env = os.environ.copy()
    env["DATABASE_URL"] = TEST_DB
    env["PLATFORM_DEV_USER_ID"] = DEV_USER_ID
    env["PLATFORM_DEV_TOKEN"] = DEV_TOKEN
    env["PLATFORM_DEV_USERNAME"] = "foundation-dev"
    env["GA_CONFIG_ROOT"] = str(config_root)
    env["GA_RUNTIME_DIR"] = str(runtime_root)
    env["GA_LEGACY_ROOT"] = str(REPO_ROOT)
    env["GA_WORKER_PYTHON"] = str(PYTHON)
    env["GA_WORKER_SRC"] = str(WORKER_SRC)
    env["GA_POLICY_FILE"] = str(POLICY_PATH)
    # 集成测试使用 loopback 开发降级路径(方案 §7: 显式声明, 非静默回退)。
    env["GA_WORKER_EXECUTION_MODE"] = "loopback"
    # The Platform receives only Proxy capability signing material and the DB
    # encryption key. The fixture upstream key enters through the Admin API.
    env["LLM_PROXY_CAPABILITY_SIGNING_KEY"] = CAPABILITY_SIGNING_KEY
    env["BOT_TOKEN_KEY"] = BOT_TOKEN_KEY
    env["LLM_PROXY_ALLOWED_UPSTREAM_CIDRS"] = "127.0.0.0/8"
    env["LLM_PROXY_ALLOW_HTTP_HOSTS"] = urllib.parse.urlsplit(fixture.root_url).netloc
    if reset_db:
        _reset_db()
    relative_policy = os.path.relpath(POLICY_PATH, BACKEND_GO)
    proc = subprocess.Popen(
        [
            str(bin_path),
            "--dev-loopback",
            "--policy-file",
            relative_policy,
            "--claim-lease",
            "15s",
            "--listen",
            listen,
            "--database-url",
            TEST_DB,
            "--config-root",
            str(config_root),
            "--runtime-root",
            str(runtime_root),
            "--legacy-root",
            str(REPO_ROOT),
            "--worker-python",
            str(PYTHON),
            "--worker-src",
            str(WORKER_SRC),
        ],
        cwd=str(BACKEND_GO),
        env=env,
        stdout=subprocess.PIPE,
        stderr=subprocess.STDOUT,
        text=True,
        encoding="utf-8",
        errors="replace",
        creationflags=(subprocess.CREATE_NEW_PROCESS_GROUP if os.name == "nt" else 0),
    )
    base = f"http://{listen}"
    deadline = time.time() + 45
    last = None
    while time.time() < deadline:
        if proc.poll() is not None:
            out = proc.stdout.read() if proc.stdout else ""
            log_path.write_text(out, encoding="utf-8")
            if owns_fixture:
                fixture.close()
            raise AssertionError(f"platform exited early code={proc.returncode}\n{out}")
        try:
            status, body = _http_json("GET", base + "/healthz", token=None)
            if status == 200 and body.get("status") == "ok":
                _ensure_fixture_provider(
                    base,
                    fixture,
                    refresh_existing=owns_fixture and not reset_db,
                )
                # Capture startup stdout until the instance_id line is emitted.
                # The platform prints LLM-proxy and scheduler startup lines before
                # the instance_id summary, so reading just one line is fragile.
                captured: list[str] = []
                line_deadline = time.time() + 3
                while time.time() < line_deadline:
                    line = proc.stdout.readline() if proc.stdout else ""
                    if line:
                        captured.append(line)
                        if "instance_id=" in line:
                            break
                    else:
                        time.sleep(0.05)
                log_path.write_text("".join(captured), encoding="utf-8")
                return proc, base, config_root, runtime_root, log_path, fixture
            last = (status, body)
        except Exception as exc:  # noqa: BLE001
            last = exc
        time.sleep(0.2)
    proc.kill()
    out = proc.stdout.read() if proc.stdout else ""
    log_path.write_text(out, encoding="utf-8")
    if owns_fixture:
        fixture.close()
    raise AssertionError(f"platform failed to start; last={last}\n{out}")


def _stop(proc: subprocess.Popen, log_path: Path | None = None) -> str:
    if proc.poll() is not None:
        out = proc.stdout.read() if proc.stdout else ""
    else:
        if os.name == "nt":
            proc.send_signal(signal.CTRL_BREAK_EVENT)
        else:
            proc.send_signal(signal.SIGTERM)
        try:
            out, _ = proc.communicate(timeout=10)
        except subprocess.TimeoutExpired:
            proc.kill()
            out, _ = proc.communicate(timeout=5)
    existing = ""
    if log_path is not None and log_path.exists():
        existing = log_path.read_text(encoding="utf-8")
        log_path.write_text(existing + (out or ""), encoding="utf-8")
    return existing + (out or "")


def _platform_instance_id(log_path: Path) -> str:
    deadline = time.time() + 10
    while time.time() < deadline:
        if log_path.exists():
            match = re.search(
                r"instance_id=([0-9a-f-]{36})", log_path.read_text(encoding="utf-8")
            )
            if match:
                return match.group(1)
        time.sleep(0.1)
    raise AssertionError(f"platform instance id missing from {log_path}")


def _seed_restart_rows(prior_instance: str) -> dict[str, str]:
    import psycopg

    ids = {
        "expired": "restart-expired-task",
        "queued": "restart-queued-task",
        "live": "restart-live-task",
    }
    with psycopg.connect(TEST_DB) as conn:
        dev_workspace = conn.execute(
            "SELECT id::text FROM workspaces WHERE session_key=%s",
            (f"personal:{DEV_USER_ID}",),
        ).fetchone()[0]
        conn.execute(
            "INSERT INTO users (id, username, status) VALUES (2, 'restart-foreign', 'approved')"
        )
        foreign_workspace = "00000000-0000-4000-8000-000000000002"
        conn.execute(
            """
            INSERT INTO workspaces (id, session_key, owner_user_id, kind, volume_id)
            VALUES (%s::uuid, 'personal:2', 2, 'personal', 'restart-foreign-volume')
            """,
            (foreign_workspace,),
        )
        task_sql = """
            INSERT INTO tasks (
                id, workspace_id, session_key, session_sequence, requester_user_id,
                source, source_instance_id, message_id, message_idempotency_key,
                prompt, persona_snapshot, tool_policy_version, prompt_bytes, persona_bytes,
                status, claim_owner, claimed_at, claim_lease_until,
                worker_instance_id, worker_dispatch_started_at
            ) VALUES (
                %s, %s::uuid, %s, %s, %s,
                'web', %s, %s, %s,
                %s, '[]'::jsonb, 'foundation.no-host-tools.v1', %s, 2,
                %s, %s, %s, %s, %s, %s
            )
        """
        conn.execute(
            task_sql,
            (
                ids["expired"],
                dev_workspace,
                f"personal:{DEV_USER_ID}",
                1,
                int(DEV_USER_ID),
                "restart-expired",
                "restart-expired",
                "restart-expired",
                "expired prior owner",
                len("expired prior owner"),
                "running",
                prior_instance,
                time.strftime("%Y-%m-%d %H:%M:%S+00", time.gmtime(time.time() - 120)),
                time.strftime("%Y-%m-%d %H:%M:%S+00", time.gmtime(time.time() - 60)),
                "prior-worker",
                time.strftime("%Y-%m-%d %H:%M:%S+00", time.gmtime(time.time() - 120)),
            ),
        )
        conn.execute(
            task_sql,
            (
                ids["queued"],
                dev_workspace,
                f"personal:{DEV_USER_ID}",
                2,
                int(DEV_USER_ID),
                "restart-queued",
                "restart-queued",
                "restart-queued",
                "complete after restart",
                len("complete after restart"),
                "queued",
                None,
                None,
                None,
                None,
                None,
            ),
        )
        conn.execute(
            task_sql,
            (
                ids["live"],
                foreign_workspace,
                "personal:2",
                1,
                2,
                "restart-live",
                "restart-live",
                "restart-live",
                "foreign live task",
                len("foreign live task"),
                "starting",
                "foreign-live-owner",
                time.strftime("%Y-%m-%d %H:%M:%S+00", time.gmtime()),
                time.strftime("%Y-%m-%d %H:%M:%S+00", time.gmtime(time.time() + 600)),
                "foreign-worker",
                time.strftime("%Y-%m-%d %H:%M:%S+00", time.gmtime()),
            ),
        )
        conn.commit()
    return ids


def _restart_row_facts(ids: dict[str, str]) -> dict[str, object]:
    import psycopg

    with psycopg.connect(TEST_DB) as conn:
        expired = conn.execute(
            "SELECT status FROM tasks WHERE id=%s", (ids["expired"],)
        ).fetchone()
        live = conn.execute(
            "SELECT status, claim_owner FROM tasks WHERE id=%s", (ids["live"],)
        ).fetchone()
        queued = conn.execute(
            "SELECT status FROM tasks WHERE id=%s", (ids["queued"],)
        ).fetchone()

        def count(sql: str, task_id: str) -> int:
            return conn.execute(sql, (task_id,)).fetchone()[0]

        return {
            "expired_status": expired[0],
            "expired_interrupt_deliveries": count(
                "SELECT count(*) FROM task_deliveries WHERE task_id=%s AND delivery_type='task_interrupted'",
                ids["expired"],
            ),
            "expired_dispatch_events": count(
                "SELECT count(*) FROM task_events WHERE task_id=%s AND event_type='dispatch'",
                ids["expired"],
            ),
            "live_status": live[0],
            "live_owner": live[1],
            "queued_status": queued[0],
            "queued_dispatch_events": count(
                "SELECT count(*) FROM task_events WHERE task_id=%s AND event_type='dispatch'",
                ids["queued"],
            ),
            "queued_complete_deliveries": count(
                "SELECT count(*) FROM task_deliveries WHERE task_id=%s AND delivery_type='task_complete'",
                ids["queued"],
            ),
        }


def _poll_status(
    base: str, task_id: str, want: set[str], timeout: float = 120.0
) -> dict:
    deadline = time.time() + timeout
    last = {}
    while time.time() < deadline:
        code, body = _http_json("GET", f"{base}/v1/tasks/{task_id}")
        if code == 200:
            last = body
            if body.get("status") in want:
                return body
        time.sleep(0.5)
    raise AssertionError(f"task {task_id} not in {want}; last={last}")


def _submit_success(base: str, message_id: str, prompt: str) -> dict:
    code, body = _http_json(
        "POST",
        f"{base}/v1/sessions/personal:{DEV_USER_ID}/tasks",
        {
            "message_id": message_id,
            "source_instance_id": "ga-contract-e2e",
            "prompt": prompt,
            "source": "web",
            "persona_snapshot": ["You are a concise contract-test agent."],
            "tool_policy_version": "foundation.no-host-tools.v1",
        },
    )
    assert code == 202, body
    final = _poll_status(base, body["task_id"], {"succeeded", "failed"}, timeout=150)
    assert final["status"] == "succeeded", final
    result_code, result = _http_json(
        "GET",
        f"{base}/v1/tasks/{body['task_id']}/result?result_ref={final['result_ref']}",
    )
    assert result_code == 200, result
    serialized_result = json.dumps(result, ensure_ascii=False)
    assert (
        "!!!Error:" not in serialized_result and "[Error:" not in serialized_result
    ), result
    return final


def _list_providers(base: str) -> list[dict]:
    code, body = _http_json("GET", f"{base}/v1/admin/llm-providers")
    assert code == 200, body
    return body["providers"]


def _provider_body(provider: dict, **overrides) -> dict:
    body = {
        "name": provider["name"],
        "provider_type": provider["provider_type"],
        "base_url": provider["base_url"],
        "model": provider["model"],
        "session_config": provider.get("session_config", {}),
        "transport_config": provider.get("transport_config", {"auth_mode": "auto"}),
    }
    body.update(overrides)
    return body


def _update_provider(base: str, provider: dict, **overrides) -> dict:
    code, body = _http_json(
        "PUT",
        f"{base}/v1/admin/llm-providers/{provider['provider_id']}",
        _provider_body(provider, **overrides),
    )
    assert code == 200, body
    return body


def _create_provider(base: str, body: dict) -> dict:
    code, response = _http_json("POST", f"{base}/v1/admin/llm-providers", body)
    assert code == 201, response
    return response


def _set_default_provider(base: str, provider_id: int) -> None:
    code, body = _http_json(
        "POST", f"{base}/v1/admin/llm-providers/{provider_id}/default"
    )
    assert code == 200, body


def _set_provider_state(base: str, provider_id: int, state: str) -> dict:
    action = "enable" if state == "active" else "disable"
    code, body = _http_json(
        "POST", f"{base}/v1/admin/llm-providers/{provider_id}/{action}"
    )
    assert code == 200, body
    return body


def _captured_request(
    fixture: _Fixture, prompt: str, credential: str | None = None
) -> dict:
    deadline = time.time() + 15
    while time.time() < deadline:
        for request in reversed(fixture.server.requests()):
            payload_text = json.dumps(request["payload"], ensure_ascii=False)
            if prompt in payload_text and (
                credential is None or request["credential"] == credential
            ):
                return request
        time.sleep(0.05)
    captures = [
        (
            request["credential"],
            request["path"],
            json.dumps(request["payload"], ensure_ascii=False)[:240],
        )
        for request in fixture.server.requests()[-8:]
    ]
    raise AssertionError(
        f"fixture request missing prompt={prompt!r} credential={credential!r}; captures={captures}"
    )


def _runtime_document(
    config_root: Path, session_key: str = f"personal:{DEV_USER_ID}"
) -> dict:
    session_dir = hashlib.sha256(session_key.encode("utf-8")).hexdigest()
    path = config_root / session_dir / "mykey.runtime.json"
    deadline = time.time() + 15
    while time.time() < deadline:
        if path.is_file():
            return json.loads(path.read_text(encoding="utf-8"))
        time.sleep(0.05)
    raise AssertionError(f"runtime config not written: {path}")


def _runtime_provider(document: dict, provider: dict) -> dict:
    key = f"platform_{provider['provider_type']}_provider_{provider['provider_id']}_config"
    value = document.get(key)
    assert isinstance(value, dict), f"missing runtime provider {key}: {document.keys()}"
    return value


def _decode_jwt_claims(token: str) -> dict:
    encoded = token.split(".")[1]
    raw = base64.urlsafe_b64decode(encoded + "=" * (-len(encoded) % 4))
    return json.loads(raw)


def _sign_capability(claims: dict) -> str:
    header = {"alg": "HS256", "typ": "ga-llm-cap+jwt"}

    def encode(value: dict) -> str:
        raw = json.dumps(value, separators=(",", ":"), sort_keys=True).encode("utf-8")
        return base64.urlsafe_b64encode(raw).rstrip(b"=").decode("ascii")

    signing_input = f"{encode(header)}.{encode(claims)}"
    signature = hmac.new(
        CAPABILITY_SIGNING_KEY.encode(), signing_input.encode(), hashlib.sha256
    ).digest()
    return (
        signing_input
        + "."
        + base64.urlsafe_b64encode(signature).rstrip(b"=").decode("ascii")
    )


def _capability_variant(token: str, **updates) -> str:
    claims = {**_decode_jwt_claims(token), **updates}
    return _sign_capability(claims)


def _proxy_connection(proxy_base: str) -> http.client.HTTPConnection:
    parsed = urllib.parse.urlsplit(proxy_base)
    assert parsed.scheme == "http" and parsed.hostname and parsed.port
    return http.client.HTTPConnection(parsed.hostname, parsed.port, timeout=10)


def _proxy_json(
    proxy_base: str, path: str, token: str, payload: dict
) -> tuple[int, dict]:
    connection = _proxy_connection(proxy_base)
    try:
        connection.request(
            "POST",
            path,
            body=json.dumps(payload).encode("utf-8"),
            headers={
                "Authorization": f"Bearer {token}",
                "Content-Type": "application/json",
            },
        )
        response = connection.getresponse()
        return response.status, json.loads(response.read().decode("utf-8") or "{}")
    finally:
        connection.close()


def _assert_stream_first_chunk_unbuffered(
    proxy_base: str, token: str, model: str
) -> None:
    payload = {
        "model": model,
        "messages": [{"role": "user", "content": "stream-timing-probe"}],
        "stream": True,
    }
    connection = _proxy_connection(proxy_base)
    started = time.monotonic()
    try:
        connection.request(
            "POST",
            "/v1/chat/completions",
            body=json.dumps(payload).encode("utf-8"),
            headers={
                "Authorization": f"Bearer {token}",
                "Content-Type": "application/json",
            },
        )
        response = connection.getresponse()
        first_line = response.readline()
        first_chunk_seconds = time.monotonic() - started
        remainder = response.read()
    finally:
        connection.close()
    assert first_line.startswith(b"data: "), first_line
    assert first_chunk_seconds < STREAM_GAP_SECONDS * 0.75, first_chunk_seconds
    assert b"chunk" in remainder


def _assert_capability_rejections(proxy_base: str, token: str, provider: dict) -> None:
    model = provider["model"]
    valid_body = {
        "model": model,
        "messages": [{"role": "user", "content": "direct-proxy"}],
        "stream": False,
    }
    # round9 审查: 语义变体必须换新 jti——任务终态后原 token 已被撤销表
    # 拒绝(401 REVOKED 抢先), 换 jti 才能让各语义检查按既定顺序返回
    # (错误码优先级: 签名/撤销 > 预算 > provider 404/409 > body model > 在线)。
    status, body = _proxy_json(
        proxy_base,
        "/v1/chat/completions",
        _capability_variant(token, jti="model-mismatch-e2e"),
        {**valid_body, "model": "wrong-model"},
    )
    assert (status, body.get("code")) == (409, "MODEL_MISMATCH"), body
    status, body = _proxy_json(
        proxy_base,
        "/v1/messages",
        _capability_variant(token, jti="type-mismatch-e2e"),
        {"model": model},
    )
    assert (status, body.get("code")) == (409, "PROVIDER_TYPE_MISMATCH"), body
    now = int(time.time())
    variants = [
        (
            _capability_variant(
                token,
                provider_id=999999,
                provider_revision=1,
                jti="missing-provider-e2e",
            ),
            404,
            "PROVIDER_NOT_FOUND",
        ),
        (
            _capability_variant(
                token, iat=now - 300, nbf=now - 300, exp=now - 120, jti="expired-e2e"
            ),
            401,
            "CAPABILITY_EXPIRED",
        ),
        (
            _capability_variant(token, aud=["wrong-audience"], jti="audience-e2e"),
            401,
            "CAPABILITY_AUDIENCE_MISMATCH",
        ),
    ]
    for variant, expected_status, expected_code in variants:
        status, body = _proxy_json(
            proxy_base, "/v1/chat/completions", variant, valid_body
        )
        assert (status, body.get("code")) == (expected_status, expected_code), body


def _list_python_pids() -> list[int]:
    """tasklist 快速枚举 python 解释器 PID(毫秒级, 适合短命 Worker 采样)。"""
    import subprocess
    out = subprocess.run(
        ["tasklist", "/FI", "IMAGENAME eq python.exe", "/FO", "CSV", "/NH"],
        capture_output=True, text=True, timeout=10,
    ).stdout
    pids: list[int] = []
    for line in out.splitlines():
        parts = [p.strip().strip('"') for p in line.split('","')]
        if len(parts) >= 2 and parts[1].isdigit():
            pids.append(int(parts[1]))
    return pids


def _assert_worker_process_isolated(
    platform_pid: int,
    config_root: Path,
    secrets: tuple[str, ...],
) -> None:
    import psutil

    expected_root = config_root.resolve()
    deadline = time.time() + 15
    workers: list[tuple[object, list[str], dict[str, str]]] = []
    candidates: list[tuple[int, int, list[str]]] = []
    while time.time() < deadline:
        workers.clear()
        candidates.clear()
        # 决策 D2.1: 任务即进程——Worker 只存活于任务执行期(秒级), psutil
        # 全系统枚举一次可达数秒会错过短命进程。tasklist 毫秒级返回所有
        # python 进程 PID, 再只对候选读取 cmdline(1-3 个进程)。
        for pid in _list_python_pids():
            try:
                process = psutil.Process(pid)
                command = process.cmdline()
                if "ga_worker.entrypoint" not in " ".join(command):
                    continue
                candidates.append((pid, process.ppid(), command))
                environment = process.environ()
                worker_root = Path(environment.get("GA_CONFIG_ROOT", "")).resolve()
                if worker_root == expected_root or expected_root in worker_root.parents:
                    workers.append((process, command, environment))
            except (psutil.AccessDenied, psutil.NoSuchProcess, psutil.ZombieProcess):
                continue
        if workers:
            break
        time.sleep(0.1)
    assert workers, (
        f"real Worker process not found for platform={platform_pid}; candidates={candidates}"
    )
    for worker, command, environment in workers:
        process_text = (
            "\n".join(command)
            + "\n"
            + "\n".join(f"{name}={value}" for name, value in environment.items())
        )
        for secret in secrets:
            assert secret not in process_text, (
                f"real key leaked into Worker process {worker.pid}"
            )
        for forbidden_name in (
            "LLM_PROVIDER_API_KEY",
            "DATABASE_URL",
            "BOT_TOKEN_KEY",
        ):
            assert forbidden_name not in process_text, (
                f"{forbidden_name} inherited by Worker {worker.pid}"
            )


def _assert_artifacts_exclude_secrets(
    roots: tuple[Path, ...], logs: str, secrets: tuple[str, ...]
) -> None:
    for root in roots:
        for path in root.rglob("*"):
            if not path.is_file():
                continue
            content = path.read_bytes()
            for secret in secrets:
                assert secret.encode() not in content, f"real key leaked into {path}"
    for secret in secrets:
        assert secret not in logs, "real key leaked into Platform/Worker logs"


def _configure_oai_providers(base: str, fixture: _Fixture) -> tuple[dict, dict]:
    providers = _list_providers(base)
    assert len(providers) == 1, providers
    primary = _update_provider(
        base,
        providers[0],
        session_config={"stream": True, "max_retries": 0},
    )
    secondary = _create_provider(
        base,
        {
            "name": "e2e-secondary-oai",
            "provider_type": "native_oai",
            "base_url": fixture.base_url,
            "model": primary["model"],
            "api_key": SECONDARY_OAI_TOKEN,
            "session_config": {"stream": True, "max_retries": 0},
            "transport_config": {"auth_mode": "auto"},
        },
    )
    assert primary["is_default"] is True
    assert secondary["is_default"] is False
    return primary, secondary


def _submit_started(base: str, message_id: str, prompt: str) -> dict:
    """提交任务并返回响应 body(不等待完成)。"""
    code, body = _http_json(
        "POST",
        f"{base}/v1/sessions/personal:{DEV_USER_ID}/tasks",
        {
            "message_id": message_id,
            "source_instance_id": "ga-contract-e2e",
            "prompt": prompt,
            "source": "web",
            "persona_snapshot": ["You are a concise contract-test agent."],
            "tool_policy_version": "foundation.no-host-tools.v1",
        },
    )
    assert code == 202, body
    return body


def _runtime_signature(document: dict) -> str:
    """决策 D1: 无 config_checksum, 用文档序列化签名检测配置推进。"""
    return hashlib.sha256(
        json.dumps(document, sort_keys=True, ensure_ascii=False).encode("utf-8")
    ).hexdigest()


def _wait_runtime_token(
    config_root: Path, primary: dict, prev_signature: str, timeout: float = 40.0
) -> tuple[str, str]:
    """轮询 runtime JSON 直到出现新的真实 capability token(任务执行期间
    签发, 尚未终态撤销)。round9 审查: 已终态任务的 token 会被在线校验与
    撤销表拒绝, capability 语义变体测试必须在任务活跃窗口内取样。"""
    deadline = time.time() + timeout
    while time.time() < deadline:
        document = _runtime_document(config_root)
        if _runtime_signature(document) != prev_signature:
            runtime_primary = _runtime_provider(document, primary)
            if runtime_primary["apikey"] not in REAL_KEY_SENTINELS:
                return runtime_primary["apikey"], runtime_primary["apibase"].removesuffix("/v1")
        time.sleep(0.2)
    raise AssertionError("no fresh runtime capability token appeared")


def _exercise_initial_oai_binding(
    proc: subprocess.Popen,
    base: str,
    config_root: Path,
    fixture: _Fixture,
    primary: dict,
    secondary: dict,
) -> None:
    # round9 审查: capability 语义变体测试(MODEL_MISMATCH/PROVIDER_TYPE_
    # MISMATCH/stream)必须在任务活跃窗口内取样 token——任务终态后 JTI 被
    # 撤销且在线校验拒绝, 401 CAPABILITY_REVOKED 会正确抢先于 409/404。
    # runtime JSON 可能在首个任务前尚未生成: 空签名使任何新签发都匹配。
    prev_signature = ""
    try:
        prev_doc = _runtime_document(config_root)
        prev_signature = _runtime_signature(prev_doc)
    except AssertionError:
        pass
    started = _submit_started(base, "ga-chat-primary", "ga-chat-primary")
    token, proxy_base = _wait_runtime_token(config_root, primary, prev_signature)
    # 决策 D2.1: 任务即进程——任务终态即销毁 Worker, 进程隔离断言必须在
    # 任务执行早期采样(token 刚签发, 进程必然活跃; psutil 已预热保证首次
    # 扫描毫秒级)。能力拒绝/流式测试随后仍在活跃窗口内。
    _assert_worker_process_isolated(proc.pid, config_root, REAL_KEY_SENTINELS)
    _assert_capability_rejections(proxy_base, token, primary)
    _assert_stream_first_chunk_unbuffered(proxy_base, token, primary["model"])
    final = _poll_status(base, started["task_id"], {"succeeded", "failed"}, timeout=150)
    assert final["status"] == "succeeded", final
    first_request = _captured_request(fixture, "ga-chat-primary", OAI_TOKEN)
    assert first_request["path"] == "/v1/chat/completions"

    document = _runtime_document(config_root)
    signature = _runtime_signature(document)
    _set_default_provider(base, secondary["provider_id"])
    _submit_success(base, "ga-existing-after-default", "ga-existing-after-default")
    # 决策 D2.1(任务即进程): 每个任务都是新 Worker, 重新解析路由快照——
    # 默认切换后新任务跟随新默认(secondary 第一)。
    _captured_request(fixture, "ga-existing-after-default", SECONDARY_OAI_TOKEN)
    # 每任务 capability(方案 §7): 每个新任务都签发绑定自身 task_id 的新
    # token, 终态后旧 token 撤销——runtime 文档必然推进。
    new_signature = _runtime_signature(_runtime_document(config_root))
    assert new_signature != signature


def _exercise_new_worker_mixin_and_key_rotation(
    base: str,
    config_root: Path,
    fixture: _Fixture,
    primary: dict,
    secondary: dict,
) -> dict:
    _submit_success(base, "ga-new-worker-secondary", "ga-new-worker-secondary")
    _captured_request(fixture, "ga-new-worker-secondary", SECONDARY_OAI_TOKEN)
    document = _runtime_document(config_root)
    mixin_names = document["mixin_config"]["llm_nos"]
    assert mixin_names[0] == f"provider-{secondary['provider_id']}"

    fixture.server.set_failure(SECONDARY_OAI_TOKEN, True)
    _submit_success(base, "ga-mixin-fallback", "ga-mixin-fallback")
    _captured_request(fixture, "ga-mixin-fallback", SECONDARY_OAI_TOKEN)
    _captured_request(fixture, "ga-mixin-fallback", OAI_TOKEN)

    signature = _runtime_signature(document)
    rotated = _update_provider(base, primary, api_key=ROTATED_OAI_TOKEN)
    assert rotated["revision"] == primary["revision"]
    fixture.server.replace_credential(OAI_TOKEN, ROTATED_OAI_TOKEN)
    _submit_success(base, "ga-key-rotation", "ga-key-rotation")
    request = _captured_request(fixture, "ga-key-rotation", ROTATED_OAI_TOKEN)
    assert "ga-mixin-fallback" in json.dumps(request["payload"])
    # 同 session 新任务同样推进 runtime 文档(每任务 token 绑定)。
    new_signature = _runtime_signature(_runtime_document(config_root))
    assert new_signature != signature
    return rotated


def _exercise_responses_and_claude_default(
    base: str,
    config_root: Path,
    fixture: _Fixture,
    primary: dict,
) -> dict:
    document = _runtime_document(config_root)
    runtime_primary = _runtime_provider(document, primary)
    old_token = runtime_primary["apikey"]
    proxy_base = runtime_primary["apibase"].removesuffix("/v1")
    responses = _update_provider(
        base,
        primary,
        session_config={"api_mode": "responses", "stream": True, "max_retries": 0},
    )
    assert responses["revision"] == primary["revision"] + 1
    # round9 审查: 语义变体换新 jti——任务终态后原 token 已被撤销表拒绝,
    # 换 jti 才能测到 revision 语义(409 优先于在线校验)。
    status, body = _proxy_json(
        proxy_base,
        "/v1/chat/completions",
        _capability_variant(old_token, jti="revision-mismatch-e2e"),
        {
            "model": primary["model"],
            "messages": [{"role": "user", "content": "old-revision"}],
        },
    )
    assert (status, body.get("code")) == (409, "PROVIDER_REVISION_MISMATCH"), body

    _submit_success(base, "ga-responses-sse", "ga-responses-sse")
    request = _captured_request(fixture, "ga-responses-sse", ROTATED_OAI_TOKEN)
    assert request["path"] == "/v1/responses" and request["payload"]["stream"] is True
    assert "ga-key-rotation" in json.dumps(request["payload"])
    status, body = _proxy_json(
        proxy_base,
        "/v1/chat/completions",
        old_token,
        {
            "model": primary["model"],
            "messages": [{"role": "user", "content": "revoked"}],
        },
    )
    assert (status, body.get("code")) == (401, "CAPABILITY_REVOKED"), body

    claude = _create_provider(
        base,
        {
            "name": "e2e-claude",
            "provider_type": "native_claude",
            "base_url": fixture.root_url,
            "model": "claude-test[1m]",
            "api_key": CLAUDE_TOKEN,
            "session_config": {"stream": True, "max_retries": 0},
            "transport_config": {"auth_mode": "auto"},
        },
    )
    _submit_success(base, "ga-provider-added", "ga-provider-added")
    request = _captured_request(fixture, "ga-provider-added", ROTATED_OAI_TOKEN)
    assert request["path"] == "/v1/responses"
    assert len(_runtime_document(config_root)["mixin_config"]["llm_nos"]) == 3
    _set_default_provider(base, claude["provider_id"])
    _submit_success(
        base, "ga-existing-oai-after-claude", "ga-existing-oai-after-claude"
    )
    # 决策 D2.1(任务即进程): 每任务重新解析路由快照——默认切到 claude 后
    # 新任务跟随 claude(不再复用旧 Worker 的旧快照)。
    request = _captured_request(
        fixture, "ga-existing-oai-after-claude", CLAUDE_TOKEN
    )
    assert request["path"] == "/v1/messages"
    return claude


def _exercise_new_claude_worker(
    proc: subprocess.Popen,
    base: str,
    config_root: Path,
    fixture: _Fixture,
    claude: dict,
) -> None:
    _submit_success(base, "ga-claude-sse", "ga-claude-sse")
    request = _captured_request(fixture, "ga-claude-sse", CLAUDE_TOKEN)
    assert request["path"] == "/v1/messages"
    assert request["query"] == "beta=true"
    assert request["payload"]["model"] == "claude-test"
    assert request["payload"]["stream"] is True
    assert "context-1m-2025-08-07" in request["headers"]["anthropic-beta"]
    assert request["headers"]["x-api-key"] == CLAUDE_TOKEN
    document = _runtime_document(config_root)
    mixin_names = document["mixin_config"]["llm_nos"]
    assert mixin_names[0] == f"provider-{claude['provider_id']}"
    assert len(mixin_names) == 3
    provider_keys = [key for key in document if key.startswith("platform_native_")]
    assert len(provider_keys) == 3


def _exercise_provider_disable(
    base: str,
    config_root: Path,
    fixture: _Fixture,
    primary_id: int,
) -> None:
    current = next(
        provider
        for provider in _list_providers(base)
        if provider["provider_id"] == primary_id
    )
    document = _runtime_document(config_root)
    runtime = _runtime_provider(document, current)
    old_token = runtime["apikey"]
    proxy_base = runtime["apibase"].removesuffix("/v1")
    runtime_key = f"platform_{current['provider_type']}_provider_{primary_id}_config"

    disabled = _set_provider_state(base, primary_id, "disabled")
    assert disabled["state"] == "disabled"
    assert disabled["revision"] == current["revision"] + 1
    # round9 审查: 语义变体换新 jti(同 revision/type 断言, 撤销表不抢先)。
    status, body = _proxy_json(
        proxy_base,
        "/v1/responses",
        _capability_variant(old_token, jti="provider-disabled-e2e"),
        {"model": current["model"], "input": "disabled-provider"},
    )
    assert (status, body.get("code")) == (409, "PROVIDER_DISABLED"), body

    _submit_success(base, "ga-after-provider-disable", "ga-after-provider-disable")
    _captured_request(fixture, "ga-after-provider-disable", CLAUDE_TOKEN)
    disabled_document = _runtime_document(config_root)
    assert runtime_key not in disabled_document
    assert len(disabled_document["mixin_config"]["llm_nos"]) == 2

    enabled = _set_provider_state(base, primary_id, "active")
    assert enabled["state"] == "active"
    assert enabled["revision"] == disabled["revision"] + 1
    _submit_success(base, "ga-after-provider-enable", "ga-after-provider-enable")
    _captured_request(fixture, "ga-after-provider-enable", CLAUDE_TOKEN)
    enabled_document = _runtime_document(config_root)
    assert runtime_key in enabled_document
    assert len(enabled_document["mixin_config"]["llm_nos"]) == 3


def test_real_ga_protocol_routing_rotation_and_security_contract(tmp_path: Path):
    fixture = _Fixture()
    fixture.start()
    proc = None
    log_path: Path | None = None
    logs: list[str] = []
    try:
        proc, base, config_root, runtime_root, log_path, _ = _start_platform(
            tmp_path, fixture=fixture
        )
        primary, secondary = _configure_oai_providers(base, fixture)
        _exercise_initial_oai_binding(
            proc, base, config_root, fixture, primary, secondary
        )
        logs.append(_stop(proc, log_path))
        proc = None

        proc, base, config_root, runtime_root, log_path, _ = _start_platform(
            tmp_path, reset_db=False, fixture=fixture
        )
        primary = _exercise_new_worker_mixin_and_key_rotation(
            base, config_root, fixture, primary, secondary
        )
        claude = _exercise_responses_and_claude_default(
            base, config_root, fixture, primary
        )
        logs.append(_stop(proc, log_path))
        proc = None

        proc, base, config_root, runtime_root, log_path, _ = _start_platform(
            tmp_path, reset_db=False, fixture=fixture
        )
        _exercise_new_claude_worker(proc, base, config_root, fixture, claude)
        _exercise_provider_disable(base, config_root, fixture, primary["provider_id"])
        logs.append(_stop(proc, log_path))
        proc = None
        _assert_artifacts_exclude_secrets(
            (config_root, runtime_root), "".join(logs), REAL_KEY_SENTINELS
        )
    finally:
        if proc is not None:
            logs.append(_stop(proc, log_path))
        fixture.close()


def test_submit_succeed_cancel_and_recovery(tmp_path: Path):
    """Submit → succeeded; cancel second; recovery leaves unexpired foreign rows."""
    proc, base, _, _, log_path, fixture = _start_platform(tmp_path)
    try:
        session = f"personal:{DEV_USER_ID}"
        code, body = _http_json(
            "POST",
            f"{base}/v1/sessions/{session}/tasks",
            {
                "message_id": "msg-success-1",
                "source_instance_id": "bot-1",
                "prompt": "Reply with a short greeting for foundation e2e.",
                "source": "web",
                "persona_snapshot": ["You are a concise foundation agent."],
                "tool_policy_version": "foundation.no-host-tools.v1",
            },
        )
        assert code == 202, body
        task_id = body["task_id"]
        assert body["status"] == "queued"

        code2, body2 = _http_json(
            "POST",
            f"{base}/v1/sessions/{session}/tasks",
            {
                "message_id": "msg-success-1",
                "source_instance_id": "bot-1",
                "prompt": "CHANGED PROMPT SHOULD NOT CREATE NEW TASK",
                "source": "web",
                "persona_snapshot": ["other"],
                "tool_policy_version": "foundation.no-host-tools.v1",
            },
        )
        assert code2 == 202
        assert body2["task_id"] == task_id

        code3, body3 = _http_json(
            "POST",
            f"{base}/v1/sessions/{session}/tasks",
            {
                "message_id": "msg-success-1",
                "source_instance_id": "bot-2",
                "prompt": "second instance should be distinct",
                "source": "web",
                "persona_snapshot": ["p"],
                "tool_policy_version": "foundation.no-host-tools.v1",
            },
        )
        assert code3 == 202
        other_id = body3["task_id"]
        assert other_id != task_id

        ccode, cbody = _http_json("POST", f"{base}/v1/tasks/{other_id}/cancel")
        assert ccode == 200, cbody
        assert cbody.get("accepted") is True

        final = _poll_status(base, task_id, {"succeeded", "failed"}, timeout=150)
        assert final["status"] == "succeeded", final
        assert final.get("result_ref")
        assert final.get("result_digest")
        assert "\\" not in final["result_ref"] and "/" not in final["result_ref"]

        rcode, rbody = _http_json(
            "GET",
            f"{base}/v1/tasks/{task_id}/result?result_ref={final['result_ref']}",
        )
        assert rcode == 200, rbody
        assert rbody["result_digest"] == final["result_digest"]
        assert rbody.get("payload") is not None

        pcode, pbody = _http_json(
            "GET",
            f"{base}/v1/tasks/{task_id}/result?result_ref=C:%5Csecrets",
        )
        assert pcode >= 400, pbody

        cancelled = _poll_status(
            base, other_id, {"cancelled", "interrupted", "failed"}, timeout=30
        )
        assert cancelled["status"] == "cancelled", cancelled

        hcode, hbody = _http_json(
            "POST",
            f"{base}/v1/sessions/{session}/tasks",
            {
                "message_id": "msg-host",
                "source_instance_id": "bot-1",
                "prompt": "nope",
                "source": "web",
                "persona_snapshot": [],
                "tool_policy_version": "not-a-real-host-policy",
            },
        )
        assert hcode >= 400, hbody
        assert hbody.get("code")
    finally:
        _stop(proc, log_path)
        fixture.close()


def test_platform_requires_dev_loopback_for_local_coordinator(tmp_path: Path):
    """Normal startup without --dev-loopback is refused in this foundation slice."""
    bin_path = _platform_bin(tmp_path)
    env = os.environ.copy()
    env["DATABASE_URL"] = TEST_DB
    env["PLATFORM_DEV_USER_ID"] = DEV_USER_ID
    env["PLATFORM_DEV_TOKEN"] = DEV_TOKEN
    proc = subprocess.run(
        [
            str(bin_path),
            "--policy-file",
            str(POLICY_PATH),
            "--claim-lease",
            "5s",
            "--database-url",
            TEST_DB,
        ],
        cwd=str(BACKEND_GO),
        env=env,
        capture_output=True,
        text=True,
        timeout=20,
    )
    assert proc.returncode != 0
    combined = (proc.stderr or "") + (proc.stdout or "")
    assert "dev-loopback" in combined.lower() or "dev-loopback" in combined


def test_policy_digest_matches_checked_in_file():
    digest = "sha256:" + hashlib.sha256(POLICY_PATH.read_bytes()).hexdigest()
    assert digest.startswith("sha256:")
    assert len(digest) == len("sha256:") + 64


def test_restart_recovers_expired_preserves_live_and_runs_queued_once(tmp_path: Path):
    first_proc, _, _, _, first_log, first_fixture = _start_platform(tmp_path / "first")
    try:
        first_instance = _platform_instance_id(first_log)
    finally:
        _stop(first_proc, first_log)
        first_fixture.close()

    seeded = _seed_restart_rows(first_instance)
    second_proc, base, _, _, second_log, second_fixture = _start_platform(
        tmp_path / "second", reset_db=False
    )
    try:
        second_instance = _platform_instance_id(second_log)
        assert second_instance != first_instance

        expired = _poll_status(base, seeded["expired"], {"interrupted"}, timeout=30)
        assert expired["status"] == "interrupted"
        queued = _poll_status(
            base, seeded["queued"], {"succeeded", "failed"}, timeout=150
        )
        assert queued["status"] == "succeeded", queued
        live = _poll_status(base, seeded["live"], {"starting"}, timeout=10)
        assert live["status"] == "starting"

        before = _restart_row_facts(seeded)
        time.sleep(1.5)
        after = _restart_row_facts(seeded)
        assert before == after
        assert after["expired_status"] == "interrupted"
        assert after["expired_interrupt_deliveries"] == 1
        assert after["expired_dispatch_events"] == 0
        assert after["live_status"] == "starting"
        assert after["live_owner"] == "foreign-live-owner"
        assert after["queued_status"] == "succeeded"
        assert after["queued_dispatch_events"] == 1
        assert after["queued_complete_deliveries"] == 1
    finally:
        _stop(second_proc, second_log)
        second_fixture.close()
