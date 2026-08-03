"""ManagedAgentAdapter: isolated single-session Worker state machine around GenericAgent.

Public API only; internals split into:
- state.py: dataclasses (PendingTask, CompletedTask, SessionState, TaskRunState) + WorkerAdapterError
- legacy_instrument.py: monkeypatch helpers for legacy modules
- session_lifecycle.py: SessionLifecycleMixin (session creation, snapshot, checkpoint write)
- task_ops.py: TaskOpsMixin (terminal, abort, record_completed, cleanup)
- task_runner.py + task_drain.py + task_terminal.py: execute_task generator logic
"""

from __future__ import annotations

import hmac
import os
import threading
import uuid
from pathlib import Path
from typing import Any, Iterator

from genericagent.worker.v1 import worker_pb2

from ga_worker.credential_config import CredentialConfigError, load_runtime_metadata, validate_reload_request
from ga_worker.limits import CapabilityRegistry
from ga_worker.session_lifecycle import SessionLifecycleMixin
from ga_worker.state import (
    AgentFactory,
    PendingTask,
    SessionState,
    WorkerAdapterError,
)
from ga_worker.task_ops import TaskOpsMixin

# P-M4: shutdown joins the runner thread with a bounded timeout.
SHUTDOWN_JOIN_TIMEOUT_S = 5.0


class ManagedAgentAdapter(SessionLifecycleMixin, TaskOpsMixin):
    def __init__(
        self,
        *,
        config_root: Path,
        legacy_root: Path,
        runtime_root: Path,
        registry: CapabilityRegistry,
        agent_factory: AgentFactory | None = None,
        worker_instance_id: str | None = None,
    ):
        self.config_root = Path(config_root)
        self.legacy_root = Path(legacy_root)
        self.runtime_root = Path(runtime_root)
        self.registry = registry
        self.agent_factory = agent_factory
        self.worker_instance_id = worker_instance_id or f"worker-{uuid.uuid4().hex[:12]}"
        self._lock = threading.Lock()
        self._session: SessionState | None = None
        self._pending: PendingTask | None = None
        self._event_queues: dict[str, Any] = {}
        self._test_pre_dispatch_barrier: tuple[threading.Event, threading.Event] | None = None
        self._legacy_mods: dict[str, Any] | None = None

    # ── public API ──────────────────────────────────────────────────────────

    def health(self) -> worker_pb2.HealthResponse:
        with self._lock:
            if self._session is None:
                return worker_pb2.HealthResponse(
                    worker_instance_id=self.worker_instance_id,
                    session_key="",
                    ready=False,
                )
            return worker_pb2.HealthResponse(
                worker_instance_id=self._session.worker_instance_id,
                session_key=self._session.session_key,
                ready=not (self._session.shutting_down or self._session.agent_failed),
            )

    def start_session(self, request: worker_pb2.StartSessionRequest) -> worker_pb2.StartSessionResponse:
        self._validate_start_session_request(request)
        with self._lock:
            if self._session is not None:
                if self._session_matches(request):
                    return worker_pb2.StartSessionResponse(
                        session_key=self._session.session_key,
                        worker_instance_id=self._session.worker_instance_id,
                    )
                raise WorkerAdapterError(
                    "SESSION_ALREADY_STARTED",
                    "session already started with different immutable inputs",
                )
            return self._create_session(request)

    def reload_credentials(
        self, request: worker_pb2.ReloadCredentialsRequest,
    ) -> worker_pb2.ReloadCredentialsResponse:
        with self._lock:
            if self._session is None:
                raise WorkerAdapterError("SESSION_NOT_STARTED", "session not started")
            # 控制面身份 fencing(方案 §7, 审查): 迟到的 reload 请求必须绑定
            # 当前 workspace 与 Runner generation。
            self._assert_control_identity(request.workspace_key, request.runner_generation)
            if self._pending is not None or self._session.active_task_id:
                raise WorkerAdapterError("TASK_ACTIVE", "cannot reload credentials during a task")
            if request.credential_generation == self._session.credential_generation:
                if not hmac.compare_digest(request.config_checksum, self._session.credential_checksum):
                    raise WorkerAdapterError("CONFIG_CHECKSUM_MISMATCH", "CONFIG_CHECKSUM_MISMATCH")
                return worker_pb2.ReloadCredentialsResponse(
                    credential_generation=self._session.credential_generation,
                    config_checksum=self._session.credential_checksum,
                )
            if request.credential_generation < self._session.credential_generation:
                raise WorkerAdapterError("CREDENTIAL_GENERATION_STALE", "credential generation must increase")
            try:
                metadata = load_runtime_metadata(self.config_root)
                validate_reload_request(
                    metadata,
                    int(request.credential_generation),
                    request.config_checksum,
                )
            except CredentialConfigError as exc:
                code = "CONFIG_CHECKSUM_MISMATCH" if "CONFIG_CHECKSUM_MISMATCH" in str(exc) else "CREDENTIAL_CONFIG_ERROR"
                raise WorkerAdapterError(code, str(exc)) from exc
            agent = self._session.agent
            had_clients = hasattr(agent, "llmclients")
            previous_clients = getattr(agent, "llmclients", None)
            try:
                agent.load_llm_sessions()
            except Exception as exc:
                if had_clients:
                    agent.llmclients = previous_clients
                raise WorkerAdapterError("CREDENTIAL_RELOAD_FAILED", str(exc)) from exc
            llm_clients = getattr(agent, "llmclients", None)
            if isinstance(llm_clients, list) and not llm_clients:
                if had_clients:
                    agent.llmclients = previous_clients
                raise WorkerAdapterError("CREDENTIAL_CONFIG_EMPTY", "no GA sessions loaded")
            self._session.credential_generation = metadata.generation
            self._session.credential_checksum = metadata.checksum
            self._session.routing_snapshot_id = metadata.routing_snapshot_id
            self._session.capability_jtis = metadata.jtis
            # 审查: reload 必须同步刷新 SOPHub proxy——复用 Runner 的第二个
            # task 不能继续使用首个 task 已撤销的 capability token。配置被
            # 移除时 proxy 清空(下一任务不再安装 sophub 工具); 配置损坏时
            # 与 StartSession 一致地显式失败。
            from ga_worker.sop_tool import SopToolError, load_runtime_sophub_proxy
            try:
                self._session.sophub_proxy = load_runtime_sophub_proxy(self.config_root)
            except SopToolError as exc:
                raise WorkerAdapterError("SOPHUB_CONFIG_ERROR", str(exc)) from exc
            if self._session.sophub_proxy is not None:
                mem_env = os.environ.get("GA_WORKSPACE_MEMORY", "").strip()
                self._session.workspace_memory = (
                    Path(mem_env) if mem_env else Path(self.legacy_root) / "memory"
                )
            return worker_pb2.ReloadCredentialsResponse(
                credential_generation=metadata.generation,
                config_checksum=metadata.checksum,
            )

    def execute_task(self, request: worker_pb2.ExecuteTaskRequest) -> Iterator[worker_pb2.WorkerEvent]:
        from ga_worker.task_runner import run_task
        yield from run_task(self, request)

    def _assert_control_identity(self, workspace_key: str, runner_generation: int) -> None:
        """控制 RPC(Reload/Cancel/Shutdown)的身份 fencing(方案 §7, 审查):
        请求必须携带并与当前会话的 workspace/generation 精确匹配, 拒绝迟到
        或跨工作区的控制请求。"""
        if self._session is None:
            return
        if workspace_key != self._session.workspace_key:
            raise WorkerAdapterError(
                "WORKSPACE_KEY_MISMATCH",
                f"control workspace_key {workspace_key!r} != active {self._session.workspace_key!r}",
            )
        if runner_generation != self._session.runner_generation:
            raise WorkerAdapterError(
                "RUNNER_GENERATION_MISMATCH",
                f"control runner_generation {runner_generation} != active {self._session.runner_generation}",
            )

    def _assert_task_capability(self, capability_jti: str) -> None:
        """审查 R5-I8: 控制 RPC(BeginCheckpoint/CancelTask/Shutdown)携带的
        capability JTI 必须属于当前会话活跃凭据集——任务终态后 Platform 撤销
        JTI(DB), 复用 Runner 的下一任务持有新 JTI; 旧 JTI 无法控制新任务。
        空 JTI 仅允许在无凭据场景(清理路径/测试), 有活跃集时拒绝空值。"""
        if self._session is None:
            return
        if not capability_jti:
            if self._session.capability_jtis:
                raise WorkerAdapterError(
                    "CAPABILITY_JTI_REQUIRED",
                    "control rpc requires the active task's capability_jti",
                )
            return
        if self._session.capability_jtis and capability_jti not in self._session.capability_jtis:
            raise WorkerAdapterError(
                "CAPABILITY_JTI_MISMATCH",
                "control capability_jti not in the session's current credential set",
            )

    def cancel_task(self, task_id: str, workspace_key: str = "", runner_generation: int = 0, capability_jti: str = "") -> worker_pb2.CancelTaskResponse:
        with self._lock:
            if self._session is None:
                return worker_pb2.CancelTaskResponse(accepted=False)
            try:
                self._assert_control_identity(workspace_key, runner_generation)
                self._assert_task_capability(capability_jti)
            except WorkerAdapterError:
                return worker_pb2.CancelTaskResponse(accepted=False)
            agent = self._session.agent
            pending = self._pending
            if pending is not None and pending.task_id == task_id:
                pending.cancel_requested = True
                if pending.started:
                    self._abort_once_locked(pending, agent)
                return worker_pb2.CancelTaskResponse(accepted=True)
            if self._session.active_task_id == task_id:
                # P-M3: pending may have been cleared in a race; abort via session flag.
                self._abort_session_task(task_id, agent)
                return worker_pb2.CancelTaskResponse(accepted=True)
            return worker_pb2.CancelTaskResponse(accepted=False)

    def begin_checkpoint(self, request: worker_pb2.BeginCheckpointRequest) -> worker_pb2.CheckpointReady:
        with self._lock:
            if self._session is None:
                raise WorkerAdapterError("SESSION_NOT_STARTED", "no active session")
            # Runner generation fencing(审查 I7): 只有当前 generation 的 Runner
            # 可以提交 checkpoint; 旧 generation 的迟到请求必须拒绝。
            if request.runner_generation != self._session.runner_generation:
                raise WorkerAdapterError(
                    "CHECKPOINT_GENERATION_MISMATCH",
                    f"checkpoint runner_generation {request.runner_generation} != active {self._session.runner_generation}",
                )
            # 审查 R5-I8: checkpoint 必须携带当前 task 的 capability JTI。
            self._assert_task_capability(request.capability_jti)
            completed = self._session.completed
            if completed is None or completed.task_id != request.task_id:
                raise WorkerAdapterError(
                    "CHECKPOINT_TASK_MISMATCH",
                    "begin_checkpoint accepts only the active session's completed task",
                )
        self._validate_checkpoint_request(request)
        return self._write_checkpoint(request)

    def shutdown(self, reason: str, workspace_key: str = "", runner_generation: int = 0, capability_jti: str = "") -> worker_pb2.ShutdownResponse:
        with self._lock:
            if self._session is None:
                return worker_pb2.ShutdownResponse(accepted=True)
            try:
                self._assert_control_identity(workspace_key, runner_generation)
                self._assert_task_capability(capability_jti)
            except WorkerAdapterError:
                return worker_pb2.ShutdownResponse(accepted=False)
            self._session.shutting_down = True
            agent = self._session.agent
            pending = self._pending
            runner = self._session.runner_thread
        if pending is not None:
            pending.cancel_requested = True
            try:
                agent.abort()
            except Exception:
                pass
        try:
            agent.task_queue.put("STOP")
        except Exception:
            pass
        # P-M4: bounded join so shutdown never hangs forever.
        accepted = True
        if runner is not None and runner.is_alive():
            runner.join(timeout=SHUTDOWN_JOIN_TIMEOUT_S)
            if runner.is_alive():
                # The runner did not drain within the grace period (hung LLM
                # call / stuck tool). Report accepted=False so the platform
                # escalates to a hard kill after this best-effort teardown.
                accepted = False
        self._close_session_mcp()
        return worker_pb2.ShutdownResponse(accepted=accepted)
