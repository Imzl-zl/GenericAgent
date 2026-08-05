"""Identity & capability fencing unit tests (方案 §7, 审查修复).

Covers:
- StartSession rejects requests that do not match the container-injected
  immutable identity (GA_WORKSPACE_KEY / GA_RUNNER_GENERATION).
- ExecuteTask rejects runner_generation == 0 / mismatched generations and
  capability_jti values outside the session's current credential set.
- Checkpoint staging files are written group-readable (0640) so the Platform
  (shared group) can consume them.
"""

from __future__ import annotations

import hashlib
import json
import os
import stat
import threading
from pathlib import Path
from typing import Any

import pytest

from genericagent.worker.v1 import worker_pb2
from ga_worker.checkpoint import build_snapshot_bundle, write_checkpoint_atomic
from ga_worker.limits import CapabilityRegistry
from ga_worker.managed_agent import ManagedAgentAdapter, WorkerAdapterError

REPO_ROOT = Path(__file__).resolve().parents[4]
POLICY_PATH = REPO_ROOT / "tenant_platform" / "contracts" / "policy" / "foundation.v1.json"
FOUNDATION_POLICY_DIGEST = "sha256:" + hashlib.sha256(POLICY_PATH.read_bytes()).hexdigest()

LEGACY_MODULES = ("agentmain.py", "ga.py", "llmcore.py", "agent_loop.py", "simphtml.py")
LEGACY_PLUGINS = ("plugins/__init__.py", "plugins/hooks.py", "plugins/project_mode.py")
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


@pytest.fixture
def roots(tmp_path: Path):
    config_root = tmp_path / "config"
    legacy_root = tmp_path / "legacy"
    runtime_root = tmp_path / "runtime"
    config_root.mkdir()
    legacy_root.mkdir()
    runtime_root.mkdir()
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
    for name in LEGACY_ASSETS:
        src = REPO_ROOT / name
        if src.exists():
            dest = legacy_root / name
            dest.parent.mkdir(parents=True, exist_ok=True)
            dest.write_bytes(src.read_bytes())
    (config_root / "mykey.py").write_text(
        "native_oai_config = {'name':'test','apikey':'test-token','apibase':'http://127.0.0.1:9/v1','model':'test','stream':False}\n",
        encoding="utf-8",
    )
    _write_runtime_config_with_jtis(config_root, 1, [])
    return {
        "config_root": config_root,
        "legacy_root": legacy_root,
        "runtime_root": runtime_root,
    }


@pytest.fixture
def foundation_registry() -> CapabilityRegistry:
    return CapabilityRegistry.load(POLICY_PATH)


def _write_runtime_config_with_jtis(config_root: Path, generation: int, jtis: list[str]) -> None:
    placeholder = "0" * 64
    document = {
        "_platform_runtime": {
            "credential_generation": generation,
            "config_checksum": placeholder,
            "routing_snapshot_id": f"snapshot-{generation}",
            "jtis": jtis,
        },
        "platform_native_oai_provider_1_config": {
            "name": "provider-1",
            "apikey": "token",
            "apibase": "http://127.0.0.1:9/v1",
            "model": "test",
        },
    }
    canonical = json.dumps(document, ensure_ascii=False, separators=(",", ":"), sort_keys=True) + "\n"
    calculated = hashlib.sha256(canonical.encode("utf-8")).hexdigest()
    encoded = canonical.replace(placeholder, calculated, 1)
    (config_root / "mykey.runtime.json").write_bytes(encoded.encode("utf-8"))


def _write_runtime_config_with_sophub(config_root: Path, generation: int, jtis: list[str], base_url: str, token: str) -> None:
    document = {
        "_platform_runtime": {
            "routing_snapshot_id": f"snapshot-{generation}",
            "jtis": jtis,
        },
        "platform_native_oai_provider_1_config": {
            "name": "provider-1",
            "apikey": "token",
            "apibase": "http://127.0.0.1:9/v1",
            "model": "test",
        },
        "_platform_sophub": {
            "base_url": base_url,
            "capability_token": token,
        },
    }
    (config_root / "mykey.runtime.json").write_text(
        json.dumps(document, ensure_ascii=False, separators=(",", ":"), sort_keys=True) + "\n",
        encoding="utf-8",
    )


def _make_adapter(roots, registry, factory=None) -> ManagedAgentAdapter:
    return ManagedAgentAdapter(
        config_root=roots["config_root"],
        legacy_root=roots["legacy_root"],
        runtime_root=roots["runtime_root"],
        registry=registry,
        agent_factory=factory,
    )


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
    workspace_key: str | None = None,
    runner_generation: int = 1,
) -> worker_pb2.StartSessionRequest:
    if workspace_key is None:
        workspace_key = session_key
    return worker_pb2.StartSessionRequest(
        session_key=session_key,
        workspace_key=workspace_key,
        runner_generation=runner_generation,
        runtime_policy=_runtime_policy(),
    )


def _task(
    task_id: str,
    prompt: str,
    *,
    session_key: str = "personal:1",
    tool_policy_version: str = "foundation.no-host-tools.v1",
    runner_generation: int = 1,
    capability_jti: str = "test-jti",
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
            persona_snapshot=[],
            tool_policy_version=tool_policy_version,
            runner_generation=runner_generation,
            capability_jti=capability_jti,
        )
    )


class _NoopAgent:
    """Minimal agent for execute_task identity checks (task never starts)."""

    instances: list["_NoopAgent"] = []

    def __init__(self):
        self.extra_sys_prompts: list[str] = []
        self.history: list[Any] = []
        self.handler: Any = None
        self._stop = threading.Event()
        _NoopAgent.instances.append(self)

    def run(self):
        # GA 主循环替身: 挂起直到测试结束, 避免 agent 线程崩溃置位
        # agent_failed 干扰 execute_task 的身份校验路径。
        self._stop.wait()


def _started_session_adapter(roots, registry, jtis: list[str], generation: int = 1):
    _write_runtime_config_with_jtis(roots["config_root"], 1, jtis)
    adapter = _make_adapter(roots, registry, factory=lambda: _NoopAgent())
    adapter.start_session(_start_req(runner_generation=generation))
    assert adapter._session is not None
    return adapter


def test_start_session_rejects_mismatched_container_workspace_key(roots, foundation_registry, monkeypatch):
    _write_runtime_config_with_jtis(roots["config_root"], 1, ["jti-1"])
    adapter = _make_adapter(roots, foundation_registry, factory=lambda: _NoopAgent())
    monkeypatch.setenv("GA_WORKSPACE_KEY", "personal:999")
    monkeypatch.setenv("GA_RUNNER_GENERATION", "1")
    with pytest.raises(WorkerAdapterError) as exc:
        adapter.start_session(_start_req(session_key="personal:1", runner_generation=1))
    assert exc.value.code == "WORKSPACE_KEY_MISMATCH"


def test_start_session_rejects_mismatched_container_generation(roots, foundation_registry, monkeypatch):
    _write_runtime_config_with_jtis(roots["config_root"], 1, ["jti-1"])
    adapter = _make_adapter(roots, foundation_registry, factory=lambda: _NoopAgent())
    monkeypatch.setenv("GA_WORKSPACE_KEY", "personal:1")
    monkeypatch.setenv("GA_RUNNER_GENERATION", "2")
    with pytest.raises(WorkerAdapterError) as exc:
        adapter.start_session(_start_req(session_key="personal:1", runner_generation=1))
    assert exc.value.code == "RUNNER_GENERATION_MISMATCH"


def test_start_session_matching_container_identity_succeeds(roots, foundation_registry, monkeypatch):
    _write_runtime_config_with_jtis(roots["config_root"], 1, ["jti-1"])
    adapter = _make_adapter(roots, foundation_registry, factory=lambda: _NoopAgent())
    monkeypatch.setenv("GA_WORKSPACE_KEY", "personal:1")
    monkeypatch.setenv("GA_RUNNER_GENERATION", "1")
    adapter.start_session(_start_req(session_key="personal:1", runner_generation=1))
    assert adapter._session is not None
    assert adapter._session.runner_generation == 1


def test_execute_task_rejects_zero_generation(roots, foundation_registry):
    adapter = _started_session_adapter(roots, foundation_registry, ["jti-1"])
    with pytest.raises(WorkerAdapterError) as exc:
        list(adapter.execute_task(_task("t0", "hi", runner_generation=0, capability_jti="jti-1")))
    assert exc.value.code == "RUNNER_GENERATION_MISMATCH"


def test_execute_task_rejects_stale_generation(roots, foundation_registry):
    adapter = _started_session_adapter(roots, foundation_registry, ["jti-1"])
    with pytest.raises(WorkerAdapterError) as exc:
        list(adapter.execute_task(_task("t1", "hi", runner_generation=2, capability_jti="jti-1")))
    assert exc.value.code == "RUNNER_GENERATION_MISMATCH"


def test_execute_task_rejects_jti_outside_credential_set(roots, foundation_registry):
    adapter = _started_session_adapter(roots, foundation_registry, ["jti-1"])
    with pytest.raises(WorkerAdapterError) as exc:
        list(adapter.execute_task(_task("t2", "hi", runner_generation=1, capability_jti="jti-evil")))
    assert exc.value.code == "CAPABILITY_JTI_MISMATCH"


def test_execute_task_accepts_jti_from_credential_set(roots, foundation_registry):
    adapter = _started_session_adapter(roots, foundation_registry, ["jti-1", "jti-2"])
    # 身份校验通过后才会进入运行路径; 若 identity 门槛未过会先抛
    # RUNNER_* / CAPABILITY_* 错误。_NoopAgent 缺少 put_task, 任务以
    # TASK_FAILED 终止而不是身份错误。
    events = list(adapter.execute_task(_task("t3", "hi", runner_generation=1, capability_jti="jti-2")))
    terminals = [e.terminal for e in events if e.WhichOneof("payload") == "terminal"]
    assert len(terminals) == 1, f"expected terminal, got {events!r}"


def test_checkpoint_staging_file_is_group_readable(tmp_path):
    bundle = build_snapshot_bundle(
        task_id="t1",
        session_key="personal:1",
        backend_history=[],
        agent_history=[],
        working={},
        display_history=[],
        result_body="ok",
        max_history_bytes=1024 * 1024,
        max_working_bytes=1024 * 1024,
        runner_generation=1,
    )
    staging = tmp_path / "staging" / "token.bundle.json"
    staging.parent.mkdir(parents=True)
    write_checkpoint_atomic(
        staging_ref=staging,
        bundle=bundle,
        max_bundle_bytes=1024 * 1024,
        token="tok",
    )
    mode = stat.S_IMODE(staging.stat().st_mode)
    if os.name == "nt":
        # Windows 无 POSIX 权限位语义(fchmod 仅影响只读位), 跳过。
        return
    assert mode & 0o004, f"staging bundle must be group-readable, got mode {oct(mode)}"
    assert not (mode & 0o002), f"staging bundle must not be group-writable, got mode {oct(mode)}"
