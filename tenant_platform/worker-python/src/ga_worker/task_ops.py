"""Task/abort/terminal helpers for ManagedAgentAdapter.

Extracted from managed_agent.py (B3: file size limit). Mixed into the adapter
so `self` still refers to the full adapter instance. Covers: terminal building,
completed-task recording, abort paths, file-based pre-dispatch barrier, and
active-task cleanup.
"""

from __future__ import annotations

import copy
import os
import time
from pathlib import Path
from typing import Any

from genericagent.worker.v1 import worker_pb2

from ga_worker.checkpoint import result_digest_for
from ga_worker.state import CompletedTask, PendingTask

# P-M8: named constants (no magic numbers).
TERMINAL_MSG_MAX_LEN = 4000
FILE_BARRIER_DEADLINE_S = 10.0
FILE_BARRIER_POLL_S = 0.02


class TaskOpsMixin:
    """Terminal construction, completion recording, abort, and active-task cleanup.

    Depends on instance attributes set by ManagedAgentAdapter.__init__:
    _lock, _session, _pending.
    """

    def _file_pre_dispatch_barrier(self, task_id: str, pending: PendingTask) -> None:
        barrier_dir = os.environ.get("GA_TEST_PRE_DISPATCH_BARRIER_DIR", "").strip()
        if not barrier_dir:
            return
        root = Path(barrier_dir)
        if not (root / f"{task_id}.wait").exists():
            return
        try:
            root.mkdir(parents=True, exist_ok=True)
            reserved = root / f"{task_id}.reserved"
            proceed = root / f"{task_id}.proceed"
            pending.reserved.set()
            reserved.write_text("1", encoding="utf-8")
            deadline = time.time() + FILE_BARRIER_DEADLINE_S
            while time.time() < deadline:
                if pending.cancel_requested or proceed.exists():
                    break
                time.sleep(FILE_BARRIER_POLL_S)
        except Exception:
            return

    def _abort_once(self, pending: PendingTask | None, agent: Any) -> None:
        with self._lock:
            self._abort_once_locked(pending, agent)

    def _abort_once_locked(self, pending: PendingTask | None, agent: Any) -> None:
        if pending is not None:
            if pending.abort_called:
                return
            pending.abort_called = True
        try:
            agent.abort()
        except Exception:
            pass

    def _abort_session_task(self, task_id: str, agent: Any) -> None:
        """P-M3: fallback abort when pending is cleared but active_task_id remains."""
        flag = getattr(self._session, "_abort_once_flag", None)  # type: ignore[union-attr]
        if flag is None:
            self._session._abort_once_flag = set()  # type: ignore[attr-defined]
            flag = self._session._abort_once_flag  # type: ignore[attr-defined]
        if task_id not in flag:
            flag.add(task_id)
            try:
                agent.abort()
            except Exception:
                pass

    def _terminal(
        self, task_id: str, status: int, *,
        user_message: str = "", error_code: str | None = None,
        result_body: str | None = None,
    ) -> worker_pb2.Terminal:
        body = result_body if result_body is not None else user_message
        rd = result_digest_for(body) if status == worker_pb2.TASK_SUCCEEDED else (
            result_digest_for(body) if body else ""
        )
        term = worker_pb2.Terminal(
            task_id=task_id, status=status, result_digest=rd,
            user_message=user_message[:TERMINAL_MSG_MAX_LEN],
        )
        if error_code and status != worker_pb2.TASK_SUCCEEDED:
            term.error.code = error_code
            term.error.user_message = user_message[:TERMINAL_MSG_MAX_LEN]
        return term

    def _record_completed(
        self, task: worker_pb2.TaskEnvelope, term: worker_pb2.Terminal,
        result_body: str, display_history: list[dict[str, Any]], agent: Any,
    ) -> None:
        working = self._extract_working(agent)
        agent_history = copy.deepcopy(list(getattr(agent, "history", []) or []))
        backend_history = self._get_backend_history(agent)
        completed = CompletedTask(
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
                self._session.seed_working = copy.deepcopy(working)
                self._session.completed = completed
                self._session.active_task_id = None
                if self._pending and self._pending.task_id == task.task_id:
                    self._pending = None

    def _extract_working(self, agent: Any) -> dict[str, Any]:
        working: dict[str, Any]
        if getattr(agent, "handler", None) is not None and hasattr(agent.handler, "working"):
            working = copy.deepcopy(agent.handler.working)
        elif hasattr(agent, "_adapter_seed_working"):
            working = copy.deepcopy(agent._adapter_seed_working)
        else:
            working = {}
        # 项目激活态(方案 §5: "项目激活态"随成功 task 的 working 一起进入
        # staging state): 项目模式在重启恢复后必须保持激活。
        project_name = getattr(agent, "_ga_project_mode_name", None)
        if project_name:
            working["_ga_project_mode_name"] = project_name
        return working

    def _clear_active_locked(self, task_id: str) -> None:
        if self._pending and self._pending.task_id == task_id:
            self._pending = None
        if self._session and self._session.active_task_id == task_id:
            self._session.active_task_id = None
