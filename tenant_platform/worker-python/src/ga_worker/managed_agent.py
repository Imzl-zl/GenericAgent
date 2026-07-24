"""ManagedAgentAdapter: isolated single-session Worker state machine around GenericAgent.

Public API only; internals split into:
- state.py: dataclasses (PendingTask, CompletedTask, SessionState, TaskRunState) + WorkerAdapterError
- legacy_instrument.py: monkeypatch helpers for legacy modules
- session_lifecycle.py: SessionLifecycleMixin (session creation, snapshot, checkpoint write)
- task_ops.py: TaskOpsMixin (terminal, abort, record_completed, cleanup)
- task_runner.py + task_drain.py + task_terminal.py: execute_task generator logic
"""

from __future__ import annotations

import threading
import uuid
from pathlib import Path
from typing import Any, Iterator

from genericagent.worker.v1 import worker_pb2

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
                ready=not self._session.shutting_down,
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

    def execute_task(self, request: worker_pb2.ExecuteTaskRequest) -> Iterator[worker_pb2.WorkerEvent]:
        from ga_worker.task_runner import run_task
        yield from run_task(self, request)

    def cancel_task(self, task_id: str) -> worker_pb2.CancelTaskResponse:
        with self._lock:
            if self._session is None:
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
            completed = self._session.completed
            if completed is None or completed.task_id != request.task_id:
                raise WorkerAdapterError(
                    "CHECKPOINT_TASK_MISMATCH",
                    "begin_checkpoint accepts only the active session's completed task",
                )
        self._validate_checkpoint_request(request)
        return self._write_checkpoint(request)

    def shutdown(self, reason: str) -> worker_pb2.ShutdownResponse:
        with self._lock:
            if self._session is None:
                return worker_pb2.ShutdownResponse(accepted=True)
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
        if runner is not None and runner.is_alive():
            runner.join(timeout=SHUTDOWN_JOIN_TIMEOUT_S)
        return worker_pb2.ShutdownResponse(accepted=True)
