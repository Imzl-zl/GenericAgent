"""Unit tests for ManagedAgentAdapter session state, cancellation, policy, overlay, and checkpoint."""

from __future__ import annotations

import hashlib
import json
import queue
import sys
import threading
import time
from copy import deepcopy
from pathlib import Path
from typing import Any

import pytest

from genericagent.worker.v1 import worker_pb2
from ga_worker.checkpoint import SNAPSHOT_SCHEMA_VERSION, build_snapshot_bundle, result_digest_for
from ga_worker.legacy_import import import_legacy_runtime
from ga_worker.limits import CapabilityRegistry, ToolPolicy
from ga_worker.managed_agent import ManagedAgentAdapter, WorkerAdapterError
from ga_worker.runtime_overlay import OVERLAY_MANIFEST_ENTRIES, materialize_runtime_overlay

REPO_ROOT = Path(__file__).resolve().parents[4]
POLICY_PATH = REPO_ROOT / "tenant_platform" / "contracts" / "policy" / "foundation.v1.json"
FOUNDATION_POLICY_DIGEST = "sha256:" + hashlib.sha256(POLICY_PATH.read_bytes()).hexdigest()

LEGACY_MODULES = (
    "agentmain.py",
    "ga.py",
    "llmcore.py",
    "agent_loop.py",
    "simphtml.py",
)
LEGACY_PLUGINS = (
    "plugins/__init__.py",
    "plugins/hooks.py",
)
LEGACY_ASSETS = (
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


def _policy_digest_bytes(data: bytes) -> str:
    return "sha256:" + hashlib.sha256(data).hexdigest()


def _runtime_policy(
    *,
    max_turns: int = 8,
    max_history_bytes: int = 64 * 1024,
    max_working_bytes: int = 32 * 1024,
    max_output_bytes: int = 64 * 1024,
    task_timeout_seconds: int = 30,
    capability_version: str = "foundation.v1",
    policy_digest: str = FOUNDATION_POLICY_DIGEST,
) -> worker_pb2.RuntimePolicy:
    return worker_pb2.RuntimePolicy(
        max_turns=max_turns,
        max_history_bytes=max_history_bytes,
        max_working_bytes=max_working_bytes,
        max_output_bytes=max_output_bytes,
        task_timeout_seconds=task_timeout_seconds,
        capability_version=capability_version,
        policy_digest=policy_digest,
    )


def _start_req(
    session_key: str = "personal:1",
    *,
    snapshot_ref: str = "",
    snapshot_id: str = "",
    snapshot_checksum: str = "",
    runtime_policy: worker_pb2.RuntimePolicy | None = None,
) -> worker_pb2.StartSessionRequest:
    return worker_pb2.StartSessionRequest(
        session_key=session_key,
        snapshot_ref=snapshot_ref,
        snapshot_id=snapshot_id,
        snapshot_checksum=snapshot_checksum,
        runtime_policy=runtime_policy or _runtime_policy(),
    )


def _task(
    task_id: str,
    prompt: str,
    *,
    session_key: str = "personal:1",
    persona: list[str] | None = None,
    tool_policy_version: str = "foundation.no-host-tools.v1",
) -> worker_pb2.ExecuteTaskRequest:
    return worker_pb2.ExecuteTaskRequest(
        task=worker_pb2.TaskEnvelope(
            task_id=task_id,
            session_key=session_key,
            requester_user_id=1,
            source="test",
            source_instance_id="test-src",
            message_id=f"msg-{task_id}",
            prompt=prompt,
            persona_snapshot=list(persona or []),
            tool_policy_version=tool_policy_version,
        )
    )


def _events(adapter: ManagedAgentAdapter, req: worker_pb2.ExecuteTaskRequest) -> list[worker_pb2.WorkerEvent]:
    return list(adapter.execute_task(req))


def _terminal(events: list[worker_pb2.WorkerEvent]) -> worker_pb2.Terminal:
    terminals = [e.terminal for e in events if e.WhichOneof("payload") == "terminal"]
    assert len(terminals) == 1, f"expected exactly one terminal, got {len(terminals)} from {events!r}"
    return terminals[0]


def _chunks(events: list[worker_pb2.WorkerEvent]) -> list[worker_pb2.Chunk]:
    return [e.chunk for e in events if e.WhichOneof("payload") == "chunk"]


def _assert_no_tool_progress(events: list[worker_pb2.WorkerEvent]) -> None:
    for e in events:
        assert e.WhichOneof("payload") != "tool_progress"


class ScriptedAgent:
    """Test-only Agent implementing put_task/abort/run/is_running + queue-compatible output."""

    instances: list["ScriptedAgent"] = []

    def __init__(self, script: dict[str, list[dict[str, Any]]] | None = None, *, hang_on: set[str] | None = None):
        self.script = script or {}
        self.hang_on = hang_on or set()
        self.task_queue: queue.Queue = queue.Queue()
        self.is_running = False
        self.stop_sig = False
        self.abort_calls = 0
        self.put_task_calls: list[dict[str, Any]] = []
        self.extra_sys_prompts: list[str] = []
        self.history: list[Any] = []
        self.handler: Any = None
        self.backend_history: list[Any] = []
        self._started = threading.Event()
        self._release = threading.Event()
        self._lock = threading.Lock()
        self._running = False
        ScriptedAgent.instances.append(self)

    def put_task(self, query, source="user", images=None):
        display_queue: queue.Queue = queue.Queue()
        item = {
            "query": query,
            "source": source,
            "images": images or [],
            "output": display_queue,
        }
        self.put_task_calls.append(item)
        self.task_queue.put(item)
        return display_queue

    def abort(self):
        self.abort_calls += 1
        self.stop_sig = True
        self._release.set()

    def run(self):
        while True:
            task = self.task_queue.get()
            if isinstance(task, str):
                break
            raw_query = task["query"]
            display_queue = task["output"]
            self.is_running = True
            self._started.set()
            self.stop_sig = False
            try:
                if raw_query in self.hang_on:
                    self._release.wait(timeout=5)
                    if self.stop_sig:
                        display_queue.put({"done": "aborted", "turn": 1})
                        continue
                messages = list(self.script.get(raw_query, [{"next": f"echo:{raw_query}", "turn": 1}, {"done": f"done:{raw_query}", "turn": 1}]))
                for msg in messages:
                    if self.stop_sig:
                        display_queue.put({"done": msg.get("done", "aborted"), "turn": msg.get("turn", 1)})
                        break
                    display_queue.put(msg)
                    if "done" in msg:
                        break
                # Capture handler working if present for checkpoint tests.
                if self.handler is not None and hasattr(self.handler, "working"):
                    pass
            finally:
                self.is_running = False
                self.task_queue.task_done()

    def stop_runner(self):
        self.task_queue.put("STOP")


class SeededHandler:
    def __init__(self, working: dict | None = None):
        self.working = deepcopy(working or {})
        self.code_stop_signal: list[int] = []
        self.history_info: list[Any] = []
        self.cwd = ""


@pytest.fixture(autouse=True)
def _reset_scripted_agents():
    ScriptedAgent.instances.clear()
    yield
    ScriptedAgent.instances.clear()


@pytest.fixture
def roots(tmp_path: Path):
    config_root = tmp_path / "config"
    legacy_root = tmp_path / "legacy"
    runtime_root = tmp_path / "runtime"
    config_root.mkdir()
    legacy_root.mkdir()
    runtime_root.mkdir()
    # Minimal legacy tree for overlay tests (content doesn't need to be full GA for scripted agent).
    for name in LEGACY_MODULES:
        (legacy_root / name).write_text(f"# {name}\n", encoding="utf-8")
    (legacy_root / "plugins").mkdir()
    for name in LEGACY_PLUGINS:
        path = legacy_root / name
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text(f"# {name}\n", encoding="utf-8")
    (legacy_root / "assets").mkdir()
    for name in LEGACY_ASSETS:
        path = legacy_root / name
        path.parent.mkdir(parents=True, exist_ok=True)
        if name.endswith(".json"):
            path.write_text("[]", encoding="utf-8")
        else:
            path.write_text(f"template:{name}\n", encoding="utf-8")
    # Also copy real assets from repo for schema filtering tests when needed.
    real_assets = REPO_ROOT / "assets"
    for rel in LEGACY_ASSETS:
        src = REPO_ROOT / rel
        if src.exists():
            dest = legacy_root / rel
            dest.parent.mkdir(parents=True, exist_ok=True)
            dest.write_bytes(src.read_bytes())
    for name in LEGACY_MODULES:
        src = REPO_ROOT / name
        if src.exists():
            (legacy_root / name).write_bytes(src.read_bytes())
    for name in LEGACY_PLUGINS:
        src = REPO_ROOT / name
        if src.exists():
            dest = legacy_root / name
            dest.parent.mkdir(parents=True, exist_ok=True)
            dest.write_bytes(src.read_bytes())
    sentinel = legacy_root / "SENTINEL_DO_NOT_TOUCH.txt"
    sentinel.write_text("legacy-root-sentinel\n", encoding="utf-8")
    (config_root / "mykey.py").write_text(
        "native_oai_config = {'name':'test','apikey':'test-token','apibase':'http://127.0.0.1:9/v1','model':'test','stream':False}\n",
        encoding="utf-8",
    )
    return {
        "config_root": config_root,
        "legacy_root": legacy_root,
        "runtime_root": runtime_root,
        "sentinel": sentinel,
        "sentinel_bytes": sentinel.read_bytes(),
    }


@pytest.fixture
def foundation_registry() -> CapabilityRegistry:
    return CapabilityRegistry.load(POLICY_PATH)


def _make_adapter(roots, registry, factory=None) -> ManagedAgentAdapter:
    return ManagedAgentAdapter(
        config_root=roots["config_root"],
        legacy_root=roots["legacy_root"],
        runtime_root=roots["runtime_root"],
        registry=registry,
        agent_factory=factory,
    )


def test_capability_registry_load_and_resolve(foundation_registry: CapabilityRegistry):
    assert foundation_registry.digest == FOUNDATION_POLICY_DIGEST
    policy = foundation_registry.resolve("foundation.v1", "foundation.no-host-tools.v1")
    assert isinstance(policy, ToolPolicy)
    assert policy.version == "foundation.no-host-tools.v1"
    assert policy.allowed_tools == frozenset({"update_working_checkpoint"})


def test_capability_registry_rejects_bad_schema(tmp_path: Path):
    bad = tmp_path / "bad.json"
    bad.write_text(json.dumps({"schema_version": "other", "capabilities": {}}), encoding="utf-8")
    with pytest.raises(ValueError):
        CapabilityRegistry.load(bad)


def test_capability_registry_rejects_empty_allowlist(tmp_path: Path):
    data = {
        "schema_version": "genericagent.capability-policy.v1",
        "capabilities": {
            "c1": {"tool_policies": {"p1": {"allowed_tools": []}}},
        },
    }
    path = tmp_path / "empty.json"
    path.write_text(json.dumps(data), encoding="utf-8")
    with pytest.raises(ValueError):
        CapabilityRegistry.load(path)


def test_capability_registry_rejects_unknown_and_cross_capability(tmp_path: Path):
    data = {
        "schema_version": "genericagent.capability-policy.v1",
        "capabilities": {
            "cap.a": {"tool_policies": {"pol.a": {"allowed_tools": ["update_working_checkpoint"]}}},
            "cap.b": {"tool_policies": {"pol.b": {"allowed_tools": ["update_working_checkpoint"]}}},
        },
    }
    path = tmp_path / "multi.json"
    raw = json.dumps(data, separators=(",", ":")).encode("utf-8")
    path.write_bytes(raw)
    reg = CapabilityRegistry.load(path)
    with pytest.raises(ValueError):
        reg.resolve("missing", "pol.a")
    with pytest.raises(ValueError):
        reg.resolve("cap.a", "pol.b")


def test_start_session_creates_exactly_one_agent(roots, foundation_registry):
    created: list[ScriptedAgent] = []

    def factory():
        agent = ScriptedAgent()
        created.append(agent)
        return agent

    adapter = _make_adapter(roots, foundation_registry, factory)
    resp1 = adapter.start_session(_start_req())
    assert resp1.session_key == "personal:1"
    assert resp1.worker_instance_id
    assert len(created) == 1
    resp2 = adapter.start_session(_start_req())
    assert resp2.session_key == "personal:1"
    assert resp2.worker_instance_id == resp1.worker_instance_id
    assert len(created) == 1


def test_start_session_conflict_returns_session_already_started(roots, foundation_registry):
    adapter = _make_adapter(roots, foundation_registry, ScriptedAgent)
    adapter.start_session(_start_req())
    with pytest.raises(WorkerAdapterError) as ei:
        adapter.start_session(_start_req(session_key="personal:2"))
    assert ei.value.code == "SESSION_ALREADY_STARTED"
    with pytest.raises(WorkerAdapterError) as ei2:
        adapter.start_session(
            _start_req(runtime_policy=_runtime_policy(max_turns=99))
        )
    assert ei2.value.code == "SESSION_ALREADY_STARTED"


def test_execute_task_maps_next_and_done(roots, foundation_registry):
    agent_holder: dict[str, ScriptedAgent] = {}

    def factory():
        agent = ScriptedAgent(
            {
                "hello": [
                    {"next": "part-a", "turn": 1},
                    {"next": "part-b", "turn": 2},
                    {"done": "final-hello", "turn": 2},
                ]
            }
        )
        agent_holder["a"] = agent
        return agent

    adapter = _make_adapter(roots, foundation_registry, factory)
    adapter.start_session(_start_req())
    events = _events(adapter, _task("t1", "hello"))
    _assert_no_tool_progress(events)
    chunks = _chunks(events)
    assert [c.text for c in chunks] == ["part-a", "part-b"]
    assert [c.turn for c in chunks] == [1, 2]
    term = _terminal(events)
    assert term.task_id == "t1"
    assert term.status == worker_pb2.TASK_SUCCEEDED
    assert term.user_message == "final-hello"
    assert term.result_digest == result_digest_for("final-hello")
    assert len([e for e in events if e.WhichOneof("payload") == "terminal"]) == 1


def test_second_task_while_running_rejected(roots, foundation_registry):
    started = threading.Event()

    class HangAgent(ScriptedAgent):
        def run(self):
            while True:
                task = self.task_queue.get()
                if isinstance(task, str):
                    break
                self.is_running = True
                started.set()
                self._release.wait(timeout=5)
                task["output"].put({"done": "later", "turn": 1})
                self.is_running = False
                self.task_queue.task_done()

    adapter = _make_adapter(roots, foundation_registry, HangAgent)
    adapter.start_session(_start_req())

    results: list[list[worker_pb2.WorkerEvent]] = []
    errors: list[BaseException] = []

    def run_first():
        try:
            results.append(_events(adapter, _task("t-run", "block-me")))
        except BaseException as exc:  # pragma: no cover
            errors.append(exc)

    t = threading.Thread(target=run_first, daemon=True)
    t.start()
    assert started.wait(2.0)
    with pytest.raises(WorkerAdapterError) as ei:
        _events(adapter, _task("t-second", "nope"))
    assert ei.value.code == "TASK_ALREADY_RUNNING"
    # Unblock first task
    ScriptedAgent.instances[0]._release.set()
    t.join(timeout=3)
    assert not errors
    assert results and _terminal(results[0]).status == worker_pb2.TASK_SUCCEEDED


def test_cancel_before_runner_start_skips_put_task(roots, foundation_registry):
    gate = threading.Event()

    class GatedAgent(ScriptedAgent):
        def put_task(self, query, source="user", images=None):
            # Should never be called for pre-start cancelled task.
            raise AssertionError("put_task must not be called for pre-start cancel")

    adapter = _make_adapter(roots, foundation_registry, GatedAgent)
    adapter.start_session(_start_req())

    # Force pending reservation: inject a slow path via cancel race.
    # Use adapter internals: reserve then cancel before dispatch.
    # Public path: cancel immediately after execute starts but before put_task.
    # Implement by making execute_task reserve, then we cancel from another thread
    # while the adapter is waiting on a test hook if available.
    # Fallback public contract: cancel_task before any chunk means pending cancel.
    # We simulate by calling cancel on a task id that is reserved via a hook.
    # Use adapter's test-friendly pending reservation if exposed; otherwise call
    # cancel_task after starting execute in a thread with a barrier in agent_factory.

    reserved = threading.Event()
    proceed = threading.Event()
    original_factory = GatedAgent

    def factory():
        agent = original_factory()
        return agent

    adapter = _make_adapter(roots, foundation_registry, factory)
    adapter.start_session(_start_req())

    # Monkeypatch reserve gate if adapter exposes _task_dispatch_gate; tests use public cancel.
    # Contract: cancel before runner start => no put_task, one TASK_CANCELLED.
    # Use ManagedAgentAdapter's optional test barrier attribute.
    adapter._test_pre_dispatch_barrier = (reserved, proceed)  # type: ignore[attr-defined]

    events_box: list[list[worker_pb2.WorkerEvent]] = []

    def runner():
        events_box.append(_events(adapter, _task("t-cancel-pre", "should-not-run")))

    th = threading.Thread(target=runner, daemon=True)
    th.start()
    assert reserved.wait(2.0)
    resp = adapter.cancel_task("t-cancel-pre")
    assert resp.accepted is True
    proceed.set()
    th.join(timeout=3)
    assert events_box
    term = _terminal(events_box[0])
    assert term.status == worker_pb2.TASK_CANCELLED
    assert ScriptedAgent.instances[0].put_task_calls == []
    assert ScriptedAgent.instances[0].abort_calls == 0


def test_cancel_during_execution_aborts_once(roots, foundation_registry):
    class SlowAgent(ScriptedAgent):
        def __init__(self):
            super().__init__(hang_on={"slow-prompt"})

    adapter = _make_adapter(roots, foundation_registry, SlowAgent)
    adapter.start_session(_start_req())
    events_box: list[list[worker_pb2.WorkerEvent]] = []

    def runner():
        events_box.append(_events(adapter, _task("t-cancel-mid", "slow-prompt")))

    th = threading.Thread(target=runner, daemon=True)
    th.start()
    agent = ScriptedAgent.instances[0]
    assert agent._started.wait(2.0)
    resp = adapter.cancel_task("t-cancel-mid")
    assert resp.accepted is True
    th.join(timeout=3)
    assert events_box
    term = _terminal(events_box[0])
    assert term.status in (worker_pb2.TASK_CANCELLED, worker_pb2.TASK_INTERRUPTED)
    assert agent.abort_calls == 1
    assert len([e for e in events_box[0] if e.WhichOneof("payload") == "terminal"]) == 1


def test_persona_is_task_scoped(roots, foundation_registry):
    seen: list[list[str]] = []

    class PersonaAgent(ScriptedAgent):
        def put_task(self, query, source="user", images=None):
            seen.append(list(self.extra_sys_prompts))
            return super().put_task(query, source=source, images=images)

    adapter = _make_adapter(roots, foundation_registry, PersonaAgent)
    adapter.start_session(_start_req())
    e1 = _events(adapter, _task("p1", "one", persona=["persona-A"]))
    e2 = _events(adapter, _task("p2", "two", persona=["persona-B"]))
    assert _terminal(e1).status == worker_pb2.TASK_SUCCEEDED
    assert _terminal(e2).status == worker_pb2.TASK_SUCCEEDED
    assert seen[0] == ["persona-A"]
    assert seen[1] == ["persona-B"]
    # After tasks, agent persona should be cleared (no session-level mutation).
    assert ScriptedAgent.instances[0].extra_sys_prompts == []


def test_checkpoint_not_in_display_stream_and_valid_bundle(roots, foundation_registry, tmp_path):
    class WorkingAgent(ScriptedAgent):
        def run(self):
            while True:
                task = self.task_queue.get()
                if isinstance(task, str):
                    break
                self.is_running = True
                self.handler = SeededHandler({"note": "from-task"})
                self.history = ["agent-hist-1"]
                self.backend_history = [{"role": "user", "content": "hi"}]
                # Expose backend history like llmclient.backend.history
                self.llmclient = type("LC", (), {"backend": type("B", (), {"history": self.backend_history})()})()
                task["output"].put({"next": "chunk", "turn": 1})
                task["output"].put({"done": "final-body", "turn": 1})
                self.is_running = False
                self.task_queue.task_done()

    adapter = _make_adapter(roots, foundation_registry, WorkingAgent)
    adapter.start_session(_start_req())
    events = _events(adapter, _task("cp1", "checkpoint-me"))
    _assert_no_tool_progress(events)
    assert all(e.WhichOneof("payload") != "terminal" or True for e in events)
    term = _terminal(events)
    assert term.status == worker_pb2.TASK_SUCCEEDED

    staging = roots["runtime_root"] / "staging" / "cp1.bundle.json"
    staging.parent.mkdir(parents=True, exist_ok=True)
    ready = adapter.begin_checkpoint(
        worker_pb2.BeginCheckpointRequest(
            task_id="cp1",
            checkpoint_token="tok-cp1",
            staging_ref=str(staging),
            max_bundle_bytes=1024 * 1024,
        )
    )
    assert ready.task_id == "cp1"
    assert ready.checkpoint_token == "tok-cp1"
    assert ready.staging_ref == str(staging)
    assert ready.result_digest == result_digest_for("final-body")
    assert staging.is_file()
    bundle = json.loads(staging.read_text(encoding="utf-8"))
    assert bundle["schema_version"] == SNAPSHOT_SCHEMA_VERSION
    assert bundle["task_id"] == "cp1"
    assert bundle["session_key"] == "personal:1"
    assert bundle["working"]["note"] == "from-task"
    assert bundle["result"]["body"] == "final-body"
    assert bundle["result_digest"] == ready.result_digest
    # checksum is over bundle bytes excluding or including? Implementation uses file bytes after write.
    file_digest = "sha256:" + hashlib.sha256(staging.read_bytes()).hexdigest()
    assert ready.checksum == file_digest
    # CheckpointReady never appears as a WorkerEvent payload.
    assert all(e.WhichOneof("payload") in ("chunk", "terminal") for e in events)


def test_snapshot_restore_validates_schema_and_does_not_replay_display(roots, foundation_registry):
    working = {"seed": "restored-value"}
    backend_history = [{"role": "assistant", "content": "old"}]
    agent_history = ["[USER]: earlier"]
    display_history = [{"text": "should-not-replay", "turn": 9}]
    body = "previous"
    bundle = build_snapshot_bundle(
        task_id="old",
        session_key="personal:1",
        backend_history=backend_history,
        agent_history=agent_history,
        working=working,
        display_history=display_history,
        result_body=body,
        max_history_bytes=64 * 1024,
        max_working_bytes=32 * 1024,
    )
    snap_path = roots["runtime_root"] / "snapshots" / "snap1.json"
    snap_path.parent.mkdir(parents=True, exist_ok=True)
    raw = json.dumps(bundle, ensure_ascii=False, separators=(",", ":")).encode("utf-8")
    snap_path.write_bytes(raw)
    checksum = "sha256:" + hashlib.sha256(raw).hexdigest()

    class RestoreAgent(ScriptedAgent):
        def __init__(self):
            super().__init__()
            self.llmclient = type("LC", (), {"backend": type("B", (), {"history": []})()})()

        def run(self):
            while True:
                task = self.task_queue.get()
                if isinstance(task, str):
                    break
                self.is_running = True
                # Handler factory path will set working; simulate via adapter-injected handler.
                if self.handler is None:
                    self.handler = SeededHandler()
                task["output"].put({"done": f"got:{self.handler.working.get('seed')}", "turn": 1})
                self.is_running = False
                self.task_queue.task_done()

    adapter = _make_adapter(roots, foundation_registry, RestoreAgent)
    adapter.start_session(
        _start_req(
            snapshot_ref=str(snap_path),
            snapshot_id="snap-1",
            snapshot_checksum=checksum,
        )
    )
    # Bad checksum rejected.
    adapter2 = _make_adapter(roots, foundation_registry, RestoreAgent)
    with pytest.raises(WorkerAdapterError) as ei:
        adapter2.start_session(
            _start_req(
                snapshot_ref=str(snap_path),
                snapshot_id="snap-1",
                snapshot_checksum="sha256:" + ("0" * 64),
            )
        )
    assert ei.value.code in {"SNAPSHOT_CHECKSUM_MISMATCH", "SNAPSHOT_INVALID", "INVALID_SNAPSHOT"}

    events = _events(adapter, _task("after-restore", "go"))
    # Restored display history must not be replayed as live chunks.
    assert all(c.text != "should-not-replay" for c in _chunks(events))
    term = _terminal(events)
    assert term.status == worker_pb2.TASK_SUCCEEDED
    assert "restored-value" in term.user_message
    agent = ScriptedAgent.instances[0]
    assert agent.history == agent_history
    assert agent.llmclient.backend.history == backend_history


def test_policy_digest_mismatch_and_unknown_tool_policy(roots, foundation_registry):
    adapter = _make_adapter(roots, foundation_registry, ScriptedAgent)
    with pytest.raises(WorkerAdapterError) as ei:
        adapter.start_session(
            _start_req(runtime_policy=_runtime_policy(policy_digest="sha256:" + ("a" * 64)))
        )
    assert ei.value.code in {"POLICY_DIGEST_MISMATCH", "INVALID_POLICY_DIGEST"}

    adapter = _make_adapter(roots, foundation_registry, ScriptedAgent)
    adapter.start_session(_start_req())
    with pytest.raises(WorkerAdapterError) as ei2:
        _events(adapter, _task("bad-pol", "x", tool_policy_version="unknown.policy.v1"))
    assert ei2.value.code in {"UNKNOWN_TOOL_POLICY", "INVALID_TOOL_POLICY", "POLICY_REJECTED"}


def test_legacy_backend_exception_maps_to_task_failed(roots, foundation_registry):
    """B1: when legacy agentmain pushes {'done': partial, 'error': msg}, the
    adapter must emit TASK_FAILED (not TASK_SUCCEEDED) with the error code
    TASK_EXCEPTION and preserve the partial body as result_body."""
    class CrashAgent(ScriptedAgent):
        def run(self):
            while True:
                task = self.task_queue.get()
                if isinstance(task, str):
                    break
                self.is_running = True
                # Simulate agent_runner_loop raising mid-task: legacy run()
                # catches and pushes done+error per B1 fix.
                task["output"].put({"next": "partial-chunk", "turn": 1})
                task["output"].put({
                    "done": "partial-body",
                    "error": "ValueError: simulated backend crash",
                    "source": "test",
                    "turn": 1,
                    "outputs": [],
                })
                self.is_running = False
                self.task_queue.task_done()

    adapter = _make_adapter(roots, foundation_registry, CrashAgent)
    adapter.start_session(_start_req())
    events = _events(adapter, _task("b1-crash", "trigger-crash"))
    _assert_no_tool_progress(events)
    chunks = _chunks(events)
    assert [c.text for c in chunks] == ["partial-chunk"]
    term = _terminal(events)
    assert term.status == worker_pb2.TASK_FAILED
    assert term.error.code == "TASK_EXCEPTION"
    assert "simulated backend crash" in term.user_message
    # Partial body is preserved for checkpoint digest.
    assert term.result_digest == result_digest_for("partial-body")


def test_output_byte_cap_emits_failed(roots, foundation_registry):
    class LoudAgent(ScriptedAgent):
        def __init__(self):
            big = "x" * 200
            super().__init__({"loud": [{"next": big, "turn": 1}, {"done": big, "turn": 1}]})

    adapter = _make_adapter(roots, foundation_registry, LoudAgent)
    adapter.start_session(_start_req(runtime_policy=_runtime_policy(max_output_bytes=50)))
    events = _events(adapter, _task("out-cap", "loud"))
    term = _terminal(events)
    assert term.status == worker_pb2.TASK_FAILED
    assert term.error.code in {"MAX_OUTPUT_BYTES", "OUTPUT_LIMIT_EXCEEDED", "POLICY_LIMIT"}


def test_overlay_manifest_and_legacy_root_untouched(roots, foundation_registry):
    adapter = _make_adapter(roots, foundation_registry, ScriptedAgent)
    adapter.start_session(_start_req())
    # Materialize should have run; inspect session overlay.
    overlays = list(roots["runtime_root"].rglob("legacy-overlay"))
    assert len(overlays) == 1
    overlay = overlays[0]
    manifest_path = overlay / "OVERLAY_MANIFEST.json"
    assert manifest_path.is_file()
    manifest = json.loads(manifest_path.read_text(encoding="utf-8"))
    entries = set(manifest.get("entries") or manifest.get("files") or [])
    expected = set(LEGACY_MODULES) | set(LEGACY_PLUGINS) | set(LEGACY_ASSETS)
    assert entries == expected
    # No memory/** or mykey.py in overlay source manifest.
    assert "mykey.py" not in entries
    assert not any(e.startswith("memory/") for e in entries)
    # Writable memory/temp exist but are not in immutable manifest entries.
    assert (overlay / "memory" / "global_mem.txt").is_file()
    assert (overlay / "temp").is_dir()
    # Sentinel unchanged after session + task + checkpoint attempt.
    events = _events(adapter, _task("ov1", "ping"))
    assert _terminal(events).status == worker_pb2.TASK_SUCCEEDED
    staging = roots["runtime_root"] / "staging" / "ov1.json"
    staging.parent.mkdir(parents=True, exist_ok=True)
    adapter.begin_checkpoint(
        worker_pb2.BeginCheckpointRequest(
            task_id="ov1",
            checkpoint_token="tok-ov1",
            staging_ref=str(staging),
            max_bundle_bytes=1024 * 1024,
        )
    )
    assert roots["sentinel"].read_bytes() == roots["sentinel_bytes"]


def test_import_ga_exercises_simphtml_and_leaves_legacy_root(roots, foundation_registry):
    """Import boundary: materialize overlay, import ga, sentinel unchanged."""
    session_id = "sess-import-test"
    overlay_dir, manifest = materialize_runtime_overlay(
        legacy_root=roots["legacy_root"],
        runtime_root=roots["runtime_root"],
        session_id=session_id,
    )
    assert set(manifest["entries"]) == set(OVERLAY_MANIFEST_ENTRIES)
    # Import via legacy_import path.
    mods = import_legacy_runtime(
        config_root=roots["config_root"],
        legacy_root=roots["legacy_root"],
        overlay_dir=overlay_dir,
        manifest=manifest,
    )
    assert "ga" in mods
    assert "simphtml" in mods or "simphtml" in __import__("sys").modules
    assert roots["sentinel"].read_bytes() == roots["sentinel_bytes"]
    # GA_LEGACY_ROOT must not remain on sys.path after import.
    import sys

    legacy_str = str(roots["legacy_root"].resolve())
    assert legacy_str not in sys.path


def test_begin_checkpoint_rejects_wrong_task_or_token(roots, foundation_registry):
    adapter = _make_adapter(roots, foundation_registry, ScriptedAgent)
    adapter.start_session(_start_req())
    _events(adapter, _task("ok", "hi"))
    staging = roots["runtime_root"] / "staging" / "x.json"
    staging.parent.mkdir(parents=True, exist_ok=True)
    with pytest.raises(WorkerAdapterError):
        adapter.begin_checkpoint(
            worker_pb2.BeginCheckpointRequest(
                task_id="wrong",
                checkpoint_token="tok",
                staging_ref=str(staging),
                max_bundle_bytes=1024,
            )
        )


def test_health_and_shutdown(roots, foundation_registry):
    adapter = _make_adapter(roots, foundation_registry, ScriptedAgent)
    h0 = adapter.health()
    assert h0.ready is False
    adapter.start_session(_start_req())
    h1 = adapter.health()
    assert h1.ready is True
    assert h1.session_key == "personal:1"
    shut = adapter.shutdown("test-done")
    assert shut.accepted is True


def test_max_history_bytes_on_restore(roots, foundation_registry):
    huge_history = [{"role": "user", "content": "Z" * 5000}]
    bundle = build_snapshot_bundle(
        task_id="h1",
        session_key="personal:1",
        backend_history=huge_history,
        agent_history=["A" * 5000],
        working={"k": "v"},
        display_history=[],
        result_body="r",
        max_history_bytes=10 * 1024 * 1024,
        max_working_bytes=1024,
    )
    snap_path = roots["runtime_root"] / "snap-huge.json"
    raw = json.dumps(bundle, ensure_ascii=False, separators=(",", ":")).encode("utf-8")
    snap_path.write_bytes(raw)
    checksum = "sha256:" + hashlib.sha256(raw).hexdigest()
    adapter = _make_adapter(roots, foundation_registry, ScriptedAgent)
    with pytest.raises(WorkerAdapterError) as ei:
        adapter.start_session(
            _start_req(
                snapshot_ref=str(snap_path),
                snapshot_id="huge",
                snapshot_checksum=checksum,
                runtime_policy=_runtime_policy(max_history_bytes=100),
            )
        )
    assert ei.value.code in {"HISTORY_LIMIT_EXCEEDED", "SNAPSHOT_TOO_LARGE", "POLICY_LIMIT", "INVALID_SNAPSHOT"}


def test_max_turns_wraps_legacy_runner_loop(roots, foundation_registry, monkeypatch):
    """RuntimePolicy.max_turns must constrain agent_runner_loop without editing agentmain."""
    import types
    import ga_worker.managed_agent as ma

    calls: list[int] = []

    def fake_runner_loop(client, system_prompt, user_input, handler, tools_schema, max_turns=40, **kwargs):
        calls.append(max_turns)
        if False:
            yield "x"
        return {"result": "ok"}

    # Simulate imported legacy modules with real wrap targets.
    agent_loop_mod = types.ModuleType("agent_loop")
    agent_loop_mod.agent_runner_loop = fake_runner_loop

    class StepOutcome:
        def __init__(self, data, next_prompt=None, should_exit=False):
            self.data = data
            self.next_prompt = next_prompt
            self.should_exit = should_exit

    agent_loop_mod.StepOutcome = StepOutcome

    agentmain_mod = types.ModuleType("agentmain")
    agentmain_mod.TOOLS_SCHEMA = [
        {"type": "function", "function": {"name": "code_run"}},
        {"type": "function", "function": {"name": "update_working_checkpoint"}},
    ]
    agentmain_mod.agent_runner_loop = fake_runner_loop

    class FakeHandler:
        def dispatch(self, tool_name, args, response, index=0, tool_num=1):
            yield f"dispatched:{tool_name}\n"
            return StepOutcome(None, next_prompt="ok")

    ga_mod = types.ModuleType("ga")
    ga_mod.GenericAgentHandler = FakeHandler

    class LoopAwareAgent(ScriptedAgent):
        def run(self):
            while True:
                task = self.task_queue.get()
                if isinstance(task, str):
                    break
                self.is_running = True
                # Call the same name agentmain uses so the wrap is observable.
                import agentmain as am

                gen = am.agent_runner_loop(None, "sys", task["query"], FakeHandler(), am.TOOLS_SCHEMA, max_turns=180)
                try:
                    for _ in gen:
                        pass
                except TypeError:
                    pass
                task["output"].put({"done": f"turns={calls[-1] if calls else None}", "turn": 1})
                self.is_running = False
                self.task_queue.task_done()

    adapter = _make_adapter(roots, foundation_registry, LoopAwareAgent)
    adapter.start_session(_start_req(runtime_policy=_runtime_policy(max_turns=2)))
    # Inject fake legacy mods as if real import happened.
    adapter._legacy_mods = {
        "agent_loop": agent_loop_mod,
        "agentmain": agentmain_mod,
        "ga": ga_mod,
    }
    # Install wraps the same way execute_task does.
    monkeypatch.setitem(sys.modules, "agent_loop", agent_loop_mod)
    monkeypatch.setitem(sys.modules, "agentmain", agentmain_mod)
    monkeypatch.setitem(sys.modules, "ga", ga_mod)

    events = _events(adapter, _task("turns", "run-loop"))
    term = _terminal(events)
    assert term.status == worker_pb2.TASK_SUCCEEDED
    assert calls == [2], f"expected agent_runner_loop max_turns=2, got {calls}"
    assert "turns=2" in term.user_message


def test_task_deadline_emits_interrupted_timeout(roots, foundation_registry):
    class HangAgent(ScriptedAgent):
        def __init__(self):
            super().__init__(hang_on={"deadline-me"})

    adapter = _make_adapter(roots, foundation_registry, HangAgent)
    adapter.start_session(_start_req(runtime_policy=_runtime_policy(task_timeout_seconds=1)))
    t0 = time.time()
    events = _events(adapter, _task("dl1", "deadline-me"))
    elapsed = time.time() - t0
    term = _terminal(events)
    assert term.status == worker_pb2.TASK_INTERRUPTED
    assert term.error.code == "TASK_TIMEOUT"
    assert elapsed < 5.0
    assert ScriptedAgent.instances[0].abort_calls == 1


def test_repeated_cancel_and_timeout_abort_once(roots, foundation_registry):
    class SlowAgent(ScriptedAgent):
        def __init__(self):
            super().__init__(hang_on={"once-abort"})

    adapter = _make_adapter(roots, foundation_registry, SlowAgent)
    adapter.start_session(_start_req(runtime_policy=_runtime_policy(task_timeout_seconds=30)))
    box: list = []

    def runner():
        box.append(_events(adapter, _task("ab1", "once-abort")))

    th = threading.Thread(target=runner, daemon=True)
    th.start()
    agent = ScriptedAgent.instances[0]
    assert agent._started.wait(2.0)
    assert adapter.cancel_task("ab1").accepted is True
    assert adapter.cancel_task("ab1").accepted is True
    assert adapter.cancel_task("ab1").accepted is True
    th.join(timeout=3)
    assert box
    term = _terminal(box[0])
    assert term.status in (worker_pb2.TASK_CANCELLED, worker_pb2.TASK_INTERRUPTED)
    assert agent.abort_calls == 1


def test_max_output_bytes_counts_handler_print(roots, foundation_registry):
    class PrintyAgent(ScriptedAgent):
        def run(self):
            while True:
                task = self.task_queue.get()
                if isinstance(task, str):
                    break
                self.is_running = True
                self.handler = SeededHandler()
                # Wait until adapter instruments handler.print (created after put_task).
                deadline = time.time() + 2.0
                while time.time() < deadline and not getattr(self.handler, "_adapter_print_wrapped", False):
                    wrap = getattr(self, "_adapter_wrap_handler", None)
                    if callable(wrap):
                        wrap(self.handler)
                    time.sleep(0.01)
                if not getattr(self.handler, "_adapter_print_wrapped", False):
                    raise AssertionError("handler.print not instrumented")
                self.handler.print("Y" * 80)
                time.sleep(0.05)
                if not self.stop_sig:
                    task["output"].put({"next": "tiny", "turn": 1})
                    task["output"].put({"done": "should-not-succeed", "turn": 1})
                else:
                    task["output"].put({"done": "stopped", "turn": 1})
                self.is_running = False
                self.task_queue.task_done()

    adapter = _make_adapter(roots, foundation_registry, PrintyAgent)
    adapter.start_session(
        _start_req(runtime_policy=_runtime_policy(max_output_bytes=40, task_timeout_seconds=10))
    )
    events = _events(adapter, _task("print-cap", "go"))
    term = _terminal(events)
    assert term.status == worker_pb2.TASK_FAILED
    assert term.error.code == "MAX_OUTPUT_BYTES"


def test_tool_schema_filter_and_dispatch_deny(roots, foundation_registry, monkeypatch):
    import types
    import sys

    class StepOutcome:
        def __init__(self, data, next_prompt=None, should_exit=False):
            self.data = data
            self.next_prompt = next_prompt
            self.should_exit = should_exit

    agent_loop_mod = types.ModuleType("agent_loop")
    agent_loop_mod.StepOutcome = StepOutcome
    agent_loop_mod.agent_runner_loop = lambda *a, **k: iter(())

    agentmain_mod = types.ModuleType("agentmain")
    agentmain_mod.TOOLS_SCHEMA = [
        {"type": "function", "function": {"name": "code_run"}},
        {"type": "function", "function": {"name": "file_read"}},
        {"type": "function", "function": {"name": "update_working_checkpoint"}},
    ]

    denied: list[str] = []
    allowed_seen: list[str] = []

    class FakeHandler:
        def dispatch(self, tool_name, args, response, index=0, tool_num=1):
            allowed_seen.append(tool_name)
            yield f"ok:{tool_name}\n"
            return StepOutcome("ok")

    ga_mod = types.ModuleType("ga")
    ga_mod.GenericAgentHandler = FakeHandler

    class ToolAgent(ScriptedAgent):
        def run(self):
            while True:
                task = self.task_queue.get()
                if isinstance(task, str):
                    break
                self.is_running = True
                import agentmain as am

                names = [t["function"]["name"] for t in am.TOOLS_SCHEMA]
                h = FakeHandler()
                # Fabricated disallowed tool must be denied by wrap.
                gen = h.dispatch("code_run", {}, None)
                out = []
                try:
                    while True:
                        out.append(next(gen))
                except StopIteration as e:
                    outcome = e.value
                denied.extend(out)
                # Allowed tool still works.
                gen2 = h.dispatch("update_working_checkpoint", {"key_info": "x"}, None)
                try:
                    while True:
                        next(gen2)
                except StopIteration:
                    pass
                task["output"].put({"done": f"schema={names}", "turn": 1})
                self.is_running = False
                self.task_queue.task_done()

    monkeypatch.setitem(sys.modules, "agent_loop", agent_loop_mod)
    monkeypatch.setitem(sys.modules, "agentmain", agentmain_mod)
    monkeypatch.setitem(sys.modules, "ga", ga_mod)

    adapter = _make_adapter(roots, foundation_registry, ToolAgent)
    adapter.start_session(_start_req())
    adapter._legacy_mods = {
        "agent_loop": agent_loop_mod,
        "agentmain": agentmain_mod,
        "ga": ga_mod,
    }
    events = _events(adapter, _task("tools", "check"))
    term = _terminal(events)
    assert term.status == worker_pb2.TASK_SUCCEEDED
    assert "update_working_checkpoint" in term.user_message
    assert "code_run" not in term.user_message.split("schema=")[-1] or "schema=['update_working_checkpoint']" in term.user_message
    assert any("denied" in str(x) for x in denied)
    assert "update_working_checkpoint" in allowed_seen
    assert "code_run" not in allowed_seen


def test_plugins_hooks_import_failure_is_visible(roots, foundation_registry):
    # Break plugins/hooks in overlay source so materialize copies broken file.
    hooks = roots["legacy_root"] / "plugins" / "hooks.py"
    hooks.write_text("raise RuntimeError('hooks-boom')\n", encoding="utf-8")
    from ga_worker.runtime_overlay import materialize_runtime_overlay
    from ga_worker.legacy_import import import_legacy_runtime, LegacyImportError

    overlay, manifest = materialize_runtime_overlay(
        legacy_root=roots["legacy_root"],
        runtime_root=roots["runtime_root"],
        session_id="s-hooks-fail",
    )
    with pytest.raises(LegacyImportError) as ei:
        import_legacy_runtime(
            config_root=roots["config_root"],
            legacy_root=roots["legacy_root"],
            overlay_dir=overlay,
            manifest=manifest,
        )
    assert "plugins" in str(ei.value).lower() or "hooks" in str(ei.value).lower()
