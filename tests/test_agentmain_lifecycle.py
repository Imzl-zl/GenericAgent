import queue
import threading
import time
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
    return agent


def test_shutdown_stops_idle_runner_and_closes_agent():
    agent = _minimal_agent()
    runner = threading.Thread(target=agentmain.GenericAgent.run, args=(agent,), daemon=True)
    runner.start()

    deadline = time.time() + 1
    while agent._runner_thread is None and time.time() < deadline:
        time.sleep(0.01)

    agent.shutdown(join_timeout=1.0)
    runner.join(timeout=1.0)

    assert not runner.is_alive()
    with pytest.raises(RuntimeError, match="shut down"):
        agent.put_task("hello")


def test_shutdown_rejects_queued_work_without_executing_it():
    agent = _minimal_agent()
    output = queue.Queue()
    agent.task_queue.put({"query": "hello", "source": "user", "images": [], "output": output})
    agent._shutdown = True

    runner = threading.Thread(target=agentmain.GenericAgent.run, args=(agent,), daemon=True)
    runner.start()
    agent.task_queue.put("STOP")
    runner.join(timeout=1.0)

    item = output.get(timeout=1.0)
    assert item["done"] == ""
    assert "shutting down" in item["error"]
    assert not runner.is_alive()


def test_max_turns_exceeded_is_reported_to_worker(monkeypatch):
    class FakeHandler:
        def __init__(self, _parent, history, _temp_dir):
            self.history_info = history
            self.working = {}
            self.code_stop_signal = []

    def maxed_runner(*_args, **_kwargs):
        yield {"turn": 1}
        yield "partial output"
        return {"result": "MAX_TURNS_EXCEEDED"}

    monkeypatch.setattr(agentmain, "GenericAgentHandler", FakeHandler)
    monkeypatch.setattr(agentmain, "agent_runner_loop", maxed_runner)
    monkeypatch.setattr(agentmain, "get_system_prompt", lambda: "system")

    agent = _minimal_agent()
    agent.history = []
    agent.extra_sys_prompts = []
    agent.peer_hint = False
    agent.llmclient = SimpleNamespace(
        log_path=None,
        backend=SimpleNamespace(extra_sys_prompt=""),
    )
    agent.log_path = ""
    agent.task_dir = None
    agent.force_non_stream = False
    agent.verbose = False
    agent.inc_out = True

    output = queue.Queue()
    agent.task_queue.put({"query": "long task", "source": "test", "images": [], "output": output})
    runner = threading.Thread(target=agentmain.GenericAgent.run, args=(agent,), daemon=True)
    runner.start()

    item = output.get(timeout=1.0)
    while "done" not in item:
        item = output.get(timeout=1.0)
    agent.task_queue.put("STOP")
    runner.join(timeout=1.0)

    assert item["done"] == "partial output"
    assert item["error_code"] == "MAX_TURNS_EXCEEDED"
    assert "turn limit" in item["error"]
    assert not runner.is_alive()


def test_llm_failed_nested_in_data_is_reported_to_worker(monkeypatch):
    """agent_loop 把 LLM_FAILED 包在 {'result':'EXITED','data':...} 内,
    agentmain 消费侧必须从 data 读取并映射 error_code=LLM_FAILED
    (回归 Round14 C2: 曾因判定顶层 result 恒 False 而把故障当成功)。"""
    class FakeHandler:
        def __init__(self, _parent, history, _temp_dir):
            self.history_info = history
            self.working = {}
            self.code_stop_signal = []

    def failed_runner(*_args, **_kwargs):
        yield {"turn": 1}
        yield "partial output"
        return {"result": "EXITED", "data": {"result": "LLM_FAILED", "msg": "repeated transport failures"}}

    monkeypatch.setattr(agentmain, "GenericAgentHandler", FakeHandler)
    monkeypatch.setattr(agentmain, "agent_runner_loop", failed_runner)
    monkeypatch.setattr(agentmain, "get_system_prompt", lambda: "system")

    agent = _minimal_agent()
    agent.history = []
    agent.extra_sys_prompts = []
    agent.peer_hint = False
    agent.llmclient = SimpleNamespace(
        log_path=None,
        backend=SimpleNamespace(extra_sys_prompt=""),
    )
    agent.log_path = ""
    agent.task_dir = None
    agent.force_non_stream = False
    agent.verbose = False
    agent.inc_out = True

    output = queue.Queue()
    agent.task_queue.put({"query": "long task", "source": "test", "images": [], "output": output})
    runner = threading.Thread(target=agentmain.GenericAgent.run, args=(agent,), daemon=True)
    runner.start()

    item = output.get(timeout=1.0)
    while "done" not in item:
        item = output.get(timeout=1.0)
    agent.task_queue.put("STOP")
    runner.join(timeout=1.0)

    assert item["done"] == "partial output"
    assert item["error_code"] == "LLM_FAILED"
    assert "repeated transport failures" in item["error"]
    assert not runner.is_alive()


def test_non_dict_data_without_llm_failed_is_success(monkeypatch):
    """正常完成时 exit_reason.data 是任意对象(如 do_no_tool 的 response),
    不得因 isinstance 非 dict 而抛错(Round14 C2 回归: data 为 MockResponse
    时曾 AttributeError 导致任务失败)。"""
    class FakeHandler:
        def __init__(self, _parent, history, _temp_dir):
            self.history_info = history
            self.working = {}
            self.code_stop_signal = []

    def normal_runner(*_args, **_kwargs):
        yield {"turn": 1}
        yield "done output"
        return {"result": "CURRENT_TASK_DONE", "data": SimpleNamespace(content="ok")}

    monkeypatch.setattr(agentmain, "GenericAgentHandler", FakeHandler)
    monkeypatch.setattr(agentmain, "agent_runner_loop", normal_runner)
    monkeypatch.setattr(agentmain, "get_system_prompt", lambda: "system")

    agent = _minimal_agent()
    agent.history = []
    agent.extra_sys_prompts = []
    agent.peer_hint = False
    agent.llmclient = SimpleNamespace(
        log_path=None,
        backend=SimpleNamespace(extra_sys_prompt=""),
    )
    agent.log_path = ""
    agent.task_dir = None
    agent.force_non_stream = False
    agent.verbose = False
    agent.inc_out = True

    output = queue.Queue()
    agent.task_queue.put({"query": "long task", "source": "test", "images": [], "output": output})
    runner = threading.Thread(target=agentmain.GenericAgent.run, args=(agent,), daemon=True)
    runner.start()

    item = output.get(timeout=1.0)
    while "done" not in item:
        item = output.get(timeout=1.0)
    agent.task_queue.put("STOP")
    runner.join(timeout=1.0)

    assert item["done"] == "done output"
    assert "error_code" not in item
    assert not runner.is_alive()
