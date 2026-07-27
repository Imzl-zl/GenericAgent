"""Shared state dataclasses and error type for ManagedAgentAdapter.

Extracted from managed_agent.py to keep file sizes under the 300-line limit
(B3 fix) without changing behavior.
"""

from __future__ import annotations

import threading
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any, Callable

from genericagent.worker.v1 import worker_pb2


class WorkerAdapterError(Exception):
    def __init__(self, code: str, message: str):
        super().__init__(message)
        self.code = code
        self.message = message


@dataclass
class PendingTask:
    task_id: str
    request: worker_pb2.ExecuteTaskRequest
    cancel_requested: bool = False
    started: bool = False
    abort_called: bool = False
    reserved: threading.Event = field(default_factory=threading.Event)


@dataclass
class CompletedTask:
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
class SessionState:
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
    credential_generation: int
    credential_checksum: str
    routing_snapshot_id: str
    seed_working: dict[str, Any] = field(default_factory=dict)
    seed_backend_history: list[Any] = field(default_factory=list)
    seed_agent_history: list[Any] = field(default_factory=list)
    seed_display_history: list[Any] = field(default_factory=list)
    display_history: list[Any] = field(default_factory=list)
    completed: CompletedTask | None = None
    active_task_id: str | None = None
    shutting_down: bool = False


@dataclass
class TaskRunState:
    """Mutable per-task runtime state shared across drain-loop sub-generators.

    Lives in state.py so task_terminal.py and task_drain.py can import it without
    creating a cycle back into task_runner.py.
    """
    pending: PendingTask
    agent: Any
    output_bytes: dict = field(default_factory=lambda: {"n": 0})
    max_output: int = 0
    max_turns: int = 0
    timed_out: dict = field(default_factory=lambda: {"v": False})
    output_exceeded: dict = field(default_factory=lambda: {"v": False})
    deadline_timer: threading.Timer | None = None
    loop_unwrap: Callable[[], None] | None = None
    dispatch_unwrap: Callable[[], None] | None = None
    seed_unwrap: Callable[[], None] | None = None
    previous_persona: list[str] = field(default_factory=list)
    previous_schema: Any = None
    final_body: str = ""
    final_turn: int = 0
    terminal_emitted: bool = False
    display_history: list[dict[str, Any]] = field(default_factory=list)
    count_fn: Callable[[str], bool] | None = None
    # monotonic clock of the last heartbeat emission; see task_drain.HEARTBEAT_INTERVAL_S.
    last_heartbeat_at: float = 0.0


AgentFactory = Callable[[], Any]
