"""Unit tests for the drain-loop heartbeat policy (Round 7 I8).

审查 C1(I8): 心跳必须来自实际任务推进点——drain 线程空轮询时, 若 agent
长时间无 display 事件(卡在 LLM/工具 I/O), 不得再发心跳刷新平台侧
last_activity_at, 否则 idle reaper 永远无法识别死锁。
"""

from __future__ import annotations

import queue
import threading
import time

from genericagent.worker.v1 import worker_pb2

from ga_worker import task_drain
from ga_worker.state import PendingTask, TaskRunState


def _make_state(last_progress_ago: float) -> TaskRunState:
    pending = PendingTask(task_id="t1", request=None)
    state = TaskRunState(pending=pending, agent=None)
    state.last_progress_at = time.monotonic() - last_progress_ago
    return state


def _empty_poll(state: TaskRunState):
    task = worker_pb2.TaskEnvelope(task_id="t1")
    q = queue.Queue()
    return list(task_drain._handle_empty_poll(None, task, state.pending, state, q))


def _has_heartbeat(events) -> bool:
    return any(e.HasField("chunk") and e.chunk.text == "" for e in events)


def test_empty_poll_skips_heartbeat_when_agent_stalled(monkeypatch):
    """Agent 卡死(超窗口无 display 事件): 空轮询不得产出心跳。"""
    monkeypatch.setattr(task_drain, "PROGRESS_WINDOW_S", 0.0)
    state = _make_state(last_progress_ago=10.0)
    assert not _has_heartbeat(_empty_poll(state))
    assert state.last_heartbeat_at == 0.0  # 心跳未发出


def test_empty_poll_emits_heartbeat_when_agent_making_progress(monkeypatch):
    """Agent 在推进(窗口内有 display 事件): 空轮询照常发心跳。"""
    monkeypatch.setattr(task_drain, "PROGRESS_WINDOW_S", 60.0)
    state = _make_state(last_progress_ago=1.0)
    assert _has_heartbeat(_empty_poll(state))
    assert state.last_heartbeat_at > 0.0


def _fake_adapter():
    class _FA:
        def __init__(self):
            self._lock = threading.Lock()
            self._session = None
            self.terminals = []

        def _terminal(self, task_id, status, user_message="", error_code="", result_body=None):
            term = worker_pb2.Terminal(task_id=task_id, status=status, user_message=user_message)
            if error_code:
                term.error.CopyFrom(worker_pb2.TerminalError(code=error_code))
            self.terminals.append(term)
            return term

        def _record_completed(self, task, term, final_body, display_history, agent):
            pass

    return _FA()


def test_drain_queue_item_receipt_refreshes_progress_window():
    """审查 C1/I8 接线: 正常 drain 路径取到 display item 必须刷新心跳窗口
    (推进点), 而非只在 cancel/timeout 分支更新——否则复用 Worker(运行 >150s)
    永远不发心跳, 正常长任务被误收割。"""
    adapter = _fake_adapter()
    q = queue.Queue()
    q.put({"next": "hello", "turn": 1})
    q.put({"done": "ok", "turn": 1})
    task = worker_pb2.TaskEnvelope(task_id="t1")
    pending = PendingTask(task_id="t1", request=None)
    state = TaskRunState(pending=pending, agent=None)
    state.last_progress_at = 0.0  # 初始 0(模拟复用 Worker 的陈旧窗口)
    events = list(task_drain.drain_display_queue(adapter, task, pending, state, q))
    assert time.monotonic() - state.last_progress_at < 1.0, "item receipt must refresh last_progress_at"
    assert any(e.HasField("terminal") for e in events)
