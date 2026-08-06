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


def cleanup_legacy_subprocesses(adapter: Any) -> bool:
    """任务终态前清理 legacy code_run 遗留进程组(审查 C2)。

    code_run 以独立进程组运行(ga.py), 正常返回后可能仍有派生的后台子进程
    (如 nohup ... &)。任务即进程(决策 D1)下 Runner 终态即销毁, 但这些进程
    若在销毁前窗口存活仍可能干扰终态提交或污染快照; 清理保持 fail-closed。
    经 adapter._legacy_mods 调用 ga.kill_all_code_run_processes; 无 ga 模块
    (测试/无凭据环境)时静默跳过。
    round10 审查(B4): 返回 False 表示清理不干净(fail-closed)——调用方必须
    把任务标记为失败(error_code=SUBPROCESS_CLEANUP_FAILED), Platform 对失败
    任务销毁 Runner, 残留进程不随快照持久化。
    """
    mods = getattr(adapter, "_legacy_mods", None) or {}
    ga_mod = mods.get("ga")
    if ga_mod is not None:
        kill_all = getattr(ga_mod, "kill_all_code_run_processes", None)
        if callable(kill_all):
            try:
                return bool(kill_all())
            except Exception:
                return False  # 清理异常同样 fail-closed
    return True


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
    cleanup_legacy_subprocesses(adapter)
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
    cleanup_legacy_subprocesses(adapter)
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
    cleanup_legacy_subprocesses(adapter)
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
        # 取消/超时路径同样清理残留(cancel/timeout 终态由 Platform evict
        # Runner, 清理失败由容器销毁兜底, 无需 fail-closed)。
        cleanup_legacy_subprocesses(adapter)
    else:
        if generated:
            state.final_body = append_missing_file_markers(state.final_body, generated)
        # round10 审查(B4): 成功路径必须确认子进程清理干净——残留进程可窃取
        # 下一任务凭据或继续写工作区, 不得复用 Runner。清理失败时任务判失败
        # (error_code=SUBPROCESS_CLEANUP_FAILED), Platform 对失败任务销毁
        # Runner(fail-closed)。
        if not cleanup_legacy_subprocesses(adapter):
            term = adapter._terminal(
                task.task_id, worker_pb2.TASK_FAILED,
                user_message="task subprocess cleanup failed; runner will be discarded",
                error_code="SUBPROCESS_CLEANUP_FAILED", result_body=state.final_body,
            )
            adapter._record_completed(task, term, state.final_body, state.display_history, state.agent)
            yield worker_pb2.WorkerEvent(terminal=term)
            state.terminal_emitted = True
            return
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
    cleanup_legacy_subprocesses(adapter)
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
    # 审查 C1/I8: agent 未捕获异常崩溃的终态路径同样必须清理 code_run
    # 残留进程组——这是最可能遗留后台进程的路径(agent 中断时任务未正常收尾)。
    cleanup_legacy_subprocesses(adapter)
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
