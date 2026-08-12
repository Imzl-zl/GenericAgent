"""2026-08-12 空结果防护回归测试: GA 核心对退化 LLM 响应的处理。

生产实证: 上游模型(relay)异常慢 + 退化响应(仅 <summary>/thinking 或
空白 content)时, 旧实现把"无用户可见文本"的轮次当作正常完成提交
(TASK_SUCCEEDED 空 body), 平台回"任务完成：任务已完成"而用户没得到
任何实际回答(QQ/飞书 14:16 实测)。

修复分层:
  * ga.py do_no_tool: content 剥掉 <summary>/<thinking> 后无可见文本
    → 视为空白响应计入 _empty_ct 重试, 3 次后结构化 LLM_FAILED。
  * llmcore.MixinSession.raw_ask: 上游返回无 text/tool_use 产出的空结果
    → 自动切换下一个 session(原生模型降级切换能力, 此前只对 !!!Error
    切换)。
"""

from types import SimpleNamespace

import pytest

import ga
import llmcore


class _FakeResponse:
    def __init__(self, content="", thinking=""):
        self.content = content
        self.thinking = thinking


def _make_handler(empty_ct=0):
    handler = ga.GenericAgentHandler.__new__(ga.GenericAgentHandler)
    handler._empty_ct = empty_ct
    handler.working = {}
    handler.current_turn = 1
    return handler


def _run_gen(gen):
    """耗尽生成器, 返回其 return 值(StepOutcome)。"""
    try:
        while True:
            next(gen)
    except StopIteration as stop:
        return stop.value
    return None


def test_do_no_tool_summary_only_is_blank_response():
    """仅 <summary> 的响应必须视为空白(此前被当作正常完成 → 空结果)。"""
    handler = _make_handler()
    outcome = _run_gen(handler.do_no_tool({}, _FakeResponse(content="<summary>直接回答了用户问题</summary>")))
    assert outcome.should_exit is False
    assert outcome.next_prompt == "[System] Blank response, regenerate and tooluse"
    assert handler._empty_ct == 1


def test_do_no_tool_thinking_only_is_blank_response():
    """仅 thinking/空白 content 的响应同样视为空白(上游退化典型形态)。"""
    handler = _make_handler()
    outcome = _run_gen(handler.do_no_tool({}, _FakeResponse(content="", thinking="用户问时间, 我应该回答...")))
    assert outcome.should_exit is False
    assert outcome.next_prompt == "[System] Blank response, regenerate and tooluse"
    assert handler._empty_ct == 1


def test_do_no_tool_summary_with_trailing_newline_is_blank():
    """summary-only + 尾部换行: 旧 endswith('</summary>') 检查抓不到, 新检查覆盖。"""
    handler = _make_handler()
    outcome = _run_gen(handler.do_no_tool({}, _FakeResponse(content="<summary>快照</summary>\n\n")))
    assert outcome.should_exit is False
    assert handler._empty_ct == 1


def test_do_no_tool_visible_text_passes_through():
    """含真实可见文本的回复不受影响(正常完成路径)。"""
    handler = _make_handler()
    resp = _FakeResponse(content="<summary>快照</summary>\n现在是 14:26 分。")
    outcome = _run_gen(handler.do_no_tool({}, resp))
    assert outcome.should_exit is False
    assert outcome.next_prompt is None
    assert outcome.data is resp
    assert handler._empty_ct == 0


def test_do_no_tool_blank_threshold_reaches_llm_failed():
    """连续 3 次空响应必须走结构化 LLM_FAILED(而非空成功)。"""
    handler = _make_handler(empty_ct=2)
    outcome = _run_gen(handler.do_no_tool({}, _FakeResponse(content="<summary>仅摘要</summary>")))
    assert outcome.should_exit is True
    assert outcome.data.get("result") == "LLM_FAILED"


# ── MixinSession 空结果切换 ──────────────────────────────────────────────


class _FakeSession:
    """非 native 假 session: raw_ask 按预设产出/返回。"""

    def __init__(self, name, chunks, blocks):
        self.name = name
        self._chunks = chunks
        self._blocks = blocks
        self.max_retries = 0
        self.history = []
        self.system = ""
        self.tools = None

    def raw_ask(self, messages):
        for c in self._chunks:
            yield c
        return self._blocks

    def make_messages(self, messages):
        return messages

    def ask(self, prompt):
        yield from self.raw_ask([{"role": "user", "content": prompt}])


def _make_mixin(sessions, llm_nos, retries=3):
    clients = [SimpleNamespace(backend=s) for s in sessions]
    cfg = {"llm_nos": llm_nos, "max_retries": retries, "base_delay": 0.01}
    return llmcore.MixinSession(clients, cfg)


def test_mixin_switches_session_on_empty_output():
    """s1 空结果(无 text/tool_use) → 自动切换到 s2。"""
    s1 = _FakeSession("s1", [], [{"type": "thinking", "thinking": "思考..."}])
    s2 = _FakeSession("s2", ["ok"], [{"type": "text", "text": "ok"}])
    ms = _make_mixin([s1, s2], ["s1", "s2"])
    blocks = list(ms.raw_ask([{"role": "user", "content": "hi"}]))
    assert blocks == ["ok"]
    assert ms._cur_idx == 1
    assert ms.current.name == "s2"


def test_mixin_switches_session_on_error_and_keeps_error_text():
    """!!!Error 场景保持既有切换行为(回归)。"""
    s1 = _FakeSession("s1", ["!!!Error: HTTP 503"], [{"type": "text", "text": "!!!Error: HTTP 503"}])
    s2 = _FakeSession("s2", ["fine"], [{"type": "text", "text": "fine"}])
    ms = _make_mixin([s1, s2], ["s1", "s2"])
    blocks = list(ms.raw_ask([{"role": "user", "content": "hi"}]))
    assert blocks == ["fine"]
    assert ms._cur_idx == 1


def test_mixin_empty_output_all_exhausted_returns_empty():
    """所有 session 都空结果: 返回空(由 agent 空白重试逻辑兜底), 不抛错。"""
    s1 = _FakeSession("s1", [], [])
    s2 = _FakeSession("s2", [], [])
    ms = _make_mixin([s1, s2], ["s1", "s2"], retries=1)
    blocks = list(ms.raw_ask([{"role": "user", "content": "hi"}]))
    assert blocks == []
    assert ms._cur_idx == 0  # 回弹主 session(无成功切换)


def test_mixin_spring_back_to_primary_after_switch():
    """切换成功后 _pick 在 spring 窗口后回弹主 session(既有语义回归)。"""
    s1 = _FakeSession("s1", [], [])
    s2 = _FakeSession("s2", ["ok"], [{"type": "text", "text": "ok"}])
    ms = _make_mixin([s1, s2], ["s1", "s2"])
    list(ms.raw_ask([{"role": "user", "content": "hi"}]))
    assert ms._cur_idx == 1
    ms._switched_at = 0.0  # 模拟 spring 窗口过期
    assert ms._pick() == 0
