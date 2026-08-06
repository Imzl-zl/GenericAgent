"""Round16 回归测试: GA 侧故障语义与 token 计数修复。"""

from types import SimpleNamespace

import pytest

import ga
import llmcore


class _FakeBackend:
    name = "test"
    model = "test-model"


def test_retry_or_exit_non_fatal_marks_llm_failed_after_three():
    """Round16-G1: 非 fatal 分支(空响应/max_tokens)连续 3 次也必须标记
    LLM_FAILED——旧实现返回空 dict, agentmain 判定恒 False, 空响应任务被
    当作成功提交(空结果 TASK_SUCCEEDED), 与 C2 修复意图相悖。"""
    handler = ga.GenericAgentHandler.__new__(ga.GenericAgentHandler)
    handler._empty_ct = 2
    outcome = handler._retry_or_exit("[System] Blank response, regenerate and tooluse")
    assert outcome.should_exit is True
    assert outcome.data.get("result") == "LLM_FAILED"
    assert "Blank response" in outcome.data.get("msg", "")


def test_retry_or_exit_returns_continue_before_limit():
    handler = ga.GenericAgentHandler.__new__(ga.GenericAgentHandler)
    handler._empty_ct = 1
    outcome = handler._retry_or_exit("prompt")
    assert outcome.should_exit is False
    assert outcome.next_prompt == "prompt"


def test_total_cd_tokens_scales_linearly_not_quadratically():
    """Round16-G2: _build_protocol_prompt 的 total_cd_tokens 只累计本条消息
    增量。旧实现把累积拼接的整个 user 字符串重复相加(O(n²)): 10 条 1000
    字符消息 total≈18348 越过 9000 阈值, 工具描述每轮重新注入; 修复后
    线性 ≈3330, 阈值不再被无意义触发。"""
    client = llmcore.ToolClient.__new__(llmcore.ToolClient)
    client.backend = _FakeBackend()
    client.auto_save_tokens = False
    client.last_tools = ""
    client.total_cd_tokens = 0
    client.log_path = None
    messages = [{"role": "system", "content": "sys"}] + [
        {"role": "user", "content": "x" * 1000} for _ in range(10)
    ]
    client._build_protocol_prompt(messages, tools=None)
    # 线性累计: 10 * (1000//3) ≈ 3330, 远低于 9000
    assert client.total_cd_tokens < 9000
    assert client.total_cd_tokens == 10 * (1000 // 3)
