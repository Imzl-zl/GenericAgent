"""Standalone measured smoke for the PostgreSQL-backed tenant foundation."""

from __future__ import annotations

import hashlib
import json
import os
import signal
import socket
import subprocess
import sys
import tempfile
import threading
import time
import urllib.error
import urllib.request
import uuid
from dataclasses import dataclass
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
from typing import Any

REPO_ROOT = Path(__file__).resolve().parents[3]
BACKEND_GO = REPO_ROOT / "tenant_platform" / "backend-go"
WORKER_SRC = REPO_ROOT / "tenant_platform" / "worker-python" / "src"
REQUIRED_ENV = (
    "TEST_DATABASE_URL",
    "PLATFORM_DEV_USER_ID",
    "GA_LEGACY_ROOT",
    "GA_POLICY_FILE",
)
DEV_TOKEN = "foundation-smoke-dev-token-not-real"
OAI_TOKEN = "foundation-smoke-oai-token-not-real"
# The fixture key is submitted through the Admin Provider API. The Worker only
# receives a short-lived capability token; the signing key authenticates it to
# the Proxy (>= 32 bytes per llmproxy.MinSigningKeyLen).
CAPABILITY_SIGNING_KEY = "foundation-smoke-signing-key-not-real"
BOT_TOKEN_KEY = "0" * 64
POLICY_VERSION = "foundation.no-host-tools.v1"
TERMINAL_STATUSES = {"succeeded", "failed", "cancelled", "interrupted"}
CHILD_ENV_ALLOWLIST = frozenset(
    {
        "APPDATA",
        "COMSPEC",
        "HOME",
        "HOMEDRIVE",
        "HOMEPATH",
        "LANG",
        "LC_ALL",
        "LC_CTYPE",
        "LOCALAPPDATA",
        "PATH",
        "PATHEXT",
        "PYTHONIOENCODING",
        "PYTHONUTF8",
        "SYSTEMROOT",
        "TEMP",
        "TMP",
        "TZ",
        "USERPROFILE",
        "WINDIR",
    }
)
WINDOWS_NO_WINDOW = 0x08000000


class SmokeError(RuntimeError):
    pass


@dataclass(frozen=True)
class _StopResult:
    exit_code: int
    graceful_worker_shutdown: bool
    used_fallback: bool


class _WindowsJob:
    def __init__(self) -> None:
        self._handle: int | None = None
        if os.name != "nt":
            return
        import ctypes
        from ctypes import wintypes

        class _BasicLimits(ctypes.Structure):
            _fields_ = [
                ("PerProcessUserTimeLimit", ctypes.c_int64),
                ("PerJobUserTimeLimit", ctypes.c_int64),
                ("LimitFlags", wintypes.DWORD),
                ("MinimumWorkingSetSize", ctypes.c_size_t),
                ("MaximumWorkingSetSize", ctypes.c_size_t),
                ("ActiveProcessLimit", wintypes.DWORD),
                ("Affinity", ctypes.c_size_t),
                ("PriorityClass", wintypes.DWORD),
                ("SchedulingClass", wintypes.DWORD),
            ]

        class _IoCounters(ctypes.Structure):
            _fields_ = [
                (name, ctypes.c_ulonglong)
                for name in (
                    "ReadOperationCount",
                    "WriteOperationCount",
                    "OtherOperationCount",
                    "ReadTransferCount",
                    "WriteTransferCount",
                    "OtherTransferCount",
                )
            ]

        class _ExtendedLimits(ctypes.Structure):
            _fields_ = [
                ("BasicLimitInformation", _BasicLimits),
                ("IoInfo", _IoCounters),
                ("ProcessMemoryLimit", ctypes.c_size_t),
                ("JobMemoryLimit", ctypes.c_size_t),
                ("PeakProcessMemoryUsed", ctypes.c_size_t),
                ("PeakJobMemoryUsed", ctypes.c_size_t),
            ]

        kernel32 = ctypes.WinDLL("kernel32", use_last_error=True)
        kernel32.CreateJobObjectW.restype = wintypes.HANDLE
        handle = kernel32.CreateJobObjectW(None, None)
        if not handle:
            raise ctypes.WinError(ctypes.get_last_error())
        limits = _ExtendedLimits()
        limits.BasicLimitInformation.LimitFlags = 0x00002000
        if not kernel32.SetInformationJobObject(
            handle, 9, ctypes.byref(limits), ctypes.sizeof(limits)
        ):
            kernel32.CloseHandle(handle)
            raise ctypes.WinError(ctypes.get_last_error())
        self._handle = int(handle)

    def assign(self, proc: subprocess.Popen[Any]) -> None:
        if self._handle is None:
            return
        import ctypes

        if not ctypes.windll.kernel32.AssignProcessToJobObject(
            self._handle, int(proc._handle)
        ):
            raise ctypes.WinError(ctypes.get_last_error())

    def close(self) -> None:
        if self._handle is None:
            return
        import ctypes

        handle, self._handle = self._handle, None
        if not ctypes.windll.kernel32.CloseHandle(handle):
            raise ctypes.WinError(ctypes.get_last_error())


class _FixtureHandler(BaseHTTPRequestHandler):
    server: "_FixtureServer"

    def log_message(self, _fmt: str, *_args: Any) -> None:
        return

    def do_POST(self) -> None:
        if self.headers.get("Authorization", "") != f"Bearer {OAI_TOKEN}":
            self.server.wrong_auth_count += 1
            self._respond(401, {"error": "unauthorized"})
            return
        if self.path not in ("/v1/chat/completions", "/chat/completions"):
            self._respond(404, {"error": "not found"})
            return
        try:
            size = int(self.headers.get("Content-Length", "0"))
            payload = json.loads(self.rfile.read(size).decode("utf-8"))
        except (ValueError, json.JSONDecodeError, UnicodeDecodeError):
            self._respond(400, {"error": "invalid json"})
            return
        if not isinstance(payload, dict) or not isinstance(payload.get("stream"), bool):
            self._respond(400, {"error": "stream boolean required"})
            return
        self.server.valid_request_count += 1
        self.server.request_arrived.set()
        if not self.server.release_response.wait(timeout=45):
            self._respond(504, {"error": "fixture release timeout"})
            return
        if payload["stream"]:
            self._respond_sse("foundation-smoke-reply")
            return
        self._respond(
            200,
            {
                "id": "chatcmpl-foundation-smoke",
                "object": "chat.completion",
                "choices": [
                    {
                        "index": 0,
                        "message": {
                            "role": "assistant",
                            "content": "foundation-smoke-reply",
                        },
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

    def _respond_sse(self, text: str) -> None:
        events = [
            {
                "id": "chatcmpl-foundation-smoke",
                "object": "chat.completion.chunk",
                "choices": [
                    {"index": 0, "delta": {"content": text}, "finish_reason": None}
                ],
            },
            {
                "id": "chatcmpl-foundation-smoke",
                "object": "chat.completion.chunk",
                "choices": [{"index": 0, "delta": {}, "finish_reason": "stop"}],
                "usage": {
                    "prompt_tokens": 1,
                    "completion_tokens": 1,
                    "total_tokens": 2,
                },
            },
        ]
        self.send_response(200)
        self.send_header("Content-Type", "text/event-stream")
        self.send_header("Cache-Control", "no-cache")
        self.end_headers()
        try:
            for event in events:
                raw = json.dumps(event, separators=(",", ":")).encode("utf-8")
                self.wfile.write(b"data: " + raw + b"\n\n")
                self.wfile.flush()
            self.wfile.write(b"data: [DONE]\n\n")
            self.wfile.flush()
        except (BrokenPipeError, ConnectionResetError):
            pass

    def _respond(self, status: int, payload: dict[str, Any]) -> None:
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

    def __init__(self) -> None:
        super().__init__(("127.0.0.1", 0), _FixtureHandler)
        self.request_arrived = threading.Event()
        self.release_response = threading.Event()
        self.wrong_auth_count = 0
        self.valid_request_count = 0

    @property
    def base_url(self) -> str:
        host, port = self.server_address
        return f"http://{host}:{port}/v1"


class _Fixture:
    def __init__(self) -> None:
        self.server = _FixtureServer()
        self.thread = threading.Thread(
            target=self.server.serve_forever, name="foundation-oai", daemon=True
        )
        self.started = False

    def start(self) -> None:
        self.thread.start()
        self.started = True
        code, _ = _request_json(
            "POST",
            self.server.base_url + "/chat/completions",
            {"model": "self-check", "messages": [], "stream": False},
            headers={"Authorization": "Bearer deliberately-wrong"},
        )
        _require(
            code == 401 and self.server.wrong_auth_count == 1,
            "fixture accepted a wrong bearer token",
        )

    def close(self) -> None:
        self.server.release_response.set()
        if self.started:
            self.server.shutdown()
            self.thread.join(timeout=5)
        self.server.server_close()
        _require(not self.thread.is_alive(), "fixture thread did not stop")


def _require(condition: bool, message: str) -> None:
    if not condition:
        raise SmokeError(message)


def _required_environment() -> dict[str, str]:
    values = {name: os.environ.get(name, "").strip() for name in REQUIRED_ENV}
    missing = [name for name, value in values.items() if not value]
    if missing:
        raise SmokeError(
            "missing required environment variable(s): " + ", ".join(missing)
        )
    try:
        user_id = int(values["PLATFORM_DEV_USER_ID"])
    except ValueError as exc:
        raise SmokeError("PLATFORM_DEV_USER_ID must be a positive integer") from exc
    _require(user_id > 0, "PLATFORM_DEV_USER_ID must be a positive integer")
    values["PLATFORM_DEV_USER_ID"] = str(user_id)
    return values


def _validate_paths(values: dict[str, str]) -> tuple[Path, Path]:
    legacy_root = Path(values["GA_LEGACY_ROOT"]).resolve()
    policy_file = Path(values["GA_POLICY_FILE"]).resolve()
    _require(
        (legacy_root / "agentmain.py").is_file(),
        "GA_LEGACY_ROOT must contain agentmain.py",
    )
    _require(policy_file.is_file(), "GA_POLICY_FILE must name a file")
    _require(
        (WORKER_SRC / "ga_worker").is_dir(), "real Python Worker source is missing"
    )
    return legacy_root, policy_file


def _request_json(
    method: str,
    url: str,
    body: dict[str, Any] | None = None,
    *,
    headers: dict[str, str] | None = None,
    timeout: float = 10,
) -> tuple[int, dict[str, Any]]:
    request_headers = dict(headers or {})
    data = None
    if body is not None:
        data = json.dumps(body, separators=(",", ":")).encode("utf-8")
        request_headers["Content-Type"] = "application/json"
    request = urllib.request.Request(
        url, data=data, headers=request_headers, method=method
    )
    try:
        with urllib.request.urlopen(request, timeout=timeout) as response:
            raw = response.read()
            return response.status, json.loads(raw.decode("utf-8") or "{}")
    except urllib.error.HTTPError as exc:
        raw = exc.read()
        try:
            payload = json.loads(raw.decode("utf-8") or "{}")
        except json.JSONDecodeError:
            payload = {}
        return exc.code, payload


def _platform_request(
    method: str, base: str, path: str, body: dict[str, Any] | None = None
):
    return _request_json(
        method,
        base + path,
        body,
        headers={"X-Platform-Dev-Token": DEV_TOKEN},
    )


def _create_fixture_provider(base: str, fixture: _Fixture) -> None:
    code, response = _platform_request(
        "POST",
        base,
        "/v1/admin/llm-providers",
        {
            "name": "foundation-smoke-fixture",
            "provider_type": "native_oai",
            "base_url": fixture.server.base_url,
            "model": "gpt-4o",
            "api_key": OAI_TOKEN,
            "session_config": {"stream": True, "max_retries": 0},
            "transport_config": {"auth_mode": "auto"},
        },
    )
    _require(code == 201, f"fixture provider create failed: {response}")

    code, providers = _platform_request("GET", base, "/v1/admin/llm-providers")
    configured = providers.get("providers", [])
    _require(
        code == 200 and len(configured) == 1 and configured[0]["is_default"],
        f"legacy environment unexpectedly seeded a Provider: {providers}",
    )


def _free_loopback_address() -> tuple[str, str]:
    with socket.socket(socket.AF_INET, socket.SOCK_STREAM) as sock:
        sock.bind(("127.0.0.1", 0))
        port = sock.getsockname()[1]
    return f"127.0.0.1:{port}", f"http://127.0.0.1:{port}"


def _build_platform(output: Path) -> None:
    completed = subprocess.run(
        ["go", "build", "-o", str(output), "./cmd/platform"],
        cwd=BACKEND_GO,
        capture_output=True,
        text=True,
        encoding="utf-8",
        errors="replace",
        timeout=120,
        check=False,
    )
    _require(completed.returncode == 0, "Go platform build failed")


def _child_environment(
    values: dict[str, str],
    config_root: Path,
    runtime_root: Path,
    legacy_root: Path,
    policy_file: Path,
) -> dict[str, str]:
    env = {
        name: value
        for name, value in os.environ.items()
        if name.upper() in CHILD_ENV_ALLOWLIST
    }
    env.update(
        {
            "DATABASE_URL": values["TEST_DATABASE_URL"],
            "PLATFORM_DEV_USER_ID": values["PLATFORM_DEV_USER_ID"],
            "PLATFORM_DEV_USERNAME": f"foundation-smoke-{values['PLATFORM_DEV_USER_ID']}",
            "PLATFORM_DEV_TOKEN": DEV_TOKEN,
            "GA_CONFIG_ROOT": str(config_root),
            "GA_RUNTIME_DIR": str(runtime_root),
            "GA_LEGACY_ROOT": str(legacy_root),
            "GA_POLICY_FILE": str(policy_file),
            "GA_WORKER_PYTHON": os.environ.get("GA_WORKER_PYTHON", "").strip()
            or sys.executable,
            "GA_WORKER_SRC": str(WORKER_SRC),
            "BOT_TOKEN_KEY": BOT_TOKEN_KEY,
            # The fixture key is sent later through the Admin Provider API.
            "LLM_PROXY_CAPABILITY_SIGNING_KEY": values.get(
                "LLM_PROXY_CAPABILITY_SIGNING_KEY", ""
            ),
            "LLM_PROXY_ALLOWED_UPSTREAM_CIDRS": values.get(
                "LLM_PROXY_ALLOWED_UPSTREAM_CIDRS", ""
            ),
            "LLM_PROXY_ALLOW_HTTP_HOSTS": values.get("LLM_PROXY_ALLOW_HTTP_HOSTS", ""),
        }
    )
    return env


def _start_platform(
    binary: Path,
    log_file: Any,
    env: dict[str, str],
    policy_file: Path,
    config_root: Path,
    runtime_root: Path,
    legacy_root: Path,
) -> tuple[subprocess.Popen[Any], str]:
    listen, base = _free_loopback_address()
    try:
        policy_arg = os.path.relpath(policy_file, BACKEND_GO)
    except ValueError:
        policy_arg = str(policy_file)
    args = [
        str(binary),
        "--dev-loopback",
        "--policy-file",
        policy_arg,
        "--claim-lease",
        "5s",
        "--listen",
        listen,
        "--config-root",
        str(config_root),
        "--runtime-root",
        str(runtime_root),
        "--legacy-root",
        str(legacy_root),
        "--worker-python",
        env["GA_WORKER_PYTHON"],
        "--worker-src",
        str(WORKER_SRC),
    ]
    kwargs: dict[str, Any] = {"start_new_session": os.name != "nt"}
    if os.name == "nt":
        kwargs["creationflags"] = subprocess.CREATE_NEW_PROCESS_GROUP
    proc = subprocess.Popen(
        args,
        cwd=BACKEND_GO,
        env=env,
        stdout=log_file,
        stderr=subprocess.STDOUT,
        **kwargs,
    )
    return proc, base


def _wait_health(proc: subprocess.Popen[Any], base: str) -> None:
    deadline = time.monotonic() + 45
    last_error: Exception | None = None
    while time.monotonic() < deadline:
        _require(proc.poll() is None, "platform exited before health became ready")
        try:
            code, body = _request_json("GET", base + "/healthz", timeout=2)
            if code == 200 and body.get("status") == "ok":
                return
        except (OSError, urllib.error.URLError) as exc:
            last_error = exc
        time.sleep(0.2)
    raise SmokeError(
        f"platform health timeout ({type(last_error).__name__ if last_error else 'not ready'})"
    )


def _process_rows() -> list[tuple[int, int, int]]:
    if os.name == "nt":
        command = (
            "Get-CimInstance Win32_Process | Select-Object ProcessId,ParentProcessId,WorkingSetSize "
            "| ConvertTo-Json -Compress"
        )
        completed = subprocess.run(
            ["powershell.exe", "-NoProfile", "-NonInteractive", "-Command", command],
            capture_output=True,
            text=True,
            encoding="utf-8",
            errors="replace",
            timeout=15,
            creationflags=WINDOWS_NO_WINDOW,
            check=False,
        )
        _require(completed.returncode == 0, "unable to sample Worker RSS")
        raw = json.loads(completed.stdout or "[]")
        items = raw if isinstance(raw, list) else [raw]
        return [
            (
                int(item["ProcessId"]),
                int(item["ParentProcessId"]),
                int(item.get("WorkingSetSize") or 0),
            )
            for item in items
        ]
    completed = subprocess.run(
        ["ps", "-eo", "pid=,ppid=,rss="],
        capture_output=True,
        text=True,
        timeout=10,
        check=False,
    )
    _require(completed.returncode == 0, "unable to sample Worker RSS")
    return [
        (int(pid), int(ppid), int(rss) * 1024)
        for pid, ppid, rss in map(str.split, completed.stdout.splitlines())
    ]


def _sample_descendants(root_pid: int) -> tuple[set[int], int]:
    rows = _process_rows()
    descendants: set[int] = set()
    changed = True
    while changed:
        changed = False
        parents = descendants | {root_pid}
        for pid, parent, _rss in rows:
            if parent in parents and pid not in descendants:
                descendants.add(pid)
                changed = True
    rss = sum(size for pid, _parent, size in rows if pid in descendants)
    return descendants, rss


def _pid_alive(pid: int) -> bool:
    if os.name == "nt":
        import ctypes

        handle = ctypes.windll.kernel32.OpenProcess(0x1000, False, pid)
        if not handle:
            return False
        try:
            code = ctypes.c_ulong()
            return (
                bool(
                    ctypes.windll.kernel32.GetExitCodeProcess(
                        handle, ctypes.byref(code)
                    )
                )
                and code.value == 259
            )
        finally:
            ctypes.windll.kernel32.CloseHandle(handle)
    try:
        os.kill(pid, 0)
        return True
    except ProcessLookupError:
        return False


def _wait_pids_gone(pids: set[int], timeout: float) -> bool:
    deadline = time.monotonic() + timeout
    alive = {pid for pid in pids if _pid_alive(pid)}
    while alive and time.monotonic() < deadline:
        time.sleep(0.1)
        alive = {pid for pid in alive if _pid_alive(pid)}
    return not alive


def _force_process_tree(
    proc: subprocess.Popen[Any], known_children: set[int], job: _WindowsJob | None
) -> None:
    if os.name == "nt":
        if job is not None:
            try:
                job.close()
            except OSError:
                pass
        targets = set(known_children)
        if proc.poll() is None:
            targets.add(proc.pid)
        for pid in targets:
            if _pid_alive(pid):
                subprocess.run(
                    ["taskkill.exe", "/PID", str(pid), "/T", "/F"],
                    capture_output=True,
                    creationflags=WINDOWS_NO_WINDOW,
                    timeout=10,
                    check=False,
                )
        return
    try:
        os.killpg(proc.pid, signal.SIGKILL)
    except ProcessLookupError:
        pass


def _stop_process_tree(
    proc: subprocess.Popen[Any],
    known_children: set[int],
    *,
    job: _WindowsJob | None = None,
    sample_descendants: Any = _sample_descendants,
    grace_seconds: float = 12,
) -> _StopResult:
    sampling_ok = True
    try:
        children, _ = sample_descendants(proc.pid)
        known_children.update(children)
    except Exception:
        sampling_ok = False
    platform_stopped = proc.poll() is not None
    if not platform_stopped:
        try:
            if os.name == "nt":
                proc.send_signal(signal.CTRL_BREAK_EVENT)
            else:
                proc.send_signal(signal.SIGTERM)
            proc.wait(timeout=grace_seconds)
            platform_stopped = True
        except (OSError, subprocess.TimeoutExpired):
            platform_stopped = proc.poll() is not None
    workers_stopped = sampling_ok and _wait_pids_gone(
        known_children, min(8.0, grace_seconds)
    )
    if platform_stopped and workers_stopped:
        return _StopResult(int(proc.returncode or 0), True, False)
    _force_process_tree(proc, known_children, job)
    if proc.poll() is None:
        proc.wait(timeout=10)
    _require(_wait_pids_gone(known_children, 5), "Worker descendant cleanup failed")
    return _StopResult(int(proc.returncode or 0), False, True)


def _poll_task(
    base: str, task_id: str, wanted: set[str], timeout: float
) -> dict[str, Any]:
    deadline = time.monotonic() + timeout
    last: dict[str, Any] = {}
    while time.monotonic() < deadline:
        code, body = _platform_request("GET", base, f"/v1/tasks/{task_id}")
        if code == 200:
            last = body
            if body.get("status") in wanted:
                return body
        time.sleep(0.25)
    raise SmokeError(
        f"task did not reach expected terminal status; last status={last.get('status', 'unknown')}"
    )


def _submit(base: str, session: str, payload: dict[str, Any]) -> dict[str, Any]:
    code, body = _platform_request(
        "POST", base, f"/v1/sessions/{session}/tasks", payload
    )
    _require(
        code == 202 and isinstance(body.get("task_id"), str), "durable submit failed"
    )
    return body


def _database_facts(
    database_url: str, success_id: str, cancel_id: str
) -> dict[str, Any]:
    try:
        import psycopg
    except ImportError as exc:
        raise SmokeError("psycopg is required for smoke measurements") from exc
    with psycopg.connect(database_url) as conn:
        row = conn.execute(
            """
            SELECT id::text, checksum, result_digest, created_at, committed_at
            FROM workspace_snapshots
            WHERE task_id = %s AND state = 'committed'
            """,
            (success_id,),
        ).fetchone()
        _require(row is not None, "success task has no committed checkpoint")
        chunk_count, chunk_bytes = conn.execute(
            "SELECT count(*), COALESCE(sum(byte_count), 0) FROM task_events WHERE task_id=%s AND event_type='chunk'",
            (success_id,),
        ).fetchone()
        cancel_snapshots = conn.execute(
            "SELECT count(*) FROM workspace_snapshots WHERE task_id=%s", (cancel_id,)
        ).fetchone()[0]
    _require(
        chunk_count > 0 and chunk_bytes > 0,
        "real Worker display stream did not produce task chunk events",
    )
    _require(cancel_snapshots == 0, "cancelled task unexpectedly created a snapshot")
    return {
        "snapshot_id": row[0],
        "checkpoint_digest": row[1],
        "checkpoint_result_digest": row[2],
        "checkpoint_prepare_to_commit_ms": max(
            0, int((row[4] - row[3]).total_seconds() * 1000)
        ),
        "chunk_count": int(chunk_count),
        "chunk_bytes": int(chunk_bytes),
    }


def _launch_stack(
    values: dict[str, str],
    legacy_root: Path,
    policy_file: Path,
    tmp: Path,
    fixture: _Fixture,
    state: dict[str, Any],
) -> tuple[subprocess.Popen[Any], str, Path]:
    config_root, runtime_root = tmp / "config", tmp / "runtime"
    state["config_root"] = config_root
    state["runtime_root"] = runtime_root
    runtime_root.mkdir()
    # The scheduler writes token-only runtime configuration at Worker startup.
    # The fixture key enters the encrypted Provider store through the Admin API.
    fixture_host, fixture_port = fixture.server.server_address
    platform_values = {
        **values,
        "LLM_PROXY_CAPABILITY_SIGNING_KEY": CAPABILITY_SIGNING_KEY,
        "LLM_PROXY_ALLOWED_UPSTREAM_CIDRS": "127.0.0.0/8",
        "LLM_PROXY_ALLOW_HTTP_HOSTS": f"{fixture_host}:{fixture_port}",
    }
    binary = tmp / ("platform.exe" if os.name == "nt" else "platform")
    _build_platform(binary)
    log_file = (tmp / "children.log").open("w+b")
    state["log_path"] = tmp / "children.log"
    state["log_file"] = log_file
    env = _child_environment(
        platform_values, config_root, runtime_root, legacy_root, policy_file
    )
    # Retired environment values are deliberately present: Platform must ignore
    # them and accept the sole Provider through the Admin API below.
    env["LLM_PROXY_UPSTREAM_BASEURL"] = "http://127.0.0.1:1/v1"
    env["LLM_PROXY_UPSTREAM_APIKEY"] = "retired-upstream-key-not-real"
    proc, base = _start_platform(
        binary, log_file, env, policy_file, config_root, runtime_root, legacy_root
    )
    state["proc"] = proc
    state["job"].assign(proc)
    _wait_health(proc, base)
    _create_fixture_provider(base, fixture)
    return proc, base, runtime_root


def _submit_deduped_success(
    base: str,
    session: str,
    unique: str,
    fixture: _Fixture,
) -> dict[str, Any]:
    payload = {
        "message_id": f"success-{unique}",
        "source_instance_id": f"foundation-smoke-{unique}",
        "prompt": "Produce the deterministic foundation smoke response.",
        "source": "web",
        "persona_snapshot": ["Use the deterministic local fixture response."],
        "tool_policy_version": POLICY_VERSION,
    }
    started = time.monotonic()
    submitted = _submit(base, session, payload)
    duplicate_payload = dict(payload)
    duplicate_payload["prompt"] = "A duplicate must not replace the durable original."
    duplicate = _submit(base, session, duplicate_payload)
    _require(
        duplicate["task_id"] == submitted["task_id"],
        "same-key submit was not deduplicated",
    )
    _require(
        fixture.server.request_arrived.wait(timeout=40),
        "real Worker never reached the OpenAI fixture",
    )
    return {
        "task_id": submitted["task_id"],
        "dedupe_id": duplicate["task_id"],
        "started": started,
    }


def _cancel_queued_task(base: str, session: str, unique: str) -> dict[str, Any]:
    started = time.monotonic()
    submitted = _submit(
        base,
        session,
        {
            "message_id": f"cancel-{unique}",
            "source_instance_id": f"foundation-smoke-{unique}",
            "prompt": "This queued task must be cancelled before Worker dispatch.",
            "source": "web",
            "persona_snapshot": ["Cancellation smoke task."],
            "tool_policy_version": POLICY_VERSION,
        },
    )
    code, response = _platform_request(
        "POST", base, f"/v1/tasks/{submitted['task_id']}/cancel"
    )
    _require(
        code == 200 and response.get("accepted") is True,
        "cancellation was not accepted",
    )
    task = _poll_task(base, submitted["task_id"], {"cancelled"}, 20)
    elapsed = int((time.monotonic() - started) * 1000)
    _require(
        not any(key in task for key in ("snapshot_id", "result_ref", "result_digest")),
        "cancelled task published result state",
    )
    result_code, _ = _platform_request(
        "GET", base, f"/v1/tasks/{submitted['task_id']}/result"
    )
    _require(result_code >= 400, "cancelled task exposed a result")
    return {
        "task_id": submitted["task_id"],
        "status": task["status"],
        "elapsed": elapsed,
    }


def _sample_worker_after_terminal(
    proc: subprocess.Popen[Any], known_children: set[int]
) -> int:
    children, rss = _sample_descendants(proc.pid)
    known_children.update(children)
    _require(bool(children) and rss > 0, "Worker process was not measurable")
    return rss


def _complete_success(
    base: str,
    database_url: str,
    runtime_root: Path,
    fixture: _Fixture,
    proc: subprocess.Popen[Any],
    known_children: set[int],
    success: dict[str, Any],
    cancel_id: str,
) -> dict[str, Any]:
    fixture.server.release_response.set()
    task = _poll_task(base, success["task_id"], TERMINAL_STATUSES, 150)
    elapsed = int((time.monotonic() - success["started"]) * 1000)
    _require(task.get("status") == "succeeded", "foundation success task failed")
    result_ref = task.get("result_ref")
    _require(
        isinstance(result_ref, str)
        and "/" not in result_ref
        and "\\" not in result_ref,
        "invalid result ref",
    )
    code, result = _platform_request(
        "GET", base, f"/v1/tasks/{success['task_id']}/result?result_ref={result_ref}"
    )
    _require(
        code == 200 and result.get("result_digest") == task.get("result_digest"),
        "digest-checked result readback failed",
    )
    returned_text = result.get("payload")
    _require(
        isinstance(returned_text, str),
        "result payload was not the expected text string",
    )
    _require(
        "!!!Error:" not in returned_text and "[Error:" not in returned_text,
        "GA returned an LLM error",
    )
    returned_digest = (
        "sha256:" + hashlib.sha256(returned_text.encode("utf-8")).hexdigest()
    )
    _require(
        returned_digest == result.get("result_digest"),
        "returned result body digest mismatch",
    )
    facts = _database_facts(database_url, success["task_id"], cancel_id)
    _require(
        facts["checkpoint_result_digest"] == task["result_digest"],
        "checkpoint/result digest mismatch",
    )
    bundle = runtime_root / "committed" / f"{facts['snapshot_id']}.bundle.json"
    _require(bundle.is_file(), "committed checkpoint bundle is missing")
    raw = bundle.read_bytes()
    _require(
        "sha256:" + hashlib.sha256(raw).hexdigest() == facts["checkpoint_digest"],
        "committed checkpoint digest mismatch",
    )
    rss = _sample_worker_after_terminal(proc, known_children)
    _require(
        fixture.server.valid_request_count == 1,
        "cancelled task reached the OpenAI fixture",
    )
    return {
        "task": task,
        "elapsed": elapsed,
        "facts": facts,
        "bundle_bytes": len(raw),
        "rss": rss,
    }


def _make_outputs(
    success: dict[str, Any],
    completed: dict[str, Any],
    cancelled: dict[str, Any],
    total_elapsed: int,
) -> tuple[dict[str, Any], dict[str, Any]]:
    task, facts = completed["task"], completed["facts"]
    summary = {
        "success_task_id": success["task_id"],
        "dedupe_task_id": success["dedupe_id"],
        "success_status": task["status"],
        "result_digest": task["result_digest"],
        "checkpoint_digest": facts["checkpoint_digest"],
        "checkpoint_result_digest": facts["checkpoint_result_digest"],
        "success_elapsed_ms": completed["elapsed"],
        "cancel_task_id": cancelled["task_id"],
        "cancel_status": cancelled["status"],
        "cancel_elapsed_ms": cancelled["elapsed"],
        "total_elapsed_ms": total_elapsed,
    }
    metrics = {
        "worker_peak_rss_bytes": completed["rss"],
        "checkpoint_bundle_bytes": completed["bundle_bytes"],
        "checkpoint_prepare_to_commit_ms": facts["checkpoint_prepare_to_commit_ms"],
        "success_task_latency_ms": completed["elapsed"],
        "cancel_task_latency_ms": cancelled["elapsed"],
        "fixture_wrong_bearer_status": 401,
        "display_chunk_events": facts["chunk_count"],
        "display_chunk_bytes": facts["chunk_bytes"],
    }
    return summary, metrics


def _exercise(
    values: dict[str, str],
    legacy_root: Path,
    policy_file: Path,
    tmp: Path,
    fixture: _Fixture,
    state: dict[str, Any],
) -> tuple[dict[str, Any], dict[str, Any]]:
    total_started = time.monotonic()
    proc, base, runtime_root = _launch_stack(
        values, legacy_root, policy_file, tmp, fixture, state
    )
    session = f"personal:{values['PLATFORM_DEV_USER_ID']}"
    unique = uuid.uuid4().hex
    success = _submit_deduped_success(base, session, unique, fixture)
    cancelled = _cancel_queued_task(base, session, unique)
    completed = _complete_success(
        base,
        values["TEST_DATABASE_URL"],
        runtime_root,
        fixture,
        proc,
        state["known_children"],
        success,
        cancelled["task_id"],
    )
    total_elapsed = int((time.monotonic() - total_started) * 1000)
    elapsed_values = (
        completed["elapsed"],
        cancelled["elapsed"],
        total_elapsed,
        completed["facts"]["checkpoint_prepare_to_commit_ms"],
    )
    _require(
        all(0 <= value <= 180_000 for value in elapsed_values),
        "elapsed measurement was out of bounds",
    )
    return _make_outputs(success, completed, cancelled, total_elapsed)


def _assert_no_real_key_artifacts(state: dict[str, Any]) -> None:
    secrets = (OAI_TOKEN, CAPABILITY_SIGNING_KEY)
    for key in ("config_root", "runtime_root"):
        root = Path(state[key])
        for path in root.rglob("*"):
            if not path.is_file():
                continue
            content = path.read_bytes()
            for secret in secrets:
                _require(secret.encode() not in content, f"secret leaked into {path}")
    log_content = Path(state["log_path"]).read_bytes()
    for secret in secrets:
        _require(
            secret.encode() not in log_content, "secret leaked into child process logs"
        )


def run() -> tuple[dict[str, Any], dict[str, Any]]:
    values = _required_environment()
    legacy_root, policy_file = _validate_paths(values)
    fixture = _Fixture()
    temp_context = tempfile.TemporaryDirectory(prefix="foundation-smoke-")
    state: dict[str, Any] = {
        "proc": None,
        "log_file": None,
        "known_children": set(),
        "job": _WindowsJob(),
    }
    try:
        fixture.start()
        summary, metrics = _exercise(
            values, legacy_root, policy_file, Path(temp_context.name), fixture, state
        )
        fixture.server.release_response.set()
        stop_result = _stop_process_tree(
            state["proc"], state["known_children"], job=state["job"]
        )
        state["proc"] = None
        _require(
            stop_result.graceful_worker_shutdown and not stop_result.used_fallback,
            "normal shutdown required process-containment fallback",
        )
        if state["log_file"] is not None:
            state["log_file"].close()
            state["log_file"] = None
        _assert_no_real_key_artifacts(state)
        metrics["platform_shutdown_exit_code"] = stop_result.exit_code
        metrics["worker_descendants_cleaned"] = True
        metrics["worker_graceful_shutdown"] = True
    finally:
        fixture.server.release_response.set()
        try:
            if state["proc"] is not None:
                _stop_process_tree(
                    state["proc"], state["known_children"], job=state["job"]
                )
        finally:
            try:
                if state["log_file"] is not None:
                    state["log_file"].close()
                    state["log_file"] = None
            finally:
                try:
                    state["job"].close()
                finally:
                    try:
                        fixture.close()
                    finally:
                        temp_context.cleanup()
    return summary, metrics


def main() -> int:
    try:
        summary, metrics = run()
        observation_file = os.environ.get(
            "FOUNDATION_SMOKE_OBSERVATION_FILE", ""
        ).strip()
        if observation_file:
            Path(observation_file).write_text(
                json.dumps(metrics, sort_keys=True) + "\n", encoding="utf-8"
            )
        encoded = json.dumps(summary, separators=(",", ":"), sort_keys=True)
        _require(
            len(encoded.encode("utf-8")) <= 2048, "summary exceeded bounded output size"
        )
        print(encoded)
        return 0
    except Exception as exc:
        print(f"foundation-smoke: {type(exc).__name__}: {exc}", file=sys.stderr)
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
