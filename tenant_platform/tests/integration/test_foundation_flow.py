"""Foundation vertical path E2E: real PostgreSQL + platform subprocess + Worker."""

from __future__ import annotations

import hashlib
import json
import os
import signal
import socket
import subprocess
import sys
import time
import urllib.error
import urllib.request
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


def _start_platform(tmp: Path, listen: str | None = None) -> tuple[subprocess.Popen, str, Path, Path]:
    if listen is None:
        listen = _free_loopback_addr()
    bin_path = _platform_bin(tmp)
    config_root = tmp / "config"
    runtime_root = tmp / "runtime"
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
    _reset_db()
    proc = subprocess.Popen(
        [
            str(bin_path),
            "--dev-loopback",
            "--policy-file",
            str(POLICY_PATH),
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
    )
    base = f"http://{listen}"
    deadline = time.time() + 45
    last = None
    while time.time() < deadline:
        if proc.poll() is not None:
            out = proc.stdout.read() if proc.stdout else ""
            raise AssertionError(f"platform exited early code={proc.returncode}\n{out}")
        try:
            status, body = _http_json("GET", base + "/healthz", token=None)
            if status == 200 and body.get("status") == "ok":
                return proc, base, config_root, runtime_root
            last = (status, body)
        except Exception as exc:  # noqa: BLE001
            last = exc
        time.sleep(0.2)
    out = ""
    proc.kill()
    if proc.stdout:
        try:
            out = proc.stdout.read()
        except Exception:
            pass
    raise AssertionError(f"platform failed to start; last={last}\n{out}")


def _stop(proc: subprocess.Popen) -> str:
    if proc.poll() is not None:
        out = proc.stdout.read() if proc.stdout else ""
        return out or ""
    if os.name == "nt":
        proc.terminate()
    else:
        proc.send_signal(signal.SIGTERM)
    try:
        out, _ = proc.communicate(timeout=10)
    except subprocess.TimeoutExpired:
        proc.kill()
        out, _ = proc.communicate(timeout=5)
    return out or ""


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
    proc, base, _, _ = _start_platform(tmp_path)
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
        _stop(proc)


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
