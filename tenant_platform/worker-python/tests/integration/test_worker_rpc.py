"""Real GenericAgent Worker integration smoke via subprocess + local OpenAI fixture."""

from __future__ import annotations

import hashlib
import json
import os
import re
import subprocess
import sys
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path

import grpc
import pytest

from genericagent.worker.v1 import worker_pb2, worker_pb2_grpc
from ga_worker.checkpoint import SNAPSHOT_SCHEMA_VERSION, build_snapshot_bundle, result_digest_for
from ga_worker.limits import CapabilityRegistry

REPO_ROOT = Path(__file__).resolve().parents[4]
WORKER_ROOT = REPO_ROOT / "tenant_platform" / "worker-python"
POLICY_PATH = REPO_ROOT / "tenant_platform" / "contracts" / "policy" / "foundation.v1.json"
PYTHON = Path(os.environ.get("GA_TEST_PYTHON") or sys.executable)
TEST_TOKEN = "test-worker-token-not-a-real-key"
FOUNDATION_DIGEST = "sha256:" + hashlib.sha256(POLICY_PATH.read_bytes()).hexdigest()

# Plan Task 3 Step 3 overlay manifest: every legacy file the Worker is allowed
# to copy. The integration test snapshots exactly this set before launching the
# worker and verifies none are modified, proving writes never reach GA_LEGACY_ROOT
# (plan Global Constraints: "Tests must prove no writes reach GA_LEGACY_ROOT").
LEGACY_ROOT_WATCHED_FILES = (
    "agentmain.py",
    "ga.py",
    "llmcore.py",
    "agent_loop.py",
    "simphtml.py",
    "plugins/__init__.py",
    "plugins/hooks.py",
    "assets/tools_schema.json",
    "assets/tools_schema_cn.json",
    "assets/sys_prompt.txt",
    "assets/sys_prompt_en.txt",
    "assets/global_mem_insight_template.txt",
    "assets/global_mem_insight_template_en.txt",
    "assets/insight_fixed_structure.txt",
    "assets/insight_fixed_structure_en.txt",
    "assets/code_run_header.py",
)


def _snapshot_legacy_root() -> dict[str, str]:
    """Return {relative_path: sha256_hex} for every watched legacy file."""
    snap: dict[str, str] = {}
    for rel in LEGACY_ROOT_WATCHED_FILES:
        path = REPO_ROOT / rel
        if not path.is_file():
            # Missing legacy file is itself a regression; record empty so the
            # post-check fails loudly instead of silently passing.
            snap[rel] = "MISSING"
            continue
        snap[rel] = hashlib.sha256(path.read_bytes()).hexdigest()
    return snap


def _assert_legacy_root_unchanged(before: dict[str, str]) -> None:
    after = _snapshot_legacy_root()
    changed = [rel for rel in before if before[rel] != after.get(rel)]
    assert not changed, (
        "legacy root files were modified during the test; GA_LEGACY_ROOT must "
        f"be read-only: {changed}"
    )


class _OAIHandler(BaseHTTPRequestHandler):
    server: "FixtureServer"

    def log_message(self, fmt, *args):  # quiet
        return

    def do_POST(self):
        length = int(self.headers.get("Content-Length") or 0)
        body = self.rfile.read(length)
        auth = self.headers.get("Authorization") or ""
        if auth != f"Bearer {TEST_TOKEN}":
            self.send_response(401)
            self.end_headers()
            self.wfile.write(b'{"error":"bad token"}')
            return
        self.server.seen_auth.append(auth)
        try:
            payload = json.loads(body.decode("utf-8"))
        except Exception:
            payload = {}
        self.server.requests.append(payload)
        # Signal request arrival for deterministic mid-run cancel.
        self.server.request_arrived.set()
        # Hold response until released (or timeout safety).
        if not self.server.release_response.wait(timeout=self.server.hold_timeout):
            # Safety: never hang the whole suite forever.
            pass
        content = self.server.response_text
        data = {
            "id": "chatcmpl-test",
            "object": "chat.completion",
            "choices": [
                {
                    "index": 0,
                    "message": {"role": "assistant", "content": content},
                    "finish_reason": "stop",
                }
            ],
            "usage": {"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2},
        }
        raw = json.dumps(data).encode("utf-8")
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(raw)))
        self.end_headers()
        self.wfile.write(raw)


class FixtureServer(ThreadingHTTPServer):
    def __init__(self, addr):
        super().__init__(addr, _OAIHandler)
        self.seen_auth: list[str] = []
        self.requests: list[dict] = []
        self.response_text = "fixture-reply-ok"
        self.request_arrived = threading.Event()
        self.release_response = threading.Event()
        self.release_response.set()  # default: do not block
        self.hold_timeout = 30.0

    def arm_hold(self):
        self.request_arrived.clear()
        self.release_response.clear()

    def release(self):
        self.release_response.set()

@pytest.fixture(scope="module")
def oai_fixture():
    srv = FixtureServer(("127.0.0.1", 0))
    thread = threading.Thread(target=srv.serve_forever, daemon=True)
    thread.start()
    host, port = srv.server_address
    base = f"http://{host}:{port}/v1"
    yield srv, base
    srv.shutdown()
    thread.join(timeout=2)


def _write_mykey(config_root: Path, apibase: str, generation: int = 1) -> str:
    config_root.mkdir(parents=True, exist_ok=True)
    placeholder = "0" * 64
    document = {
        "_platform_runtime": {
            "credential_generation": generation,
            "config_checksum": placeholder,
            "routing_snapshot_id": f"integration-{generation}",
        },
        "platform_native_oai_provider_1_config": {
            "name": "provider-1",
            "apikey": TEST_TOKEN,
            "apibase": apibase,
            "model": "gpt-test",
            "api_mode": "chat_completions",
            "stream": False,
            "read_timeout": 30,
        },
    }
    canonical = json.dumps(document, ensure_ascii=False, separators=(",", ":"), sort_keys=True) + "\n"
    checksum = hashlib.sha256(canonical.encode("utf-8")).hexdigest()
    (config_root / "mykey.runtime.json").write_bytes(canonical.replace(placeholder, checksum, 1).encode("utf-8"))
    (config_root / "mykey.py").write_text(
        "import json as _json\n"
        "from pathlib import Path as _Path\n"
        "_config = _json.loads(_Path(__file__).with_name(\"mykey.runtime.json\").read_text(encoding=\"utf-8\"))\n"
        "globals().update(_config)\n"
        "del _config\n",
        encoding="utf-8",
    )
    return checksum


def _start_worker(config_root: Path, runtime_root: Path) -> tuple[subprocess.Popen, str]:
    env = os.environ.copy()
    env["GA_CONFIG_ROOT"] = str(config_root)
    env["GA_LEGACY_ROOT"] = str(REPO_ROOT)
    env["GA_RUNTIME_DIR"] = str(runtime_root)
    env["GA_POLICY_FILE"] = str(POLICY_PATH)
    env["GA_WORKER_LISTEN"] = "127.0.0.1:0"
    # Integration pre-start cancel barrier directory (file protocol).
    env["GA_TEST_PRE_DISPATCH_BARRIER_DIR"] = str(runtime_root / "barriers")
    env["PYTHONPATH"] = str(WORKER_ROOT / "src") + os.pathsep + env.get("PYTHONPATH", "")
    env.pop("OPENAI_API_KEY", None)
    env.pop("ANTHROPIC_API_KEY", None)
    proc = subprocess.Popen(
        [str(PYTHON), "-m", "ga_worker.entrypoint", "--listen", "127.0.0.1:0", "--grace-seconds", "3"],
        cwd=str(WORKER_ROOT),
        env=env,
        stdout=subprocess.PIPE,
        stderr=subprocess.STDOUT,
        text=True,
        encoding="utf-8",
        errors="replace",
    )
    listen = None
    deadline = time.time() + 20
    buf = []
    assert proc.stdout is not None
    while time.time() < deadline:
        line = proc.stdout.readline()
        if not line and proc.poll() is not None:
            break
        if line:
            buf.append(line)
            m = re.search(r"WORKER_LISTEN=(\S+)", line)
            if m:
                listen = m.group(1)
                break
    if not listen:
        proc.kill()
        out = "".join(buf)
        remaining = proc.stdout.read() if proc.stdout else ""
        raise AssertionError(
            "worker failed to publish WORKER_LISTEN; deps/fixture missing?\n"
            f"output:\n{out}{remaining}"
        )
    return proc, listen


def _channel(listen: str):
    return grpc.insecure_channel(listen)


def _runtime_policy(**overrides):
    base = dict(
        max_turns=6,
        max_history_bytes=256 * 1024,
        max_working_bytes=64 * 1024,
        max_output_bytes=256 * 1024,
        task_timeout_seconds=60,
        capability_version="foundation.v1",
        policy_digest=FOUNDATION_DIGEST,
    )
    base.update(overrides)
    return worker_pb2.RuntimePolicy(**base)


def _collect(stub, task_id: str, prompt: str, persona: list[str] | None = None, session_key: str = "personal:1"):
    events = list(
        stub.ExecuteTask(
            worker_pb2.ExecuteTaskRequest(
                task=worker_pb2.TaskEnvelope(
                    task_id=task_id,
                    session_key=session_key,
                    requester_user_id=1,
                    source="integration",
                    source_instance_id="itest",
                    message_id=f"m-{task_id}",
                    prompt=prompt,
                    persona_snapshot=list(persona or []),
                    tool_policy_version="foundation.no-host-tools.v1",
                )
            )
        )
    )
    return events


def _terminals(events):
    return [e.terminal for e in events if e.WhichOneof("payload") == "terminal"]


def _assert_one_terminal(events):
    terms = _terminals(events)
    assert len(terms) == 1, events
    assert all(e.WhichOneof("payload") != "tool_progress" for e in events)
    return terms[0]


def test_worker_rpc_smoke(tmp_path, oai_fixture):
    srv, apibase = oai_fixture
    config_root = tmp_path / "config"
    runtime_root = tmp_path / "runtime"
    runtime_root.mkdir()
    _write_mykey(config_root, apibase)

    # Sentinel under legacy root must remain unchanged.
    sentinel = REPO_ROOT / ".worker_itest_sentinel_do_not_commit"
    sentinel.write_text("sentinel\n", encoding="utf-8")
    sentinel_bytes = sentinel.read_bytes()
    # Stronger guarantee: sha256-snapshot every legacy file the overlay is
    # allowed to copy, so any write-back (not just the sentinel) is caught.
    legacy_before = _snapshot_legacy_root()
    try:
        # Registry load sanity.
        reg = CapabilityRegistry.load(POLICY_PATH)
        assert reg.digest == FOUNDATION_DIGEST
        assert reg.resolve("foundation.v1", "foundation.no-host-tools.v1").allowed_tools == frozenset(
            {"update_working_checkpoint"}
        )

        proc, listen = _start_worker(config_root, runtime_root)
        try:
            with _channel(listen) as ch:
                stub = worker_pb2_grpc.WorkerServiceStub(ch)

                # Health before session.
                h0 = stub.Health(worker_pb2.HealthRequest())
                assert h0.ready is False

                # Bad policy digest rejected.
                with pytest.raises(grpc.RpcError) as ei:
                    stub.StartSession(
                        worker_pb2.StartSessionRequest(
                            session_key="personal:1",
                            runtime_policy=_runtime_policy(policy_digest="sha256:" + ("b" * 64)),
                        )
                    )
                assert "POLICY_DIGEST" in ei.value.details() or ei.value.code() == grpc.StatusCode.FAILED_PRECONDITION

                # Empty snapshot start.
                s1 = stub.StartSession(
                    worker_pb2.StartSessionRequest(
                        session_key="personal:1",
                        runtime_policy=_runtime_policy(),
                    )
                )
                assert s1.session_key == "personal:1"
                assert s1.worker_instance_id

                checksum2 = _write_mykey(config_root, apibase, generation=2)
                reloaded = stub.ReloadCredentials(worker_pb2.ReloadCredentialsRequest(
                    credential_generation=2,
                    config_checksum=checksum2,
                ))
                assert reloaded.credential_generation == 2
                assert reloaded.config_checksum == checksum2

                h1 = stub.Health(worker_pb2.HealthRequest())
                assert h1.ready is True
                assert h1.session_key == "personal:1"

                # Idempotent start.
                s2 = stub.StartSession(
                    worker_pb2.StartSessionRequest(
                        session_key="personal:1",
                        runtime_policy=_runtime_policy(),
                    )
                )
                assert s2.worker_instance_id == s1.worker_instance_id

                # Task with persona A.
                srv.response_text = "reply-persona-A"
                events_a = _collect(stub, "t-a", "Say hello A", persona=["You are persona A."])
                term_a = _assert_one_terminal(events_a)
                assert term_a.status == worker_pb2.TASK_SUCCEEDED
                assert "persona-A" in term_a.user_message or "reply-persona-A" in term_a.user_message
                # Plan Task 3 Step 5: result_digest is sha256 over UTF-8 bytes of
                # result.body. Verify both presence and exact byte-sequence match
                # (previously a single `== ... or term_a.result_digest` assertion
                # was short-circuited by the truthy digest and never compared).
                assert term_a.result_digest
                assert term_a.result_digest == result_digest_for(term_a.user_message)

                # Checkpoint for task A.
                staging = runtime_root / "staging" / "t-a.bundle.json"
                staging.parent.mkdir(parents=True, exist_ok=True)
                ready = stub.BeginCheckpoint(
                    worker_pb2.BeginCheckpointRequest(
                        task_id="t-a",
                        checkpoint_token="tok-a",
                        staging_ref=str(staging),
                        max_bundle_bytes=2 * 1024 * 1024,
                    )
                )
                assert ready.task_id == "t-a"
                assert ready.checkpoint_token == "tok-a"
                assert staging.is_file()
                raw = staging.read_bytes()
                assert ready.checksum == "sha256:" + hashlib.sha256(raw).hexdigest()
                bundle = json.loads(raw.decode("utf-8"))
                assert bundle["schema_version"] == SNAPSHOT_SCHEMA_VERSION
                assert bundle["result_digest"] == ready.result_digest
                assert result_digest_for(bundle["result"]["body"]) == ready.result_digest

                # Second persona task.
                srv.response_text = "reply-persona-B"
                events_b = _collect(stub, "t-b", "Say hello B", persona=["You are persona B."])
                term_b = _assert_one_terminal(events_b)
                assert term_b.status == worker_pb2.TASK_SUCCEEDED

                # Mid-run cancel: hold fixture until request arrives, cancel, then release.
                srv.response_text = "slow-reply-should-not-win"
                srv.arm_hold()
                box: list = []
                err_box: list = []

                def run_slow():
                    try:
                        box.append(_collect(stub, "t-slow", "slow please"))
                    except Exception as exc:
                        err_box.append(exc)

                th = threading.Thread(target=run_slow, daemon=True)
                th.start()
                assert srv.request_arrived.wait(10.0), "fixture never received LLM request"
                cancel_mid = stub.CancelTask(worker_pb2.CancelTaskRequest(task_id="t-slow"))
                assert cancel_mid.accepted is True
                time.sleep(0.15)
                srv.release()
                th.join(timeout=20)
                assert not err_box, err_box
                assert box
                term_slow = _assert_one_terminal(box[0])
                assert term_slow.status in (
                    worker_pb2.TASK_CANCELLED,
                    worker_pb2.TASK_INTERRUPTED,
                ), f"accepted mid-run cancel must not succeed: {term_slow.status}"
                # True pre-start cancel: opt-in barrier file before put_task.
                barrier_dir = runtime_root / "barriers"
                barrier_dir.mkdir(parents=True, exist_ok=True)
                wait_flag = barrier_dir / "t-pre.wait"
                reserved_flag = barrier_dir / "t-pre.reserved"
                proceed_flag = barrier_dir / "t-pre.proceed"
                for p in (wait_flag, reserved_flag, proceed_flag):
                    if p.exists():
                        p.unlink()
                wait_flag.write_text("1", encoding="utf-8")
                pre_box: list = []
                pre_err: list = []

                def run_pre():
                    try:
                        pre_box.append(_collect(stub, "t-pre", "should-not-reach-model"))
                    except Exception as exc:
                        pre_err.append(exc)

                th_pre = threading.Thread(target=run_pre, daemon=True)
                th_pre.start()
                deadline = time.time() + 10
                while time.time() < deadline and not reserved_flag.exists():
                    time.sleep(0.05)
                assert reserved_flag.exists(), "worker never reserved pre-start task"
                pre_req_count = len(srv.requests)
                cancel_pre = stub.CancelTask(worker_pb2.CancelTaskRequest(task_id="t-pre"))
                assert cancel_pre.accepted is True
                proceed_flag.write_text("1", encoding="utf-8")
                th_pre.join(timeout=15)
                assert not pre_err, pre_err
                assert pre_box
                term_pre = _assert_one_terminal(pre_box[0])
                assert term_pre.status == worker_pb2.TASK_CANCELLED
                assert len(srv.requests) == pre_req_count
                c_unknown = stub.CancelTask(worker_pb2.CancelTaskRequest(task_id="no-such"))
                assert c_unknown.accepted is False

                shut = stub.Shutdown(worker_pb2.ShutdownRequest(reason="swap"))
                assert shut.accepted is True
        finally:
            proc.terminate()
            try:
                proc.wait(timeout=5)
            except subprocess.TimeoutExpired:
                proc.kill()
            leftover = ""
            if proc.stdout:
                leftover = proc.stdout.read()
            combined = leftover
            assert "sk-" not in combined
            assert "sk-ant-" not in combined
            assert TEST_TOKEN not in combined

        # Restart worker for snapshot restore + policy limit paths.
        proc2, listen2 = _start_worker(config_root, runtime_root)
        try:
            with _channel(listen2) as ch:
                stub = worker_pb2_grpc.WorkerServiceStub(ch)

                # Committed snapshot restore.
                snap_bundle = build_snapshot_bundle(
                    task_id="prev",
                    session_key="personal:2",
                    backend_history=[{"role": "user", "content": "restored-user"}],
                    agent_history=["[USER]: restored"],
                    working={"seed_key": "seed_val"},
                    display_history=[{"text": "old-display", "turn": 1}],
                    result_body="old-result",
                    max_history_bytes=256 * 1024,
                    max_working_bytes=64 * 1024,
                )
                snap_path = runtime_root / "snap-restore.json"
                snap_raw = json.dumps(snap_bundle, ensure_ascii=False, separators=(",", ":"), sort_keys=True).encode(
                    "utf-8"
                )
                snap_path.write_bytes(snap_raw)
                checksum = "sha256:" + hashlib.sha256(snap_raw).hexdigest()

                # Bad checksum.
                with pytest.raises(grpc.RpcError):
                    stub.StartSession(
                        worker_pb2.StartSessionRequest(
                            session_key="personal:2",
                            snapshot_ref=str(snap_path),
                            snapshot_id="snap-1",
                            snapshot_checksum="sha256:" + ("0" * 64),
                            runtime_policy=_runtime_policy(),
                        )
                    )

                s_ok = stub.StartSession(
                    worker_pb2.StartSessionRequest(
                        session_key="personal:2",
                        snapshot_ref=str(snap_path),
                        snapshot_id="snap-1",
                        snapshot_checksum=checksum,
                        runtime_policy=_runtime_policy(),
                    )
                )
                assert s_ok.session_key == "personal:2"

                srv.response_text = "after-restore"
                events_r = _collect(
                    stub, "t-restore", "continue", persona=["restore-persona"], session_key="personal:2"
                )
                term_r = _assert_one_terminal(events_r)
                assert term_r.status == worker_pb2.TASK_SUCCEEDED
                # Restored display history must not appear as live chunks.
                for e in events_r:
                    if e.WhichOneof("payload") == "chunk":
                        assert e.chunk.text != "old-display"

                # Graceful shutdown.
                shut2 = stub.Shutdown(worker_pb2.ShutdownRequest(reason="done"))
                assert shut2.accepted is True
        finally:
            proc2.terminate()
            try:
                proc2.wait(timeout=5)
            except subprocess.TimeoutExpired:
                proc2.kill()

        # Legacy root sentinel unchanged.
        assert sentinel.read_bytes() == sentinel_bytes
        # Every watched legacy file must be byte-identical to the pre-run snapshot.
        _assert_legacy_root_unchanged(legacy_before)
        # Fixture saw our test bearer token.
        assert any(a == f"Bearer {TEST_TOKEN}" for a in srv.seen_auth)
        # No real key material in fixture captures.
        for req in srv.requests:
            blob = json.dumps(req)
            assert "sk-ant-" not in blob
    finally:
        if sentinel.exists():
            sentinel.unlink()
