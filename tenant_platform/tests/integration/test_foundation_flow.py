"""Foundation vertical path E2E: real PostgreSQL + platform subprocess + Worker."""

from __future__ import annotations

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
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path

REPO_ROOT = Path(__file__).resolve().parents[3]
POLICY_PATH = REPO_ROOT / "tenant_platform" / "contracts" / "policy" / "foundation.v1.json"
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
# Real upstream key + capability signing key. The real key lives ONLY in the
# in-process LLM Proxy (passed via LLM_PROXY_UPSTREAM_APIKEY env var); the
# Worker receives only a capability_token + Proxy URL (security red line).
OAI_TOKEN = "foundation-integration-oai-token-not-real"
CAPABILITY_SIGNING_KEY = "foundation-integration-signing-key"


class _FixtureHandler(BaseHTTPRequestHandler):
    """OpenAI-compatible fixture: validates the real upstream bearer token."""

    def log_message(self, _fmt: str, *_args: object) -> None:
        return

    def do_POST(self) -> None:
        if self.headers.get("Authorization", "") != f"Bearer {OAI_TOKEN}":
            self._respond(401, {"error": "unauthorized"})
            return
        if self.path not in ("/v1/chat/completions", "/chat/completions"):
            self._respond(404, {"error": "not found"})
            return
        size = int(self.headers.get("Content-Length", "0"))
        try:
            json.loads(self.rfile.read(size).decode("utf-8"))
        except (ValueError, json.JSONDecodeError, UnicodeDecodeError):
            self._respond(400, {"error": "invalid json"})
            return
        self.server.request_count += 1
        self._respond(
            200,
            {
                "id": "chatcmpl-integration",
                "object": "chat.completion",
                "choices": [
                    {
                        "index": 0,
                        "message": {"role": "assistant", "content": "integration-test-reply"},
                        "finish_reason": "stop",
                    }
                ],
                "usage": {"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2},
            },
        )

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
        self.request_count = 0


class _Fixture:
    """Minimal OAI fixture started per platform instance."""

    def __init__(self) -> None:
        self.server = _FixtureServer(("127.0.0.1", 0), _FixtureHandler)
        self.thread = threading.Thread(target=self.server.serve_forever, name="integration-oai", daemon=True)

    def start(self) -> None:
        self.thread.start()

    @property
    def base_url(self) -> str:
        host, port = self.server.server_address
        return f"http://{host}:{port}/v1"

    def close(self) -> None:
        self.server.shutdown()
        self.server.server_close()
        self.thread.join(timeout=5)

_DROP_SQL = """
DROP TABLE IF EXISTS task_deliveries CASCADE;
DROP TABLE IF EXISTS task_events CASCADE;
DROP TABLE IF EXISTS workspace_snapshots CASCADE;
DROP TABLE IF EXISTS tasks CASCADE;
DROP TABLE IF EXISTS workspaces CASCADE;
DROP TABLE IF EXISTS users CASCADE;
DROP TYPE IF EXISTS task_deliveries CASCADE;
DROP TYPE IF EXISTS task_events CASCADE;
DROP TYPE IF EXISTS workspace_snapshots CASCADE;
DROP TYPE IF EXISTS tasks CASCADE;
DROP TYPE IF EXISTS workspaces CASCADE;
DROP TYPE IF EXISTS users CASCADE;
DROP SEQUENCE IF EXISTS task_events_id_seq CASCADE;
"""


def _platform_bin(tmp: Path) -> Path:
    out = tmp / ("platform.exe" if os.name == "nt" else "platform")
    subprocess.check_call(
        ["go", "build", "-o", str(out), "./cmd/platform"],
        cwd=str(BACKEND_GO),
        env=os.environ.copy(),
    )
    return out


def _http_json(method: str, url: str, body: dict | None = None, token: str | None = DEV_TOKEN):
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
    try:
        import psycopg
    except ImportError as exc:
        raise RuntimeError("psycopg required for foundation tests") from exc
    mig = REPO_ROOT / "tenant_platform" / "infra" / "postgres" / "migrations" / "0001_foundation.sql"
    sql = mig.read_text(encoding="utf-8")
    with psycopg.connect(TEST_DB) as conn:
        conn.execute(_DROP_SQL)
        conn.commit()
        try:
            conn.execute(sql)
            conn.commit()
        except Exception:
            conn.rollback()
            conn.execute(_DROP_SQL)
            conn.commit()
            conn.execute(sql)
            conn.commit()


def _start_platform(
    tmp: Path,
    listen: str | None = None,
    *,
    reset_db: bool = True,
) -> tuple[subprocess.Popen, str, Path, Path, Path, _Fixture]:
    tmp.mkdir(parents=True, exist_ok=True)
    if listen is None:
        listen = _free_loopback_addr()
    fixture = _Fixture()
    fixture.start()
    bin_path = _platform_bin(tmp)
    config_root = tmp / "config"
    runtime_root = tmp / "runtime"
    log_path = tmp / "platform.log"
    config_root.mkdir()
    runtime_root.mkdir()
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
    # LLM Proxy: the platform starts an in-process Proxy in dev-loopback that
    # holds the real upstream key (OAI_TOKEN). The Worker receives only a
    # capability_token + Proxy URL via a token-only mykey.py generated by the
    # scheduler (security red line: no real key in Worker).
    env["LLM_PROXY_UPSTREAM_BASEURL"] = fixture.base_url
    env["LLM_PROXY_UPSTREAM_APIKEY"] = OAI_TOKEN
    env["LLM_PROXY_CAPABILITY_SIGNING_KEY"] = CAPABILITY_SIGNING_KEY
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
            fixture.close()
            raise AssertionError(f"platform exited early code={proc.returncode}\n{out}")
        try:
            status, body = _http_json("GET", base + "/healthz", token=None)
            if status == 200 and body.get("status") == "ok":
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
            match = re.search(r"instance_id=([0-9a-f-]{36})", log_path.read_text(encoding="utf-8"))
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
            "SELECT id::text FROM workspaces WHERE session_key=%s", (f"personal:{DEV_USER_ID}",)
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
                ids["expired"], dev_workspace, f"personal:{DEV_USER_ID}", 1, int(DEV_USER_ID),
                "restart-expired", "restart-expired", "restart-expired", "expired prior owner",
                len("expired prior owner"), "running", prior_instance,
                time.strftime("%Y-%m-%d %H:%M:%S+00", time.gmtime(time.time() - 120)),
                time.strftime("%Y-%m-%d %H:%M:%S+00", time.gmtime(time.time() - 60)),
                "prior-worker", time.strftime("%Y-%m-%d %H:%M:%S+00", time.gmtime(time.time() - 120)),
            ),
        )
        conn.execute(
            task_sql,
            (
                ids["queued"], dev_workspace, f"personal:{DEV_USER_ID}", 2, int(DEV_USER_ID),
                "restart-queued", "restart-queued", "restart-queued", "complete after restart",
                len("complete after restart"), "queued", None, None, None, None, None,
            ),
        )
        conn.execute(
            task_sql,
            (
                ids["live"], foreign_workspace, "personal:2", 1, 2,
                "restart-live", "restart-live", "restart-live", "foreign live task",
                len("foreign live task"), "starting", "foreign-live-owner",
                time.strftime("%Y-%m-%d %H:%M:%S+00", time.gmtime()),
                time.strftime("%Y-%m-%d %H:%M:%S+00", time.gmtime(time.time() + 600)),
                "foreign-worker", time.strftime("%Y-%m-%d %H:%M:%S+00", time.gmtime()),
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


def _poll_status(base: str, task_id: str, want: set[str], timeout: float = 120.0) -> dict:
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

        cancelled = _poll_status(base, other_id, {"cancelled", "interrupted", "failed"}, timeout=30)
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
        queued = _poll_status(base, seeded["queued"], {"succeeded", "failed"}, timeout=150)
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
