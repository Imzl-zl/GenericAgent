"""agent_loop 输出分层回归测试(2026-08-12 架构修复)。

verbose=True = 完整过程转录(TUI/CLI/根项目前端展示思考);
verbose=False = 只输出用户可见回复(租户 worker 交付, 不含轮次标记/
工具调用痕迹/工作记忆 <summary> 块)。此分层由 verbose 开关表达,
不是事后正则清洗——防止未来把过程内容混入非 verbose 输出。
"""

import sys
from pathlib import Path

import pytest

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))

from agent_loop import StepOutcome, agent_runner_loop


class _FakeClient:
    """最小 chat 生成器: yield 一个文本 chunk, StopIteration 返回 response。"""

    reply_content = "这是给用户的回复"

    def __init__(self):
        self.last_tools = ""

    def chat(self, messages, tools=None):
        class Resp:
            content = _FakeClient.reply_content
            tool_calls = []

        def gen():
            # 真实 llmcore 流式 chunk = 模型输出原文(含工作记忆 <summary>)。
            yield "<summary>快照</summary>\n\n"
            yield "这是给用户的回复"
            return Resp()

        return gen()


class _FakeHandler:
    class Parent:
        task_dir = None  # 模拟 worker 环境(无 CLI task 目录)

    parent = Parent()
    max_turns = 3
    current_turn = 0
    _done_hooks = []

    def turn_end_callback(self, *args):
        return ""

    def dispatch(self, tool_name, args, response, **kwargs):
        def g():
            yield None
            return StepOutcome(should_exit=True, next_prompt="", data=None)

        return g()


def _run(verbose):
    out = list(
        agent_runner_loop(
            _FakeClient(), "sys", "hi", _FakeHandler(), [],
            verbose=verbose, yield_info=True,
        )
    )
    return "".join(c for c in out if isinstance(c, str))


def test_non_verbose_is_user_facing_only():
    text = _run(verbose=False)
    # 轮次标记/工具痕迹/summary 工作记忆不得出现。
    assert "LLM Running" not in text
    assert "Turn " not in text
    assert "🛠️" not in text
    assert "<summary>" not in text
    # 用户可见回复保留。
    assert "这是给用户的回复" in text


def test_verbose_is_full_transcript():
    text = _run(verbose=True)
    # 完整转录包含轮次标记与模型原文(含 <summary> 工作记忆)。
    assert "LLM Running (Turn 1)" in text
    assert "<summary>" in text
    assert "这是给用户的回复" in text


def test_tool_event_only_in_non_verbose():
    """工具活动事件({'tool': name})只发非 verbose(yield_info 模式):
    verbose 已有完整工具文本痕迹, 事件是 worker 心跳保活信号。"""
    def _tool_call(name="code_run"):
        import json as _json
        from types import SimpleNamespace as _SN
        return [_SN(function=_SN(name=name, arguments=_json.dumps({"script": "ls"})), id="1")]

    class ToolClient:
        reply_content = "回复"

        def __init__(self):
            self.last_tools = ""

        def chat(self, messages, tools=None):
            class Resp:
                content = ToolClient.reply_content
                tool_calls = _tool_call()

            def gen():
                yield "回复"
                return Resp()

            return gen()

    class ToolHandler(_FakeHandler):
        def dispatch(self, tool_name, args, response, **kwargs):
            def g():
                yield None
                return StepOutcome(should_exit=False, next_prompt="继续", data=None)

            return g()

    def run(verbose):
        out = []
        for chunk in agent_runner_loop(
            ToolClient(), "sys", "hi", ToolHandler(), [],
            verbose=verbose, yield_info=True, max_turns=2,
        ):
            out.append(chunk)
        return out

    events = run(verbose=False)
    tools = [e for e in events if isinstance(e, dict) and "tool" in e]
    assert tools and all(t["tool"] == "code_run" for t in tools)
    # 非 verbose 无工具文本痕迹
    text = "".join(e for e in events if isinstance(e, str))
    assert "🛠️" not in text
    # verbose 不产生 tool 事件(文本痕迹足够)
    events_v = run(verbose=True)
    assert not any(isinstance(e, dict) and "tool" in e for e in events_v)
    assert any("🛠️" in e for e in events_v if isinstance(e, str))
