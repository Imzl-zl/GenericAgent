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


# ---------------------------------------------------------------------------
# 2026-08-14 审查 I-4: image_url 块在 Anthropic 协议通道必须转 Claude
# image 块(此前双轨制语义分裂: native_oai 透传可用, native_claude 原样
# 发送必然上游 400/丢图); S-1: 协议通道(ToolClient)拍平内容时图片块
# 降级为占位, 不再把 base64 文本垃圾注入提示词/历史/日志。
# ---------------------------------------------------------------------------

def test_claude_image_block_conversion():
    from llmcore import _claude_image_block
    out = _claude_image_block({
        "type": "image_url",
        "image_url": {"url": "data:image/png;base64,QUJD"},
    })
    assert out == {
        "type": "image",
        "source": {"type": "base64", "media_type": "image/png", "data": "QUJD"},
    }


def test_claude_image_block_passthrough_non_image_and_external_url():
    from llmcore import _claude_image_block
    text = {"type": "text", "text": "hi"}
    assert _claude_image_block(text) is text  # 非 image_url 原样
    ext = {"type": "image_url", "image_url": {"url": "https://x.example/a.png"}}
    assert _claude_image_block(ext) is ext  # 外部 URL 不转换
    malformed = {"type": "image_url", "image_url": {"url": "data:"}}
    assert _claude_image_block(malformed)["type"] == "image_url"  # 解析失败透传


def test_fix_messages_converts_image_url_for_claude_channels():
    """ClaudeSession/NativeClaudeSession 的 raw_ask 都经 _fix_messages——
    转换后 user 消息的 image_url 块变为 Claude image 块。"""
    from llmcore import _fix_messages
    out = _fix_messages([{
        "role": "user",
        "content": [
            {"type": "text", "text": "看图"},
            {"type": "image_url", "image_url": {"url": "data:image/jpeg;base64,QUJD"}},
        ],
    }])
    assert out[0]["content"][0]["type"] == "text"
    image = out[0]["content"][1]
    assert image["type"] == "image"
    assert image["source"]["type"] == "base64"
    assert image["source"]["data"] == "QUJD"


def test_fix_messages_roundtrip_via_msgs_claude2oai():
    """OAI 通道: _fix_messages 转 image → _msgs_claude2oai 转回 image_url,
    往返一致(OAI 协议仍收到 image_url 块)。"""
    from llmcore import _fix_messages, _msgs_claude2oai
    fixed = _fix_messages([{
        "role": "user",
        "content": [
            {"type": "image_url", "image_url": {"url": "data:image/png;base64,QUJD"}},
        ],
    }])
    oai = _msgs_claude2oai(fixed)
    blocks = oai[0]["content"]
    assert blocks[0]["type"] == "image_url"
    assert blocks[0]["image_url"]["url"] == "data:image/png;base64,QUJD"


def test_cache_control_lands_on_last_text_block_not_image():
    """I-4 连带: 图片任务首轮最后块是 image, cache_control 必须落在
    前面的 text 块(Anthropic 不允许标记在 image 块上)。"""
    from llmcore import ClaudeSession
    sess = ClaudeSession.__new__(ClaudeSession)
    sess.context_win = 30000
    sess.cut_msg_interval = 5
    sess.trim_keep_rate = 0.6
    sess.trim_keep_prefix = 0
    msgs = sess.make_messages([{
        "role": "user",
        "content": [
            {"type": "text", "text": "看图"},
            {"type": "image", "source": {"type": "base64", "media_type": "image/png", "data": "QUJD"}},
        ],
    }])
    content = msgs[0]["content"]
    assert content[0].get("cache_control") == {"type": "ephemeral"}  # text 块
    assert "cache_control" not in content[1]  # image 块无标记


def test_protocol_prompt_flattens_image_blocks_to_placeholder():
    """S-1: ToolClient 协议拍平必须剔除 image 块——旧实现 str(list) 把
    base64 文本垃圾注入提示词(每轮重发最多 3.5MB), 模型却看不到图。"""
    from llmcore import ToolClient, _flatten_prompt_content
    flat = _flatten_prompt_content([
        {"type": "text", "text": "看图"},
        {"type": "image_url", "image_url": {"url": "data:image/png;base64," + "A" * 1000}},
    ])
    assert "A" * 1000 not in flat  # base64 不进提示词
    assert "image omitted" in flat  # 占位提示
    assert "看图" in flat  # 文本块原样保留
    # 非 list content(旧路径)保持 str 原样
    assert _flatten_prompt_content("plain text") == "plain text"


def test_protocol_prompt_build_uses_flattened_content():
    """S-1: _build_protocol_prompt 对含图片块的消息输出占位而非 base64。"""
    from llmcore import ToolClient
    backend = type("B", (), {"name": "t", "model": "m"})()
    client = ToolClient.__new__(ToolClient)
    client._prepare_tool_instruction = lambda tools: ""
    client.auto_save_tokens = True
    client.last_tools = ""
    client.total_cd_tokens = 0
    client.backend = backend
    prompt = client._build_protocol_prompt([
        {"role": "system", "content": "sys"},
        {"role": "user", "content": [
            {"type": "text", "text": "看图"},
            {"type": "image_url", "image_url": {"url": "data:image/png;base64," + "B" * 2000}},
        ]},
    ], None)
    assert "B" * 2000 not in prompt
    assert "image omitted" in prompt
    assert "看图" in prompt
