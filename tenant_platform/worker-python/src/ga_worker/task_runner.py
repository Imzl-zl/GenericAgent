"""Task execution orchestrator: validate → setup → drain → cleanup.

Extracted from managed_agent.py (B3: file/function size limits). The drain loop
lives in task_drain.py and terminal emitters in task_terminal.py; this module
holds only the top-level `run_task` generator and the runtime-setup helpers.

Also addresses:
- P-M8: magic numbers extracted to named constants.
- B1: detects 'error' key in done payload → TASK_FAILED (handled in task_terminal).
- B2: no silent fallbacks for output quota (start_session validates > 0).
"""

from __future__ import annotations

import queue
import threading
from typing import Any, Callable, Iterator

from genericagent.worker.v1 import worker_pb2

from ga_worker.state import PendingTask, TaskRunState, WorkerAdapterError

# P-M8: named constants (no magic numbers).
PRE_DISPATCH_BARRIER_TIMEOUT_S = 5.0


def run_task(adapter: Any, request: worker_pb2.ExecuteTaskRequest) -> Iterator[worker_pb2.WorkerEvent]:
    """Main entry: validate → setup → drain → cleanup. Yields WorkerEvents."""
    task = request.task
    _validate_task_request(task)
    pending, tool_policy = _validate_and_reserve(adapter, request, task)
    state: TaskRunState | None = None
    try:
        if (yield from _await_pre_dispatch(adapter, task, pending)):
            return
        state = _setup_runtime(adapter, task, pending, tool_policy)
        if (yield from _check_post_setup_cancel(adapter, task, pending)):
            return
        display_q = _put_task_and_wrap_handler(adapter, task, pending, state)
        yield from _drain_display_queue(adapter, task, pending, state, display_q)
        yield from _emit_missing_terminal_if_needed(adapter, task, state)
    except WorkerAdapterError:
        raise
    except Exception as exc:
        yield from _emit_exception_terminal(adapter, task, state, exc)
    finally:
        _cleanup_task(adapter, task, state)


def _validate_task_request(task: worker_pb2.TaskEnvelope) -> None:
    if not task.task_id:
        raise WorkerAdapterError("INVALID_TASK", "task_id required")
    if not task.prompt and task.prompt != "":
        raise WorkerAdapterError("INVALID_TASK", "prompt required")


def _validate_and_reserve(
    adapter: Any, request: worker_pb2.ExecuteTaskRequest, task: worker_pb2.TaskEnvelope,
) -> tuple[PendingTask, Any]:
    with adapter._lock:
        session = adapter._session
        if session is None:
            raise WorkerAdapterError("SESSION_NOT_STARTED", "call start_session first")
        if session.shutting_down:
            raise WorkerAdapterError("SHUTTING_DOWN", "worker is shutting down")
        if task.session_key != session.session_key:
            raise WorkerAdapterError(
                "SESSION_MISMATCH",
                f"task session_key {task.session_key!r} != active {session.session_key!r}",
            )
        if adapter._pending is not None or session.active_task_id is not None:
            raise WorkerAdapterError("TASK_ALREADY_RUNNING", "a task is already running")
        try:
            tool_policy = adapter.registry.resolve(
                session.runtime_policy.capability_version,
                task.tool_policy_version,
            )
        except ValueError as exc:
            raise WorkerAdapterError("UNKNOWN_TOOL_POLICY", str(exc)) from exc
        if session.runtime_policy.policy_digest != adapter.registry.digest:
            raise WorkerAdapterError("POLICY_DIGEST_MISMATCH", "session policy_digest mismatch")
        pending = PendingTask(task_id=task.task_id, request=request)
        adapter._pending = pending
        session.active_task_id = task.task_id
        adapter._event_queues[task.task_id] = queue.Queue()
    return pending, tool_policy


def _await_pre_dispatch(
    adapter: Any, task: worker_pb2.TaskEnvelope, pending: PendingTask,
) -> Iterator[worker_pb2.WorkerEvent]:
    """First cancel check: after barrier, before runtime setup. Returns True if cancelled."""
    barrier = adapter._test_pre_dispatch_barrier
    if barrier is not None:
        reserved_ev, proceed_ev = barrier
        pending.reserved.set()
        reserved_ev.set()
        proceed_ev.wait(timeout=PRE_DISPATCH_BARRIER_TIMEOUT_S)
    else:
        adapter._file_pre_dispatch_barrier(task.task_id, pending)
    with adapter._lock:
        cancelled = pending.cancel_requested
        if cancelled:
            adapter._clear_active_locked(task.task_id)
    if cancelled:
        term = adapter._terminal(
            task.task_id, worker_pb2.TASK_CANCELLED,
            user_message="cancelled before start", error_code="TASK_CANCELLED",
        )
        yield worker_pb2.WorkerEvent(terminal=term)
        return True
    return False


def _check_post_setup_cancel(
    adapter: Any, task: worker_pb2.TaskEnvelope, pending: PendingTask,
) -> Iterator[worker_pb2.WorkerEvent]:
    """Second cancel check: after runtime setup, before put_task. Returns True if cancelled."""
    with adapter._lock:
        cancelled = pending.cancel_requested
        if cancelled:
            adapter._clear_active_locked(task.task_id)
        else:
            pending.started = True
    if cancelled:
        term = adapter._terminal(
            task.task_id, worker_pb2.TASK_CANCELLED,
            user_message="cancelled before start", error_code="TASK_CANCELLED",
        )
        yield worker_pb2.WorkerEvent(terminal=term)
        return True
    return False


def _setup_runtime(
    adapter: Any, task: worker_pb2.TaskEnvelope, pending: PendingTask, tool_policy: Any,
) -> TaskRunState:
    from ga_worker.legacy_instrument import (
        apply_tool_policy, install_dispatch_guard, install_handler_print_counter,
        install_max_turns, prepare_handler_seed,
    )
    agent = adapter._session.agent
    state = TaskRunState(
        pending=pending,
        agent=agent,
        max_output=int(adapter._session.runtime_policy.max_output_bytes),
        max_turns=int(adapter._session.runtime_policy.max_turns),
        previous_persona=list(getattr(agent, "extra_sys_prompts", []) or []),
    )
    agent.extra_sys_prompts = list(task.persona_snapshot)
    state.previous_schema = apply_tool_policy(tool_policy, adapter._legacy_mods)
    state.dispatch_unwrap = install_dispatch_guard(tool_policy, adapter._legacy_mods)
    state.seed_unwrap = prepare_handler_seed(
        agent, adapter._session.seed_working, adapter.agent_factory, adapter._legacy_mods,
    )
    state.count_fn = _make_count_fn(state, adapter)
    install_handler_print_counter(agent, state.count_fn, adapter._legacy_mods)
    if state.max_turns > 0:
        state.loop_unwrap = install_max_turns(agent, state.max_turns, adapter._legacy_mods)
    _arm_deadline_timer(adapter, task, state)
    return state


def _make_count_fn(state: TaskRunState, adapter: Any) -> Callable[[str], bool]:
    """Create a single-arg byte counter closure (original was a closure in execute_task)."""
    def count_output(text: str) -> bool:
        n = len((text or "").encode("utf-8"))
        state.output_bytes["n"] += n
        if state.output_bytes["n"] > state.max_output:
            state.output_exceeded["v"] = True
            try:
                if getattr(state.agent, "handler", None) is not None:
                    state.agent.handler.code_stop_signal.append(1)
                adapter._abort_once(state.pending, state.agent)
            except Exception:
                pass
            return True
        return False
    return count_output


def _arm_deadline_timer(adapter: Any, task: worker_pb2.TaskEnvelope, state: TaskRunState) -> None:
    timeout_s = int(adapter._session.runtime_policy.task_timeout_seconds or 0)
    if timeout_s <= 0:
        return

    def _on_timeout():
        state.timed_out["v"] = True
        adapter.cancel_task(task.task_id)

    state.deadline_timer = threading.Timer(timeout_s, _on_timeout)
    state.deadline_timer.daemon = True
    state.deadline_timer.start()


def _put_task_and_wrap_handler(
    adapter: Any, task: worker_pb2.TaskEnvelope, pending: PendingTask, state: TaskRunState,
) -> queue.Queue:
    from ga_worker.task_drain import put_task_and_wrap_handler
    return put_task_and_wrap_handler(adapter, task, pending, state)


def _drain_display_queue(
    adapter: Any, task: worker_pb2.TaskEnvelope, pending: PendingTask,
    state: TaskRunState, display_q: queue.Queue,
) -> Iterator[worker_pb2.WorkerEvent]:
    from ga_worker.task_drain import drain_display_queue
    yield from drain_display_queue(adapter, task, pending, state, display_q)


def _emit_missing_terminal_if_needed(
    adapter: Any, task: worker_pb2.TaskEnvelope, state: TaskRunState,
) -> Iterator[worker_pb2.WorkerEvent]:
    from ga_worker.task_terminal import emit_missing_terminal_if_needed
    yield from emit_missing_terminal_if_needed(adapter, task, state)


def _emit_exception_terminal(
    adapter: Any, task: worker_pb2.TaskEnvelope, state: TaskRunState | None, exc: Exception,
) -> Iterator[worker_pb2.WorkerEvent]:
    from ga_worker.task_terminal import emit_exception_terminal
    yield from emit_exception_terminal(adapter, task, state, exc)


def _cleanup_task(adapter: Any, task: worker_pb2.TaskEnvelope, state: TaskRunState | None) -> None:
    if state is not None:
        if state.deadline_timer is not None:
            state.deadline_timer.cancel()
        for unwrap in (state.loop_unwrap, state.dispatch_unwrap, state.seed_unwrap):
            if unwrap is not None:
                try:
                    unwrap()
                except Exception:
                    pass
        try:
            state.agent.extra_sys_prompts = state.previous_persona
        except Exception:
            state.agent.extra_sys_prompts = []
        from ga_worker.legacy_instrument import restore_tool_schema
        restore_tool_schema(state.previous_schema, adapter._legacy_mods)
    with adapter._lock:
        if adapter._pending and adapter._pending.task_id == task.task_id:
            adapter._pending = None
        if adapter._session and adapter._session.active_task_id == task.task_id:
            adapter._session.active_task_id = None
        adapter._event_queues.pop(task.task_id, None)
