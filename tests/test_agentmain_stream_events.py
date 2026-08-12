"""agentmain display 流事件转发回归测试(2026-08-12 输出分层配套)。

分层后非 verbose 输出只含用户可见文本, 轮次/工具活动以事件 dict 形式
转发: {'turn': N}(轮次边界, 前端切分消息 + worker 心跳推进)与
{'tool': name}(工具活动, worker 心跳保活)。本测试验证:
  1. turn 事件前先冲刷残留文本('next' 不跨轮次边界, 文本归属精确);
  2. turn 事件携带 outputs(turn_resps[-2:]), 兼容 wechatapp 消费方式;
  3. tool 事件原样转发(非用户文本, 消费方忽略)。
"""

import queue
import threading
from types import SimpleNamespace

import pytest

import agentmain


def _minimal_agent():
    agent = agentmain.GenericAgent.__new__(agentmain.GenericAgent)
    agent.lock = threading.Lock()
    agent.task_queue = queue.Queue()
    agent.is_running = False
    agent.stop_sig = False
    agent.handler = None
    agent._shutdown = False
    agent._runner_thread = None
    agent.history = []
    agent.extra_sys_prompts = []
    agent.peer_hint = False
    agent.log_path = ""
    agent.task_dir = None
    agent.force_non_stream = False
    agent.verbose = False
    agent.inc_out = True
    return agent


def _run_task(monkeypatch, runner, source="test"):
    class FakeHandler:
        def __init__(self, _parent, history, _temp_dir):
            self.history_info = history
            self.working = {}
            self.code_stop_signal = []

    monkeypatch.setattr(agentmain, "GenericAgentHandler", FakeHandler)
    monkeypatch.setattr(agentmain, "agent_runner_loop", runner)
    monkeypatch.setattr(agentmain, "get_system_prompt", lambda: "system")

    agent = _minimal_agent()
    agent.llmclient = SimpleNamespace(
        log_path=None,
        backend=SimpleNamespace(extra_sys_prompt=""),
    )
    output = queue.Queue()
    agent.task_queue.put({"query": "task", "source": source, "images": [], "output": output})
    thread = threading.Thread(target=agentmain.GenericAgent.run, args=(agent,), daemon=True)
    thread.start()

    items = []
    while True:
        item = output.get(timeout=2.0)
        items.append(item)
        if "done" in item:
            break
    agent.task_queue.put("STOP")
    thread.join(timeout=1.0)
    return items


def test_turn_events_preflush_and_outputs(monkeypatch):
    """turn 事件: 先冲刷上一轮残留文本, 再发事件; outputs=turn_resps[-2:]。"""

    def runner(*_args, **_kwargs):
        yield {"turn": 1}
        yield "第一轮回复内容\n"
        yield {"tool": "code_run"}
        yield {"turn": 2}
        yield "第二轮回复"
        return {"result": "EXITED"}

    items = _run_task(monkeypatch, runner)

    turns = [it for it in items if "turn" in it and "next" not in it and "done" not in it and "tool" not in it]
    assert [it["turn"] for it in turns] == [1, 2]
    # turn 事件 outputs = turn_resps[-2:]: turn2 事件的 outputs[0] 是第一轮
    # 完整文本(wechatapp 按 outputs[-2] 落定上一轮)。
    assert turns[1]["outputs"][0] == "第一轮回复内容\n"

    # 残留文本在 turn2 事件之前以 'next' 冲刷(turn=1, 不跨轮)。
    nexts = [it for it in items if "next" in it]
    pre_flush = nexts[0]
    assert pre_flush["turn"] == 1
    assert "第一轮回复内容" in pre_flush["next"]
    # 冲刷发生在 turn2 事件之前(文本归属精确)。
    turn2_index = next(i for i, it in enumerate(items) if "turn" in it and it["turn"] == 2 and "next" not in it and "done" not in it and "tool" not in it)
    assert items.index(pre_flush) < turn2_index

    done = items[-1]
    assert done["done"] == "第一轮回复内容\n第二轮回复"


def test_tool_events_forwarded(monkeypatch):
    """tool 事件原样转发(worker 心跳推进信号, 非用户文本)。"""

    def runner(*_args, **_kwargs):
        yield {"turn": 1}
        yield {"tool": "code_run"}
        yield {"tool": "file_patch"}
        yield "结果"
        return {"result": "EXITED"}

    items = _run_task(monkeypatch, runner)
    tools = [it for it in items if "tool" in it]
    assert [it["tool"] for it in tools] == ["code_run", "file_patch"]
    # tool 事件不携带 next/done, 消费方安全忽略。
    assert all("next" not in it and "done" not in it for it in tools)
