"""reply_clean 单测: 过程转录 → 用户可见文本。"""

import pytest

from ga_worker.reply_clean import clean_reply_text


def test_strips_summary_block():
    raw = "<summary>用户在问在干嘛</summary>\n\n好的，我在待命。"
    assert clean_reply_text(raw) == "好的，我在待命。"


def test_strips_multiline_summary():
    raw = "<summary>第一行\n第二行</summary>\n回复"
    assert clean_reply_text(raw) == "回复"


def test_strips_turn_marker_plain():
    raw = "\nTurn 2 ...\n\nhello"
    assert clean_reply_text(raw) == "hello"


def test_strips_turn_marker_llm_running():
    raw = "\nLLM Running (Turn 3) ...\n\nworld"
    assert clean_reply_text(raw) == "world"


def test_strips_turn_marker_bold():
    raw = "\n**Turn 4 ...**\n\nbold"
    assert clean_reply_text(raw) == "bold"


def test_strips_tool_trace():
    raw = "前文\n🛠️ code_run({\"script\": \"ls\", \"type\": \"bash\"})\n后文"
    assert clean_reply_text(raw) == "前文\n后文"


def test_strips_error_fragment():
    raw = "重试前\n!!!Error: HTTP 503: {\"code\": \"UPSTREAM_ERROR\"}\n重试后"
    assert clean_reply_text(raw) == "重试前\n重试后"


def test_preserves_user_facing_content():
    raw = "\nTurn 1 ...\n\n<summary>快照</summary>\n\n这是最终回复：\n- 第一点\n- 第二点"
    cleaned = clean_reply_text(raw)
    assert cleaned == "这是最终回复：\n- 第一点\n- 第二点"


def test_empty_and_pure_tool_turn():
    assert clean_reply_text("") == ""
    assert clean_reply_text("\nTurn 2 ...\n\n<summary>x</summary>\n🛠️ ls()\n") == ""


def test_idempotent():
    raw = "\nTurn 1 ...\n\n<summary>快照</summary>\n\n回复内容"
    once = clean_reply_text(raw)
    assert clean_reply_text(once) == once
