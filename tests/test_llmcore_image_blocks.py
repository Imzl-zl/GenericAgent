"""2026-08-14 生产实证回归: NativeToolClient 空白块过滤误杀 image_url 块。

根因: filtered_content 用 c.get("text","") 判断——image_url/image 块没有
text 字段, 被判为空白块整个丢弃, 多模态注入被静默吞掉(GA 日志显示
injected, 模型首轮却没有图)。修复: 显式保留 image_url/image 块。
"""

import sys

sys.path.insert(0, ".")
from llmcore import NativeToolClient  # noqa: E402


class _FakeBackend:
    """记录 backend.ask 收到的 merged content, 返回模拟响应。"""

    def __init__(self):
        self.received = None
        self.model = "fake"
        self.name = "fake"
        self.stream = False
        self.history = []

    def ask(self, merged):
        self.received = merged
        resp = type("R", (), {
            "raw": "",
            "tool_calls": [],
            "stop_reason": "end_turn",
        })()
        yield ""
        return resp


def _run_chat(blocks):
    backend = _FakeBackend()
    client = NativeToolClient(backend)
    list(client.chat([{"role": "user", "content": blocks}]))
    return backend.received


def test_image_url_block_preserved():
    """image_url 块必须保留(2026-08-14 修复: 原过滤把无 text 字段的块全丢)。"""
    blocks = [
        {"type": "text", "text": "这是啥"},
        {"type": "image_url", "image_url": {"url": "data:image/png;base64,AAAA"}},
    ]
    received = _run_chat(blocks)
    content = received["content"]
    types = [b.get("type") for b in content]
    assert "image_url" in types, f"image_url block dropped: {types}"
    assert "text" in types, f"text block dropped: {types}"


def test_whitespace_text_block_still_filtered():
    """纯空白 text 块仍被过滤(原过滤意图保留)。"""
    blocks = [
        {"type": "text", "text": "   "},
        {"type": "text", "text": "real"},
    ]
    received = _run_chat(blocks)
    content = received["content"]
    texts = [b.get("text") for b in content if b.get("type") == "text"]
    assert texts == ["real"], f"whitespace filtering broken: {texts}"
