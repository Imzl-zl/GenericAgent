"""共享测试 fixture(2026-08-26 O7): minimal GenericAgent 构造工厂。

此前 _minimal_agent 在 test_agentmain_lifecycle.py / test_agentmain_stream_events.py
重复手写, 新增 agent 字段要同步多处改(本次 all_outputs 踩了 5 个测试回归)。
统一到 conftest, 覆盖 GenericAgent.__init__ 全部字段。用法:
    def test_x(minimal_agent, ...):  # fixture 注入
或:
    from conftest import make_minimal_agent
"""

import queue
import threading

import pytest

import agentmain


def make_minimal_agent():
    """__new__ 绕过 __init__(不读 mykey / 不建 temp 目录 / 不加载 LLM sessions),
    补齐 GenericAgent.__init__ 全部字段。llmclient 由各测试按需注入 fake。"""
    agent = agentmain.GenericAgent.__new__(agentmain.GenericAgent)
    agent.lock = threading.Lock()
    agent.task_dir = None
    agent.history = []
    agent.handler = None
    agent.all_outputs = []
    agent.task_queue = queue.Queue()
    agent.is_running = False
    agent.stop_sig = False
    agent.llm_no = 0
    agent.inc_out = False
    agent.verbose = True
    agent.peer_hint = True
    agent.force_non_stream = False
    agent._shutdown = False
    agent._runner_thread = None
    agent._current_queue = None
    agent.log_path = ""
    agent.llmclient = None
    agent.extra_sys_prompts = []
    agent.intervene = None
    agent.extrakeyinfo = None
    return agent


@pytest.fixture
def minimal_agent():
    """每个测试独立的 minimal GenericAgent。"""
    return make_minimal_agent()