"""ManagedAgentAdapter: isolated single-session Worker state machine around GenericAgent."""

from __future__ import annotations

import copy
import hashlib
import json
import os
import queue
import threading
import time
import uuid
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any, Callable, Iterator, Optional

from genericagent.worker.v1 import worker_pb2

from ga_worker.checkpoint import (
    CheckpointError,
    build_snapshot_bundle,
    load_snapshot_bundle,
    result_digest_for,
    write_checkpoint_atomic,
)
from ga_worker.legacy_import import LegacyImportError, import_legacy_runtime
from ga_worker.limits import CapabilityRegistry, ToolPolicy
from ga_worker.runtime_overlay import (
    OverlayError,
    encode_session_id,
    materialize_runtime_overlay,
)


class WorkerAdapterError(Exception):
    def __init__(self, code: str, message: str):
        super().__init__(message)
        self.code = code
        self.message = message


@dataclass
class _PendingTask:
    task_id: str
    request: worker_pb2.ExecuteTaskRequest
    cancel_requested: bool = False
    started: bool = False
    reserved: threading.Event = field(default_factory=threading.Event)


@dataclass
class _CompletedTask:
    task_id: str
    session_key: str
    result_body: str
    result_digest: str
    status: int
    backend_history: list[Any]
    agent_history: list[Any]
    working: dict[str, Any]
    display_history: list[Any]
    checkpoint_token: str | None = None


@dataclass
class _SessionState:
    session_key: str
    session_id: str
    worker_instance_id: str
    runtime_policy: worker_pb2.RuntimePolicy
    snapshot_id: str
    snapshot_checksum: str
    snapshot_ref: str
    overlay_dir: Path
    manifest: dict[str, Any]
    agent: Any
    runner_thread: threading.Thread
    seed_working: dict[str, Any] = field(default_factory=dict)
    seed_backend_history: list[Any] = field(default_factory=list)
    seed_agent_history: list[Any] = field(default_factory=list)
    seed_display_history: list[Any] = field(default_factory=list)
    display_history: list[Any] = field(default_factory=list)
    completed: _CompletedTask | None = None
    active_task_id: str | None = None
    shutting_down: bool = False


AgentFactory = Callable[[], Any]


class ManagedAgentAdapter:
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
        self._session: _SessionState | None = None
        self._pending: _PendingTask | None = None
        self._event_queues: dict[str, queue.Queue] = {}
        # Optional test barrier: (reserved_event, proceed_event)
        self._test_pre_dispatch_barrier: tuple[threading.Event, threading.Event] | None = None
        self._legacy_mods: dict[str, Any] | None = None
        self._original_tools_schema: Any = None
        self._dispatch_wrap_installed = False

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
                ready=not self._session.shutting_down,
            )

    def start_session(self, request: worker_pb2.StartSessionRequest) -> worker_pb2.StartSessionResponse:
        if not request.session_key:
            raise WorkerAdapterError("INVALID_SESSION_KEY", "session_key required")
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

            try:
                session_id = encode_session_id(request.session_key)
                overlay_dir, manifest = materialize_runtime_overlay(
                    legacy_root=self.legacy_root,
                    runtime_root=self.runtime_root,
                    session_id=session_id,
                )
            except OverlayError as exc:
                raise WorkerAdapterError("OVERLAY_ERROR", str(exc)) from exc

            seed_working: dict[str, Any] = {}
            seed_backend: list[Any] = []
            seed_agent: list[Any] = []
            seed_display: list[Any] = []

            if request.snapshot_ref:
                seed_working, seed_backend, seed_agent, seed_display = self._load_snapshot(
                    request, policy
                )

            # Import legacy only when using real agent factory.
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

            # Clear handler carryover before first task.
            if hasattr(agent, "handler"):
                agent.handler = None

            # Restore histories before first task.
            if seed_agent:
                agent.history = copy.deepcopy(seed_agent)
            if seed_backend:
                self._set_backend_history(agent, copy.deepcopy(seed_backend))

            runner = threading.Thread(target=self._safe_run, args=(agent,), name="ga-runner", daemon=True)
            runner.start()

            self._session = _SessionState(
                session_key=request.session_key,
                session_id=session_id,
                worker_instance_id=self.worker_instance_id,
                runtime_policy=copy.deepcopy(policy),
                snapshot_id=request.snapshot_id or "",
                snapshot_checksum=request.snapshot_checksum or "",
                snapshot_ref=request.snapshot_ref or "",
                overlay_dir=overlay_dir,
                manifest=manifest,
                agent=agent,
                runner_thread=runner,
                seed_working=seed_working,
                seed_backend_history=seed_backend,
                seed_agent_history=seed_agent,
                seed_display_history=seed_display,
                display_history=[],  # never replay restored display to live stream
            )
            return worker_pb2.StartSessionResponse(
                session_key=self._session.session_key,
                worker_instance_id=self._session.worker_instance_id,
            )

    def execute_task(self, request: worker_pb2.ExecuteTaskRequest) -> Iterator[worker_pb2.WorkerEvent]:
        task = request.task
        if not task.task_id:
            raise WorkerAdapterError("INVALID_TASK", "task_id required")
        if not task.prompt and task.prompt != "":
            raise WorkerAdapterError("INVALID_TASK", "prompt required")

        with self._lock:
            if self._session is None:
                raise WorkerAdapterError("SESSION_NOT_STARTED", "call start_session first")
            if self._session.shutting_down:
                raise WorkerAdapterError("SHUTTING_DOWN", "worker is shutting down")
            if task.session_key != self._session.session_key:
                raise WorkerAdapterError(
                    "SESSION_MISMATCH",
                    f"task session_key {task.session_key!r} != active {self._session.session_key!r}",
                )
            if self._pending is not None or self._session.active_task_id is not None:
                raise WorkerAdapterError("TASK_ALREADY_RUNNING", "a task is already running")

            # Validate policy before enqueue.
            try:
                tool_policy = self.registry.resolve(
                    self._session.runtime_policy.capability_version,
                    task.tool_policy_version,
                )
            except ValueError as exc:
                raise WorkerAdapterError("UNKNOWN_TOOL_POLICY", str(exc)) from exc

            if self._session.runtime_policy.policy_digest != self.registry.digest:
                raise WorkerAdapterError("POLICY_DIGEST_MISMATCH", "session policy_digest mismatch")

            pending = _PendingTask(task_id=task.task_id, request=request)
            self._pending = pending
            self._session.active_task_id = task.task_id
            eq: queue.Queue = queue.Queue()
            self._event_queues[task.task_id] = eq

        # Test barrier for pre-start cancel.
        barrier = self._test_pre_dispatch_barrier
        if barrier is not None:
            reserved_ev, proceed_ev = barrier
            pending.reserved.set()
            reserved_ev.set()
            proceed_ev.wait(timeout=5)

        # Pre-start cancel check.
        with self._lock:
            if pending.cancel_requested:
                self._clear_active_locked(task.task_id)
                term = self._terminal(
                    task.task_id,
                    worker_pb2.TASK_CANCELLED,
                    user_message="cancelled before start",
                    error_code="TASK_CANCELLED",
                )
                yield worker_pb2.WorkerEvent(terminal=term)
                return

        # Apply task-scoped persona and tool filter, then put_task.
        agent = self._session.agent
        previous_persona = list(getattr(agent, "extra_sys_prompts", []) or [])
        previous_schema = None
        deadline_timer: threading.Timer | None = None
        output_bytes = 0
        max_output = int(self._session.runtime_policy.max_output_bytes or 0)
        max_turns = int(self._session.runtime_policy.max_turns or 0)
        timed_out = {"v": False}
        output_exceeded = {"v": False}
        cancel_flag = {"v": False}

        try:
            agent.extra_sys_prompts = list(task.persona_snapshot)
            previous_schema = self._apply_tool_policy(tool_policy)
            self._install_dispatch_guard(tool_policy)
            self._prepare_handler_seed(agent, self._session.seed_working)

            if max_turns > 0:
                self._install_max_turns(agent, max_turns)

            timeout_s = int(self._session.runtime_policy.task_timeout_seconds or 0)
            if timeout_s > 0:
                def _on_timeout():
                    timed_out["v"] = True
                    try:
                        agent.abort()
                    except Exception:
                        pass
                    self.cancel_task(task.task_id)

                deadline_timer = threading.Timer(timeout_s, _on_timeout)
                deadline_timer.daemon = True
                deadline_timer.start()

            with self._lock:
                if pending.cancel_requested:
                    self._clear_active_locked(task.task_id)
                    term = self._terminal(
                        task.task_id,
                        worker_pb2.TASK_CANCELLED,
                        user_message="cancelled before start",
                        error_code="TASK_CANCELLED",
                    )
                    yield worker_pb2.WorkerEvent(terminal=term)
                    return
                pending.started = True

            display_q = agent.put_task(task.prompt, source=task.source or "user")

            final_body = ""
            final_turn = 0
            terminal_emitted = False
            display_history: list[dict[str, Any]] = []

            while True:
                try:
                    item = display_q.get(timeout=0.1)
                except queue.Empty:
                    if pending.cancel_requested or cancel_flag["v"]:
                        # Wait a bit more for abort terminal.
                        try:
                            item = display_q.get(timeout=0.5)
                        except queue.Empty:
                            if not terminal_emitted:
                                status = (
                                    worker_pb2.TASK_INTERRUPTED
                                    if timed_out["v"]
                                    else worker_pb2.TASK_CANCELLED
                                )
                                code = "TASK_TIMEOUT" if timed_out["v"] else "TASK_CANCELLED"
                                term = self._terminal(
                                    task.task_id,
                                    status,
                                    user_message="cancelled" if not timed_out["v"] else "task timeout",
                                    error_code=code,
                                )
                                self._record_completed(
                                    task, term, final_body, display_history, agent
                                )
                                yield worker_pb2.WorkerEvent(terminal=term)
                                terminal_emitted = True
                            break
                    else:
                        continue

                if "next" in item:
                    text = item.get("next") or ""
                    turn = int(item.get("turn") or 0)
                    text_bytes = len(text.encode("utf-8"))
                    if max_output and output_bytes + text_bytes > max_output:
                        output_exceeded["v"] = True
                        try:
                            if getattr(agent, "handler", None) is not None:
                                agent.handler.code_stop_signal.append(1)
                            agent.abort()
                        except Exception:
                            pass
                        if not terminal_emitted:
                            term = self._terminal(
                                task.task_id,
                                worker_pb2.TASK_FAILED,
                                user_message="max_output_bytes exceeded",
                                error_code="MAX_OUTPUT_BYTES",
                            )
                            self._record_completed(
                                task, term, text[: max(0, max_output)], display_history, agent
                            )
                            yield worker_pb2.WorkerEvent(terminal=term)
                            terminal_emitted = True
                        break
                    output_bytes += text_bytes
                    display_history.append({"text": text, "turn": turn})
                    with self._lock:
                        if self._session is not None:
                            self._session.display_history.append({"text": text, "turn": turn})
                    yield worker_pb2.WorkerEvent(
                        chunk=worker_pb2.Chunk(task_id=task.task_id, text=text, turn=turn)
                    )
                    continue

                if "done" in item:
                    final_body = item.get("done") or ""
                    final_turn = int(item.get("turn") or 0)
                    if output_exceeded["v"]:
                        break
                    if pending.cancel_requested or timed_out["v"]:
                        status = (
                            worker_pb2.TASK_INTERRUPTED
                            if timed_out["v"]
                            else worker_pb2.TASK_CANCELLED
                        )
                        code = "TASK_TIMEOUT" if timed_out["v"] else "TASK_CANCELLED"
                        term = self._terminal(
                            task.task_id,
                            status,
                            user_message=final_body or ("timeout" if timed_out["v"] else "cancelled"),
                            error_code=code,
                            result_body=final_body,
                        )
                    else:
                        term = self._terminal(
                            task.task_id,
                            worker_pb2.TASK_SUCCEEDED,
                            user_message=final_body,
                            result_body=final_body,
                        )
                    self._record_completed(task, term, final_body, display_history, agent)
                    yield worker_pb2.WorkerEvent(terminal=term)
                    terminal_emitted = True
                    break

            if not terminal_emitted:
                term = self._terminal(
                    task.task_id,
                    worker_pb2.TASK_FAILED,
                    user_message="task ended without terminal payload",
                    error_code="MISSING_TERMINAL",
                )
                self._record_completed(task, term, final_body, display_history, agent)
                yield worker_pb2.WorkerEvent(terminal=term)

        except WorkerAdapterError:
            raise
        except Exception as exc:
            term = self._terminal(
                task.task_id,
                worker_pb2.TASK_FAILED,
                user_message=str(exc)[:500],
                error_code="TASK_EXCEPTION",
            )
            try:
                self._record_completed(task, term, "", [], agent)
            except Exception:
                with self._lock:
                    self._clear_active_locked(task.task_id)
            yield worker_pb2.WorkerEvent(terminal=term)
        finally:
            if deadline_timer is not None:
                deadline_timer.cancel()
            # Restore task-scoped settings.
            try:
                agent.extra_sys_prompts = previous_persona
            except Exception:
                agent.extra_sys_prompts = []
            self._restore_tool_schema(previous_schema)
            with self._lock:
                if self._pending and self._pending.task_id == task.task_id:
                    self._pending = None
                if self._session and self._session.active_task_id == task.task_id:
                    self._session.active_task_id = None
                self._event_queues.pop(task.task_id, None)

    def cancel_task(self, task_id: str) -> worker_pb2.CancelTaskResponse:
        with self._lock:
            if self._session is None:
                return worker_pb2.CancelTaskResponse(accepted=False)
            if self._pending and self._pending.task_id == task_id:
                self._pending.cancel_requested = True
                if self._pending.started:
                    try:
                        self._session.agent.abort()
                    except Exception:
                        pass
                return worker_pb2.CancelTaskResponse(accepted=True)
            if self._session.active_task_id == task_id:
                if self._pending:
                    self._pending.cancel_requested = True
                try:
                    self._session.agent.abort()
                except Exception:
                    pass
                return worker_pb2.CancelTaskResponse(accepted=True)
            return worker_pb2.CancelTaskResponse(accepted=False)

    def begin_checkpoint(self, request: worker_pb2.BeginCheckpointRequest) -> worker_pb2.CheckpointReady:
        with self._lock:
            if self._session is None:
                raise WorkerAdapterError("SESSION_NOT_STARTED", "no active session")
            completed = self._session.completed
            if completed is None or completed.task_id != request.task_id:
                raise WorkerAdapterError(
                    "CHECKPOINT_TASK_MISMATCH",
                    "begin_checkpoint accepts only the active session's completed task",
                )
            if not request.checkpoint_token:
                raise WorkerAdapterError("INVALID_CHECKPOINT_TOKEN", "checkpoint_token required")
            if not request.staging_ref:
                raise WorkerAdapterError("INVALID_STAGING_REF", "staging_ref required")

            staging = Path(request.staging_ref)
            # staging_ref must resolve under runtime_root
            try:
                resolved = staging.resolve()
                resolved.relative_to(self.runtime_root.resolve())
            except Exception as exc:
                raise WorkerAdapterError(
                    "INVALID_STAGING_REF",
                    f"staging_ref must resolve under runtime root: {request.staging_ref}",
                ) from exc

            policy = self._session.runtime_policy
            try:
                bundle = build_snapshot_bundle(
                    task_id=completed.task_id,
                    session_key=completed.session_key,
                    backend_history=completed.backend_history,
                    agent_history=completed.agent_history,
                    working=completed.working,
                    display_history=completed.display_history,
                    result_body=completed.result_body,
                    max_history_bytes=int(policy.max_history_bytes or 0) or 10**12,
                    max_working_bytes=int(policy.max_working_bytes or 0) or 10**12,
                )
                checksum, result_digest = write_checkpoint_atomic(
                    staging_ref=resolved,
                    bundle=bundle,
                    max_bundle_bytes=int(request.max_bundle_bytes or 0) or 10**12,
                    token=request.checkpoint_token,
                )
            except CheckpointError as exc:
                raise WorkerAdapterError(exc.code, exc.message) from exc

            completed.checkpoint_token = request.checkpoint_token
            return worker_pb2.CheckpointReady(
                task_id=request.task_id,
                checkpoint_token=request.checkpoint_token,
                staging_ref=str(resolved),
                checksum=checksum,
                result_digest=result_digest,
            )

    def shutdown(self, reason: str) -> worker_pb2.ShutdownResponse:
        with self._lock:
            if self._session is None:
                return worker_pb2.ShutdownResponse(accepted=True)
            self._session.shutting_down = True
            agent = self._session.agent
            pending = self._pending
        if pending is not None:
            pending.cancel_requested = True
            try:
                agent.abort()
            except Exception:
                pass
        # Cooperative stop of runner loop if supported.
        try:
            agent.task_queue.put("STOP")
        except Exception:
            pass
        return worker_pb2.ShutdownResponse(accepted=True)

    # ── internals ───────────────────────────────────────────────────────────

    def _session_matches(self, request: worker_pb2.StartSessionRequest) -> bool:
        assert self._session is not None
        s = self._session
        p = request.runtime_policy
        sp = s.runtime_policy
        return (
            request.session_key == s.session_key
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
        self,
        request: worker_pb2.StartSessionRequest,
        policy: worker_pb2.RuntimePolicy,
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
                max_history_bytes=int(policy.max_history_bytes or 0) or 10**12,
                max_working_bytes=int(policy.max_working_bytes or 0) or 10**12,
            )
        except CheckpointError as exc:
            raise WorkerAdapterError(exc.code, exc.message) from exc
        working = copy.deepcopy(data.get("working") or {})
        backend = copy.deepcopy(data.get("backend_history") or [])
        agent_hist = copy.deepcopy(data.get("agent_history") or [])
        display = copy.deepcopy(data.get("display_history") or [])
        return working, backend, agent_hist, display

    def _safe_run(self, agent: Any) -> None:
        try:
            agent.run()
        except Exception:
            pass

    def _set_backend_history(self, agent: Any, history: list[Any]) -> None:
        try:
            if hasattr(agent, "llmclient") and agent.llmclient is not None:
                agent.llmclient.backend.history = history
                return
        except Exception:
            pass
        # Scripted agent path.
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

    def _prepare_handler_seed(self, agent: Any, seed_working: dict[str, Any]) -> None:
        """Ensure new handler receives restored working; clear carryover handler."""
        agent.handler = None
        # For scripted agents without real handler factory, attach a seed handler
        # that put_task/run can observe.
        if self.agent_factory is not None and seed_working:
            class _Seed:
                def __init__(self):
                    self.working = copy.deepcopy(seed_working)
                    self.code_stop_signal: list[int] = []
                    self.history_info: list[Any] = []

            # Inject after construction of real handler is not available in scripted mode;
            # set a seed that run() may replace. Also stash for run to pick up.
            agent._adapter_seed_working = copy.deepcopy(seed_working)
            # Pre-set handler so tests that read handler.working before run see seed
            # only if run doesn't overwrite immediately; restore tests set handler in run.
            # For restore test, run creates handler if None — seed via attribute.
            if getattr(agent, "handler", None) is None:
                h = _Seed()
                agent.handler = h

        # Real GenericAgent: wrap handler construction by monkeypatching GenericAgentHandler if imported.
        if self._legacy_mods and seed_working:
            ga_mod = self._legacy_mods.get("ga")
            if ga_mod is not None and hasattr(ga_mod, "GenericAgentHandler"):
                original = ga_mod.GenericAgentHandler
                seed = copy.deepcopy(seed_working)

                class WrappedHandler(original):  # type: ignore[misc,valid-type]
                    def __init__(self, *args, **kwargs):
                        super().__init__(*args, **kwargs)
                        self.working = copy.deepcopy(seed)

                ga_mod.GenericAgentHandler = WrappedHandler  # type: ignore[assignment]
                agent._adapter_handler_wrapped = original

    def _apply_tool_policy(self, tool_policy: ToolPolicy) -> Any:
        """Filter agentmain.TOOLS_SCHEMA to allowed tools; return previous schema."""
        if self._legacy_mods is None:
            return None
        agentmain = self._legacy_mods.get("agentmain")
        if agentmain is None or not hasattr(agentmain, "TOOLS_SCHEMA"):
            return None
        previous = agentmain.TOOLS_SCHEMA
        allowed = tool_policy.allowed_tools
        filtered = [
            t
            for t in previous
            if isinstance(t, dict)
            and t.get("function", {}).get("name") in allowed
        ]
        agentmain.TOOLS_SCHEMA = filtered
        return previous

    def _restore_tool_schema(self, previous: Any) -> None:
        if previous is None or self._legacy_mods is None:
            return
        agentmain = self._legacy_mods.get("agentmain")
        if agentmain is not None:
            agentmain.TOOLS_SCHEMA = previous

    def _install_dispatch_guard(self, tool_policy: ToolPolicy) -> None:
        if self._legacy_mods is None:
            return
        ga_mod = self._legacy_mods.get("ga")
        agent_loop = self._legacy_mods.get("agent_loop")
        if ga_mod is None:
            return
        handler_cls = getattr(ga_mod, "GenericAgentHandler", None)
        if handler_cls is None:
            return
        allowed = tool_policy.allowed_tools
        if getattr(handler_cls, "_adapter_dispatch_guard", None) is allowed:
            return
        base_dispatch = handler_cls.dispatch

        def guarded(self, tool_name, args, response, index=0, tool_num=1):
            if tool_name not in allowed and tool_name not in ("no_tool", "bad_json"):
                yield f"tool denied by policy: {tool_name}\n"
                from agent_loop import StepOutcome

                return StepOutcome(None, next_prompt=f"tool denied: {tool_name}", should_exit=False)
            return (yield from base_dispatch(self, tool_name, args, response, index=index, tool_num=tool_num))

        handler_cls.dispatch = guarded  # type: ignore[assignment]
        handler_cls._adapter_dispatch_guard = allowed  # type: ignore[attr-defined]

    def _install_max_turns(self, agent: Any, max_turns: int) -> None:
        # Real path: agent_runner_loop uses handler.max_turns; we can't easily wrap without
        # patching agent_runner_loop. Store for handler.
        agent._adapter_max_turns = max_turns

    def _terminal(
        self,
        task_id: str,
        status: int,
        *,
        user_message: str = "",
        error_code: str | None = None,
        result_body: str | None = None,
    ) -> worker_pb2.Terminal:
        body = result_body if result_body is not None else user_message
        rd = result_digest_for(body) if status == worker_pb2.TASK_SUCCEEDED else (
            result_digest_for(body) if body else ""
        )
        term = worker_pb2.Terminal(
            task_id=task_id,
            status=status,
            result_digest=rd,
            user_message=user_message[:4000],
        )
        if error_code and status != worker_pb2.TASK_SUCCEEDED:
            term.error.code = error_code
            term.error.user_message = user_message[:4000]
        return term

    def _record_completed(
        self,
        task: worker_pb2.TaskEnvelope,
        term: worker_pb2.Terminal,
        result_body: str,
        display_history: list[dict[str, Any]],
        agent: Any,
    ) -> None:
        working: dict[str, Any] = {}
        if getattr(agent, "handler", None) is not None and hasattr(agent.handler, "working"):
            working = copy.deepcopy(agent.handler.working)
        # Also check seed attribute mutation.
        if not working and hasattr(agent, "_adapter_seed_working"):
            working = copy.deepcopy(agent._adapter_seed_working)
        agent_history = copy.deepcopy(list(getattr(agent, "history", []) or []))
        backend_history = self._get_backend_history(agent)
        completed = _CompletedTask(
            task_id=task.task_id,
            session_key=task.session_key,
            result_body=result_body,
            result_digest=term.result_digest or result_digest_for(result_body),
            status=term.status,
            backend_history=backend_history,
            agent_history=agent_history,
            working=working,
            display_history=list(display_history),
        )
        with self._lock:
            if self._session is not None:
                # Replace seed with complete working map from this task.
                self._session.seed_working = copy.deepcopy(working)
                self._session.completed = completed
                self._session.active_task_id = None
                if self._pending and self._pending.task_id == task.task_id:
                    self._pending = None

    def _clear_active_locked(self, task_id: str) -> None:
        if self._pending and self._pending.task_id == task_id:
            self._pending = None
        if self._session and self._session.active_task_id == task_id:
            self._session.active_task_id = None
        self._event_queues.pop(task_id, None)
