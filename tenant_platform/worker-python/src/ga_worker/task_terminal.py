"""Terminal-event emitters for the task drain loop.

Extracted from task_runner.py (B3: file size limit). Each function is a generator
that yields at most one WorkerEvent carrying a Terminal payload, and records the
completed task on the adapter. None of them return early silently: a terminal is
always emitted unless one was already emitted for this task.
"""

from __future__ import annotations

from typing import Any, Iterator

from genericagent.worker.v1 import worker_pb2

from ga_worker.session_files import append_missing_file_markers
from ga_worker.state import TaskRunState

# P-M8: named constant (no magic numbers).
ERROR_MSG_MAX_LEN = 500


def map_exception_code(exc: BaseException) -> str:
    """Map a Python exception to a structured terminal error code.

    Before this, every unhandled exception surfaced as TASK_EXCEPTION, which
    made "LLM rate-limited" indistinguishable from "disk full" in task_events
    and delivery messages. Order matters: most specific first.
    """
    if isinstance(exc, MemoryError):
        return "OUT_OF_MEMORY"
    if isinstance(exc, TimeoutError):
        return "TASK_TIMEOUT"
    if isinstance(exc, PermissionError):
        return "PERMISSION_DENIED"
    if isinstance(exc, OSError):
        if getattr(exc, "errno", None) == 28:  # ENOSPC
            return "DISK_FULL"
        return "IO_ERROR"
    if isinstance(exc, (KeyboardInterrupt, SystemExit)):
        return "INTERRUPTED"
    # Requests/HTTP-shaped errors from the LLM client (duck-typed: works for
    # requests.HTTPError and any exception exposing a response.status_code).
    status = getattr(getattr(exc, "response", None), "status_code", None)
    if isinstance(status, int):
        if status == 429:
            return "LLM_RATE_LIMITED"
        if status >= 500:
            return "LLM_SERVER_ERROR"
        if status in (401, 403):
            return "LLM_AUTH_ERROR"
        return "LLM_CLIENT_ERROR"
    if exc.__class__.__name__ in ("ConnectionError", "ConnectTimeout", "ReadTimeout"):
        return "LLM_CONNECT_ERROR"
    return "TASK_EXCEPTION"



def emit_error_terminal(
    adapter: Any, task: worker_pb2.TaskEnvelope, state: TaskRunState,
    error_msg: Any, error_code: Any = None,
) -> Iterator[worker_pb2.WorkerEvent]:
    """Legacy failure → TASK_FAILED while preserving a structured error code."""
    if state.terminal_emitted:
        return
    code = str(error_code or "TASK_EXCEPTION")[:ERROR_MSG_MAX_LEN]
    message = str(error_msg)
    term = adapter._terminal(
        task.task_id, worker_pb2.TASK_FAILED,
        user_message=message[:ERROR_MSG_MAX_LEN],
        error_code=code, result_body=state.final_body,
    )
    adapter._record_completed(task, term, state.final_body, state.display_history, state.agent)
    yield worker_pb2.WorkerEvent(terminal=term)
    state.terminal_emitted = True


def emit_output_exceeded_terminal(
    adapter: Any, task: worker_pb2.TaskEnvelope, state: TaskRunState,
) -> Iterator[worker_pb2.WorkerEvent]:
    if state.terminal_emitted:
        return
    message = "max_output_bytes exceeded"
    term = adapter._terminal(
        task.task_id, worker_pb2.TASK_FAILED,
        user_message=message[:ERROR_MSG_MAX_LEN], error_code="MAX_OUTPUT_BYTES",
    )
    adapter._record_completed(task, term, state.final_body, state.display_history, state.agent)
    yield worker_pb2.WorkerEvent(terminal=term)
    state.terminal_emitted = True


def emit_cancel_or_timeout_terminal(
    adapter: Any, task: worker_pb2.TaskEnvelope, state: TaskRunState,
) -> Iterator[worker_pb2.WorkerEvent]:
    is_timeout = state.timed_out["v"]
    status = worker_pb2.TASK_INTERRUPTED if is_timeout else worker_pb2.TASK_CANCELLED
    code = "TASK_TIMEOUT" if is_timeout else "TASK_CANCELLED"
    message = "task timeout" if is_timeout else "cancelled"
    term = adapter._terminal(
        task.task_id, status,
        user_message=message[:ERROR_MSG_MAX_LEN],
        error_code=code,
    )
    adapter._record_completed(task, term, state.final_body, state.display_history, state.agent)
    yield worker_pb2.WorkerEvent(terminal=term)
    state.terminal_emitted = True


def emit_final_terminal(
    adapter: Any, task: worker_pb2.TaskEnvelope, state: TaskRunState,
) -> Iterator[worker_pb2.WorkerEvent]:
    session = getattr(adapter, "_session", None)
    generated = list(getattr(session, "generated_output_files", []) or [])
    if state.pending.cancel_requested or state.timed_out["v"]:
        is_timeout = state.timed_out["v"]
        status = worker_pb2.TASK_INTERRUPTED if is_timeout else worker_pb2.TASK_CANCELLED
        code = "TASK_TIMEOUT" if is_timeout else "TASK_CANCELLED"
        message = state.final_body or ("timeout" if is_timeout else "cancelled")
        term = adapter._terminal(
            task.task_id, status,
            user_message=message[:ERROR_MSG_MAX_LEN],
            error_code=code, result_body=state.final_body,
        )
    else:
        if generated:
            state.final_body = append_missing_file_markers(state.final_body, generated)
        term = adapter._terminal(
            task.task_id, worker_pb2.TASK_SUCCEEDED,
            user_message=state.final_body, result_body=state.final_body,
        )
    adapter._record_completed(task, term, state.final_body, state.display_history, state.agent)
    yield worker_pb2.WorkerEvent(terminal=term)
    state.terminal_emitted = True


def emit_missing_terminal_if_needed(
    adapter: Any, task: worker_pb2.TaskEnvelope, state: TaskRunState,
) -> Iterator[worker_pb2.WorkerEvent]:
    if state.terminal_emitted:
        return
    message = "max_output_bytes exceeded" if state.output_exceeded["v"] else "task ended without terminal payload"
    if state.output_exceeded["v"]:
        term = adapter._terminal(
            task.task_id, worker_pb2.TASK_FAILED,
            user_message=message[:ERROR_MSG_MAX_LEN], error_code="MAX_OUTPUT_BYTES",
        )
    else:
        term = adapter._terminal(
            task.task_id, worker_pb2.TASK_FAILED,
            user_message=message[:ERROR_MSG_MAX_LEN], error_code="MISSING_TERMINAL",
        )
    adapter._record_completed(task, term, state.final_body, state.display_history, state.agent)
    yield worker_pb2.WorkerEvent(terminal=term)
    state.terminal_emitted = True


def emit_exception_terminal(
    adapter: Any, task: worker_pb2.TaskEnvelope, state: TaskRunState | None, exc: Exception,
) -> Iterator[worker_pb2.WorkerEvent]:
    code = map_exception_code(exc)
    message = f"{code}: {exc}"
    term = adapter._terminal(
        task.task_id, worker_pb2.TASK_FAILED,
        user_message=message[:ERROR_MSG_MAX_LEN], error_code=code,
    )
    agent = state.agent if state is not None else None
    try:
        if agent is not None:
            adapter._record_completed(task, term, "", [], agent)
        else:
            with adapter._lock:
                adapter._clear_active_locked(task.task_id)
    except Exception:
        with adapter._lock:
            adapter._clear_active_locked(task.task_id)
    yield worker_pb2.WorkerEvent(terminal=term)
    if state is not None:
        state.terminal_emitted = True
