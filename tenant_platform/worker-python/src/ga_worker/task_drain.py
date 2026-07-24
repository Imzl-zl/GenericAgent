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
from ga_worker.task_terminal import emit_cancel_or_timeout_terminal

# P-M8: named constants (no magic numbers).
QUEUE_POLL_TIMEOUT_S = 0.1
ABORT_WAIT_TIMEOUT_S = 0.5
HANDLER_WRAP_RETRIES = 100
HANDLER_WRAP_SLEEP_S = 0.01


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
    """Handle queue.Empty: re-wrap handler, check cancel/timeout. Returns True if should break."""
    from ga_worker.legacy_instrument import install_handler_print_counter
    h = getattr(state.agent, "handler", None)
    if h is not None and not getattr(h, "_adapter_print_wrapped", False):
        install_handler_print_counter(state.agent, state.count_fn, adapter._legacy_mods)
    if not (pending.cancel_requested or state.timed_out["v"]):
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
            term = adapter._terminal(
                task.task_id, worker_pb2.TASK_FAILED,
                user_message="max_output_bytes exceeded", error_code="MAX_OUTPUT_BYTES",
            )
            adapter._record_completed(
                task, term, text[:max(0, state.max_output)], state.display_history, state.agent,
            )
            yield worker_pb2.WorkerEvent(terminal=term)
            state.terminal_emitted = True
        return True
    state.display_history.append({"text": text, "turn": turn})
    with adapter._lock:
        if adapter._session is not None:
            adapter._session.display_history.append({"text": text, "turn": turn})
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
    if error_msg:
        yield from emit_error_terminal(adapter, task, state, error_msg)
        return
    if state.output_exceeded["v"]:
        yield from emit_output_exceeded_terminal(adapter, task, state)
        return
    yield from emit_final_terminal(adapter, task, state)
