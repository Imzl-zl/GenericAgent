"""Session lifecycle + checkpoint helpers for ManagedAgentAdapter.

Extracted from managed_agent.py (B3: file size limit). Mixed into the adapter
so `self` still refers to the full adapter instance. Covers: start_session
validation, session creation, snapshot loading, checkpoint write, and backend
history accessors.
"""

from __future__ import annotations

import copy
import logging
import os
import threading
from pathlib import Path
from typing import Any

from genericagent.worker.v1 import worker_pb2

logger = logging.getLogger(__name__)

from ga_worker.checkpoint import (
    CheckpointError,
    build_snapshot_bundle,
    load_snapshot_bundle,
    result_digest_for,
    write_checkpoint_atomic,
)
from ga_worker.credential_config import CredentialConfigError, load_runtime_metadata
from ga_worker.legacy_import import LegacyImportError, import_legacy_runtime
from ga_worker.mcp_config import MCPConfigError, load_runtime_mcp_snapshot
from ga_worker.mcp_runtime import MCPRuntimeError, close_mcp_clients, initialize_mcp_catalog
from ga_worker.runtime_overlay import (
    OverlayError,
    encode_session_id,
    materialize_runtime_overlay,
)
from ga_worker.sop_tool import SopToolError, load_runtime_sophub_proxy
from ga_worker.state import (
    CompletedTask,
    SessionState,
    WorkerAdapterError,
)


class SessionLifecycleMixin:
    """Session creation, validation, snapshot loading, and checkpoint write.

    Depends on instance attributes set by ManagedAgentAdapter.__init__:
    config_root, legacy_root, runtime_root, agent_factory, _lock, _session,
    _legacy_mods, registry.
    """

    def _validate_start_session_request(self, request: worker_pb2.StartSessionRequest) -> None:
        if not request.session_key:
            raise WorkerAdapterError("INVALID_SESSION_KEY", "session_key required")
        # generation fencing(方案 §7): Runner 只接受绑定的 workspace_key 与 generation。
        if not request.workspace_key:
            raise WorkerAdapterError("INVALID_WORKSPACE_KEY", "workspace_key required")
        if request.workspace_key != request.session_key:
            raise WorkerAdapterError("WORKSPACE_KEY_MISMATCH", "workspace_key must match session_key")
        if request.runner_generation == 0:
            raise WorkerAdapterError("INVALID_RUNNER_GENERATION", "runner_generation required")
        # 容器不可变身份(方案 §7, 审查): Manager 创建容器时固定注入
        # GA_WORKSPACE_KEY / GA_RUNNER_GENERATION, Worker 自身无法修改。
        # 容器模式(env 存在)强制校验, 防止误路由/迟到的控制面请求在错误
        # 工作区挂载中初始化; loopback 开发(env 缺失)跳过。
        container_ws = os.environ.get("GA_WORKSPACE_KEY", "").strip()
        container_gen = os.environ.get("GA_RUNNER_GENERATION", "").strip()
        if container_ws and request.workspace_key != container_ws:
            raise WorkerAdapterError(
                "WORKSPACE_KEY_MISMATCH",
                f"workspace_key {request.workspace_key!r} != container identity {container_ws!r}",
            )
        if container_gen:
            try:
                container_generation = int(container_gen)
            except ValueError:
                raise WorkerAdapterError("RUNNER_IDENTITY_INVALID", "container runner generation env is invalid")
            if request.runner_generation != container_generation:
                raise WorkerAdapterError(
                    "RUNNER_GENERATION_MISMATCH",
                    f"runner_generation {request.runner_generation} != container generation {container_generation}",
                )
        policy = request.runtime_policy
        if not policy.capability_version:
            raise WorkerAdapterError("INVALID_RUNTIME_POLICY", "capability_version required")
        if policy.policy_digest != self.registry.digest:
            raise WorkerAdapterError(
                "POLICY_DIGEST_MISMATCH",
                f"policy_digest mismatch: expected {self.registry.digest}",
            )
        if policy.capability_version not in self.registry.capability_versions():
            raise WorkerAdapterError(
                "UNKNOWN_CAPABILITY",
                f"unknown capability_version: {policy.capability_version}",
            )
        # task_timeout_seconds=0 means disabled (matches platform --task-timeout-seconds=0
        # convention); only positive values are enforced as a wall-clock deadline.
        for name, value in (
            ("max_turns", policy.max_turns),
            ("max_history_bytes", policy.max_history_bytes),
            ("max_working_bytes", policy.max_working_bytes),
            ("max_output_bytes", policy.max_output_bytes),
        ):
            if int(value) <= 0:
                raise WorkerAdapterError(
                    "INVALID_RUNTIME_POLICY",
                    f"{name} must be positive, got {value}",
                )
        if int(policy.task_timeout_seconds) < 0:
            raise WorkerAdapterError(
                "INVALID_RUNTIME_POLICY",
                f"task_timeout_seconds must be non-negative, got {policy.task_timeout_seconds}",
            )

    def _create_session(self, request: worker_pb2.StartSessionRequest) -> worker_pb2.StartSessionResponse:
        try:
            session_id = encode_session_id(request.session_key)
            # 审查 R4-I13: 容器 Runner 的 GA_OVERLAY_ROOT 指向 tmpfs, overlay
            # 不落持久 state; loopback 未设置时回退 runtime_root。
            overlay_root_env = os.environ.get("GA_OVERLAY_ROOT", "").strip()
            overlay_root = Path(overlay_root_env) if overlay_root_env else None
            overlay_dir, manifest = materialize_runtime_overlay(
                legacy_root=self.legacy_root,
                runtime_root=self.runtime_root,
                session_id=session_id,
                overlay_root=overlay_root,
            )
        except OverlayError as exc:
            raise WorkerAdapterError("OVERLAY_ERROR", str(exc)) from exc

        seed_working, seed_backend, seed_agent, seed_display = self._load_snapshot_if_any(request)
        try:
            credential_metadata = load_runtime_metadata(self.config_root)
        except CredentialConfigError as exc:
            raise WorkerAdapterError("CREDENTIAL_CONFIG_ERROR", str(exc)) from exc
        # SOPHub proxy capability(方案 §5.2): 配置存在时 Worker 才能搜索/安装
        # SOP; 安装目标为当前工作区 memory/sops/。
        try:
            sophub_proxy = load_runtime_sophub_proxy(self.config_root)
        except SopToolError as exc:
            raise WorkerAdapterError("SOPHUB_CONFIG_ERROR", str(exc)) from exc
        workspace_memory = None
        if sophub_proxy is not None:
            mem_env = os.environ.get("GA_WORKSPACE_MEMORY", "").strip()
            workspace_memory = Path(mem_env) if mem_env else Path(self.legacy_root) / "memory"
        agent = self._create_agent(overlay_dir, manifest)
        self._restore_histories(agent, seed_agent, seed_backend)
        try:
            mcp_snapshot = load_runtime_mcp_snapshot(self.config_root)
            mcp_tools, mcp_clients = initialize_mcp_catalog(mcp_snapshot)
        except (MCPConfigError, MCPRuntimeError, ValueError) as exc:
            raise WorkerAdapterError("MCP_INITIALIZATION_ERROR", str(exc)) from exc

        runner = threading.Thread(target=self._safe_run, args=(agent,), name="ga-runner", daemon=True)
        # 审查: 必须先赋值 self._session 再启动 runner 线程, 否则线程在
        # SessionState 创建前崩溃时 agent_failed 无处标记, 失败静默丢失且
        # health 继续报 ready(审查 R4-I11 启动竞态)。
        self._session = SessionState(
            session_key=request.session_key,
            session_id=session_id,
            worker_instance_id=self.worker_instance_id,
            runtime_policy=copy.deepcopy(request.runtime_policy),
            workspace_key=request.workspace_key,
            runner_generation=request.runner_generation,
            snapshot_id=request.snapshot_id or "",
            snapshot_checksum=request.snapshot_checksum or "",
            snapshot_ref=request.snapshot_ref or "",
            overlay_dir=overlay_dir,
            manifest=manifest,
            agent=agent,
            runner_thread=runner,
            credential_generation=credential_metadata.generation,
            credential_checksum=credential_metadata.checksum,
            routing_snapshot_id=credential_metadata.routing_snapshot_id,
            capability_jtis=credential_metadata.jtis,
            sophub_proxy=sophub_proxy,
            workspace_memory=workspace_memory,
            seed_working=seed_working,
            seed_backend_history=seed_backend,
            seed_agent_history=seed_agent,
            seed_display_history=seed_display,
            display_history=[],
            mcp_snapshot_id=mcp_snapshot.snapshot_id,
            mcp_tools=mcp_tools,
            mcp_clients=mcp_clients,
        )
        runner.start()
        return worker_pb2.StartSessionResponse(
            session_key=self._session.session_key,
            worker_instance_id=self._session.worker_instance_id,
        )

    def _create_agent(self, overlay_dir: Path, manifest: dict[str, Any]) -> Any:

        if self.agent_factory is None:
            try:
                self._legacy_mods = import_legacy_runtime(
                    config_root=self.config_root,
                    legacy_root=self.legacy_root,
                    overlay_dir=overlay_dir,
                    manifest=manifest,
                )
            except (LegacyImportError, Exception) as exc:
                raise WorkerAdapterError("LEGACY_IMPORT_ERROR", str(exc)) from exc
            agent_cls = self._legacy_mods["agentmain"].GenericAgent
            agent = agent_cls()
        else:
            agent = self.agent_factory()
        # Formal Worker path streams deltas only. Legacy frontends opt into
        # inc_out individually; forcing it here keeps gRPC payloads, in-memory
        # display history, and chunk-event persistence linear instead of O(n²)
        # cumulative-text replay.
        if hasattr(agent, "inc_out"):
            agent.inc_out = True
        # Tenant deliveries should match GA chat frontends: user-facing output
        # comes from cleaned reply text, not the verbose raw transcript that
        # includes internal turn markers / tool traces / provider-specific
        # streaming detail.
        if hasattr(agent, "verbose"):
            agent.verbose = False
        if hasattr(agent, "handler"):
            agent.handler = None
        return agent

    def _restore_histories(self, agent: Any, seed_agent: list[Any], seed_backend: list[Any]) -> None:
        if seed_agent:
            agent.history = copy.deepcopy(seed_agent)
        if seed_backend:
            self._set_backend_history(agent, copy.deepcopy(seed_backend))

    def _load_snapshot_if_any(
        self, request: worker_pb2.StartSessionRequest,
    ) -> tuple[dict[str, Any], list[Any], list[Any], list[Any]]:
        if not request.snapshot_ref:
            return {}, [], [], []
        working, backend, agent_hist, display = self._load_snapshot(request, request.runtime_policy)
        return working, backend, agent_hist, display

    def _session_matches(self, request: worker_pb2.StartSessionRequest) -> bool:
        assert self._session is not None
        s = self._session
        p = request.runtime_policy
        sp = s.runtime_policy
        return (
            request.session_key == s.session_key
            and (request.workspace_key or "") == s.workspace_key
            and request.runner_generation == s.runner_generation
            and (request.snapshot_ref or "") == s.snapshot_ref
            and (request.snapshot_id or "") == s.snapshot_id
            and (request.snapshot_checksum or "") == s.snapshot_checksum
            and p.max_turns == sp.max_turns
            and p.max_history_bytes == sp.max_history_bytes
            and p.max_working_bytes == sp.max_working_bytes
            and p.max_output_bytes == sp.max_output_bytes
            and p.task_timeout_seconds == sp.task_timeout_seconds
            and p.capability_version == sp.capability_version
            and p.policy_digest == sp.policy_digest
        )

    def _load_snapshot(
        self, request: worker_pb2.StartSessionRequest, policy: worker_pb2.RuntimePolicy,
    ) -> tuple[dict[str, Any], list[Any], list[Any], list[Any]]:
        ref = Path(request.snapshot_ref)
        try:
            resolved = ref.resolve()
            resolved.relative_to(self.runtime_root.resolve())
        except Exception as exc:
            raise WorkerAdapterError(
                "INVALID_SNAPSHOT_REF",
                f"snapshot_ref must resolve under runtime root: {request.snapshot_ref}",
            ) from exc
        if not request.snapshot_checksum:
            raise WorkerAdapterError("SNAPSHOT_CHECKSUM_REQUIRED", "snapshot_checksum required")
        try:
            data = load_snapshot_bundle(
                resolved,
                expected_checksum=request.snapshot_checksum,
                max_history_bytes=int(policy.max_history_bytes),
                max_working_bytes=int(policy.max_working_bytes),
                # 审查 R4-I6: Platform 签发快照时按 Prepare 的 max_bundle_bytes
                # 校验, 恢复时下发该上限供限长读取; 未下发时按策略上限推导
                # 保守界, 保证任何路径都不会无界读入。
                max_bundle_bytes=int(request.max_bundle_bytes)
                or int(policy.max_history_bytes)
                + int(policy.max_working_bytes)
                + int(policy.max_output_bytes)
                + (1 << 20),
            )
        except CheckpointError as exc:
            raise WorkerAdapterError(exc.code, exc.message) from exc
        working = copy.deepcopy(data.get("working") or {})
        backend = copy.deepcopy(data.get("backend_history") or [])
        agent_hist = copy.deepcopy(data.get("agent_history") or [])
        display = copy.deepcopy(data.get("display_history") or [])
        return working, backend, agent_hist, display

    def _validate_checkpoint_request(self, request: worker_pb2.BeginCheckpointRequest) -> None:
        if not request.checkpoint_token:
            raise WorkerAdapterError("INVALID_CHECKPOINT_TOKEN", "checkpoint_token required")
        if not request.staging_ref:
            raise WorkerAdapterError("INVALID_STAGING_REF", "staging_ref required")
        staging = Path(request.staging_ref)
        try:
            resolved = staging.resolve()
            resolved.relative_to(self.runtime_root.resolve())
        except Exception as exc:
            raise WorkerAdapterError(
                "INVALID_STAGING_REF",
                f"staging_ref must resolve under runtime root: {request.staging_ref}",
            ) from exc

    def _write_checkpoint(self, request: worker_pb2.BeginCheckpointRequest) -> worker_pb2.CheckpointReady:
        with self._lock:
            completed: CompletedTask = self._session.completed  # type: ignore[union-attr]
            policy = self._session.runtime_policy  # type: ignore[union-attr]
        # Use the staging_ref as-is for writing and round-tripping back to the
        # coordinator. Path.resolve() on Windows normalizes the drive letter to
        # uppercase (c:\ -> C:\), which would mismatch the DB-stored ref and
        # fail the coordinator's staging_ref equality check. Validation that
        # the ref resolves under runtime_root is done in _validate_checkpoint_request.
        staging = Path(request.staging_ref)
        try:
            bundle = build_snapshot_bundle(
                task_id=completed.task_id,
                session_key=completed.session_key,
                backend_history=completed.backend_history,
                agent_history=completed.agent_history,
                working=completed.working,
                display_history=completed.display_history,
                result_body=completed.result_body,
                max_history_bytes=int(policy.max_history_bytes),
                max_working_bytes=int(policy.max_working_bytes),
                runner_generation=request.runner_generation,
            )
            checksum, result_digest = write_checkpoint_atomic(
                staging_ref=staging,
                bundle=bundle,
                max_bundle_bytes=int(request.max_bundle_bytes),
                token=request.checkpoint_token,
            )
        except CheckpointError as exc:
            raise WorkerAdapterError(exc.code, exc.message) from exc
        with self._lock:
            if self._session is not None and self._session.completed is not None:
                self._session.completed.checkpoint_token = request.checkpoint_token
        return worker_pb2.CheckpointReady(
            task_id=request.task_id,
            checkpoint_token=request.checkpoint_token,
            staging_ref=str(staging),
            checksum=checksum,
            result_digest=result_digest,
            runner_generation=request.runner_generation,
        )

    def _close_session_mcp(self) -> None:
        if self._session is None:
            return
        close_mcp_clients(self._session.mcp_clients)
        self._session.mcp_clients = []
        self._session.mcp_tools = {}

    def _safe_run(self, agent: Any) -> None:
        # 审查: agent 主线程异常必须显式记录并标记会话失败, 不能静默吞掉——
        # 否则 health/heartbeat 继续 ready, 后续任务会对死亡线程的空队列挂起,
        # 关闭 task timeout 时永久占用 Runner。
        try:
            agent.run()
        except Exception:
            logger.exception("ga-runner thread crashed")
            with self._lock:
                if self._session is not None:
                    self._session.agent_failed = True

    def _set_backend_history(self, agent: Any, history: list[Any]) -> None:
        try:
            if hasattr(agent, "llmclient") and agent.llmclient is not None:
                agent.llmclient.backend.history = history
                return
        except Exception:
            pass
        if hasattr(agent, "backend_history"):
            agent.backend_history = history
        if hasattr(agent, "llmclient"):
            try:
                agent.llmclient.backend.history = history
            except Exception:
                pass

    def _get_backend_history(self, agent: Any) -> list[Any]:
        try:
            if hasattr(agent, "llmclient") and agent.llmclient is not None:
                return copy.deepcopy(list(agent.llmclient.backend.history))
        except Exception:
            pass
        if hasattr(agent, "backend_history"):
            return copy.deepcopy(list(agent.backend_history))
        return []
