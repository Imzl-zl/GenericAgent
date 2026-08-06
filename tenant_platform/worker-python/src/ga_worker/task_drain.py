"""Display-queue drain loop for task execution.

Extracted from task_runner.py (B3: file size limit). Polls the legacy agent's
display queue, re-wraps the print counter when the handler resets, and dispatches
each item to the next/done handlers or the cancel/timeout terminal path.
"""

from __future__ import annotations

import queue
import time
from typing import Any, Iterator

from genericagent.worker.v1 import worker_pb2

from ga_worker.state import PendingTask, TaskRunState
from ga_worker.task_terminal import emit_cancel_or_timeout_terminal, emit_output_exceeded_terminal

# P-M8: named constants (no magic numbers).
QUEUE_POLL_TIMEOUT_S = 0.1
ABORT_WAIT_TIMEOUT_S = 0.5
HANDLER_WRAP_RETRIES = 100
HANDLER_WRAP_SLEEP_S = 0.01

# Worker-side heartbeat: while the display queue is empty (LLM still thinking,
# large file processing, between tool calls), emit an empty-text Chunk so the
# platform's scheduler can refresh tasks.last_activity_at. This is the
# Temporal-HeartbeatTimeout pattern: as long as the Worker is alive and polling,
# last_activity_at stays fresh; if the Worker deadlocks (GIL, hung I/O), the
# reaper finalizes the task after TASK_IDLE_TIMEOUT_SECONDS.
# Empty-text Chunk is used as the heartbeat carrier to avoid a proto change
# (no protoc on Windows dev). Go scheduler identifies text=="" as heartbeat.
HEARTBEAT_INTERVAL_S = 30.0
# 审查 C1/I8: 心跳是**推进信号**而非纯存活信号——drain 空轮询只在自上次
# 真实 display 事件(last_progress_at)不超过 PROGRESS_WINDOW_S 时发心跳。
# agent 卡死在 LLM/工具 I/O 时无新事件, 心跳停止, 平台 idle reaper 按
# last_activity_at 收割(默认 idle 300s > 窗口)。长 LLM 思考由 llm-proxy
# 响应头超时(默认 120s)兜底: 思考超时必然返回/报错, agent 恢复推进,
# 不会误收割。
# 部署契约(审查 F4): 必须保持 PROGRESS_WINDOW_S(150s) > llm-proxy
# defaultResponseHeaderTimeout(120s) < TASK_IDLE_TIMEOUT_SECONDS(默认 300s)。
# 违反任一侧: 窗口 <= 代理超时 → 思考恰好超窗但代理未返回时心跳停发;
# 代理超时 >= idle 阈值 → 长思考任务被 idle reaper 误收割。
PROGRESS_WINDOW_S = 150.0


def _emit_heartbeat(task: worker_pb2.TaskEnvelope, state: TaskRunState) -> worker_pb2.WorkerEvent:
    """Emit an empty-text Chunk as a heartbeat signal (see HEARTBEAT_INTERVAL_S)."""
    state.last_heartbeat_at = time.monotonic()
    return worker_pb2.WorkerEvent(
        chunk=worker_pb2.Chunk(task_id=task.task_id, text="", turn=0)
    )


def put_task_and_wrap_handler(
    adapter: Any, task: worker_pb2.TaskEnvelope, pending: PendingTask, state: TaskRunState,
) -> queue.Queue:
    from ga_worker.legacy_instrument import install_handler_print_counter
    agent = state.agent
    display_q = agent.put_task(task.prompt, source=task.source or "user")
    for _ in range(HANDLER_WRAP_RETRIES):
        h = getattr(agent, "handler", None)
        if h is not None and not getattr(h, "_adapter_print_wrapped", False):
            install_handler_print_counter(agent, state.count_fn, adapter._legacy_mods)
        if h is not None and getattr(h, "_adapter_print_wrapped", False):
            break
        if not display_q.empty():
            break
        time.sleep(HANDLER_WRAP_SLEEP_S)
    return display_q


def drain_display_queue(
    adapter: Any, task: worker_pb2.TaskEnvelope, pending: PendingTask,
    state: TaskRunState, display_q: queue.Queue,
) -> Iterator[worker_pb2.WorkerEvent]:
    while True:
        try:
            item = display_q.get(timeout=QUEUE_POLL_TIMEOUT_S)
        except queue.Empty:
            should_break = yield from _handle_empty_poll(adapter, task, pending, state, display_q)
            if should_break:
                return
            continue
        # 审查 C1/I8: 取到真实 display 事件 = 任务推进点, 刷新心跳窗口
        # (覆盖 next/done 两条路径; 不能在 cancel/timeout 分支更新——
        # 那个分支心跳本就停发, 会让复用 Worker 永远无法发心跳)。
        state.last_progress_at = time.monotonic()
        if "next" in item:
            should_break = yield from _handle_next_item(adapter, task, state, item)
            if should_break:
                return
            continue
        if "done" in item:
            yield from _handle_done_item(adapter, task, state, item)
            return


def _handle_empty_poll(
    adapter: Any, task: worker_pb2.TaskEnvelope, pending: PendingTask,
    state: TaskRunState, display_q: queue.Queue,
) -> Iterator[worker_pb2.WorkerEvent]:
    """Handle queue.Empty: re-wrap handler, check cancel/timeout, emit heartbeat.

    Returns True if should break.
    """
    from ga_worker.legacy_instrument import install_handler_print_counter
    h = getattr(state.agent, "handler", None)
    if h is not None and not getattr(h, "_adapter_print_wrapped", False):
        install_handler_print_counter(state.agent, state.count_fn, adapter._legacy_mods)
    # Heartbeat: keep platform's tasks.last_activity_at fresh while LLM is
    # thinking or files are processing. Skipped when cancel/timeout is pending
    # (terminal path takes over). 审查 C1/I8: 仅当 agent 在推进窗口内
    # (最近有 display 事件)才发心跳——卡死时停发, 让 idle reaper 收割。
    if not (pending.cancel_requested or state.timed_out["v"]):
        now = time.monotonic()
        if now - state.last_progress_at <= PROGRESS_WINDOW_S and now - state.last_heartbeat_at >= HEARTBEAT_INTERVAL_S:
            yield _emit_heartbeat(task, state)
        return False  # continue polling
    try:
        item = display_q.get(timeout=ABORT_WAIT_TIMEOUT_S)
    except queue.Empty:
        if not state.terminal_emitted:
            yield from emit_cancel_or_timeout_terminal(adapter, task, state)
        return True  # break
    # Re-process the item we just got.
    if "next" in item:
        yield from _handle_next_item(adapter, task, state, item)
    elif "done" in item:
        yield from _handle_done_item(adapter, task, state, item)
    return True  # break


def _handle_next_item(
    adapter: Any, task: worker_pb2.TaskEnvelope, state: TaskRunState, item: dict,
) -> Iterator[worker_pb2.WorkerEvent]:
    """Handle 'next' chunk. Returns True if should break (output exceeded)."""
    text = item.get("next") or ""
    turn = int(item.get("turn") or 0)
    if state.count_fn is not None and state.count_fn(text):
        if not state.terminal_emitted:
            state.final_body = text[:max(0, state.max_output)]
            yield from emit_output_exceeded_terminal(adapter, task, state)
        return True
    state.display_history.append({"text": text, "turn": turn})
    # round9 审查: 不再双写 session.display_history(从未被消费, 复用 Runner
    # 时按任务线性泄漏内存); 展示历史只属于当前 TaskRunState。
    yield worker_pb2.WorkerEvent(
        chunk=worker_pb2.Chunk(task_id=task.task_id, text=text, turn=turn)
    )
    return False


def _handle_done_item(
    adapter: Any, task: worker_pb2.TaskEnvelope, state: TaskRunState, item: dict,
) -> Iterator[worker_pb2.WorkerEvent]:
    """Handle 'done' item: B1 error check, output check, cancel/timeout, success."""
    from ga_worker.task_terminal import (
        emit_error_terminal, emit_final_terminal, emit_output_exceeded_terminal,
    )
    state.final_body = item.get("done") or ""
    state.final_turn = int(item.get("turn") or 0)
    error_msg = item.get("error")
    error_code = item.get("error_code")
    if error_msg:
        yield from emit_error_terminal(adapter, task, state, error_msg, error_code)
        return
    if state.output_exceeded["v"]:
        yield from emit_output_exceeded_terminal(adapter, task, state)
        return
    yield from emit_final_terminal(adapter, task, state)
