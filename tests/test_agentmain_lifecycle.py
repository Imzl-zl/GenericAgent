import queue
import threading
import time

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
