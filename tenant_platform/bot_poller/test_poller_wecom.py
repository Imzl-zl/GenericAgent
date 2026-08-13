"""企业微信(wecom)渠道 adapter 单测: 入站帧 → webhook body 契约 + 出站帧构造。

真实渠道冒烟需用户提供企微智能机器人凭据(bot_id/secret); 此处用与
wecom_aibot_sdk 事件结构一致的轻量 dict 帧验证消息映射(conversation_id
取值、群/单聊区分、字段透传)与出站 send_message 帧格式。
"""

from __future__ import annotations

import asyncio
import threading
import uuid

import tenant_platform.bot_poller.poller_server as poller_server
from tenant_platform.bot_poller.poller_server import CHANNEL_WECOM


def _frame(msgtype, *, msgid="msg_1", chatid="chatid_group_1", userid="u_1", text="你好"):
    """构造与 wecom_aibot_sdk 消息回调一致的帧(见 message_handler.py)。"""
    body = {
        "msgtype": msgtype,
        "msgid": msgid,
        "chatid": chatid,
        "sendertime": 1700000000,
        "from": {"userid": userid},
    }
    if msgtype == "text":
        body["text"] = {"content": text}
    elif msgtype == "image":
        body["image"] = {"url": "https://example.com/img.jpg", "aeskey": "k"}
    elif msgtype == "file":
        body["file"] = {"url": "https://example.com/f.pdf", "file_name": "f.pdf", "aeskey": "k"}
    return {"cmd": "aibot_msg_callback", "headers": {"req_id": "req-1"}, "body": body}


def _capture(adapter):
    posted = []

    def post(body, max_attempts=None):
        posted.append(body)
        return True

    adapter.post_webhook = post  # type: ignore[method-assign]
    return posted


def _make_adapter():
    return poller_server.WeComAdapter(
        "wc-bot", {"app_id": "bot-1", "app_secret": "secret-1"},
        "http://platform/webhook",
    )


# ---------------------------------------------------------------------------
# 入站: chatid = 对话单元; 单聊 chatid == userid, 群聊 chatid 为群 ID。
# ---------------------------------------------------------------------------

def test_wecom_group_text_maps_chat_id_as_conversation():
    adapter = _make_adapter()
    posted = _capture(adapter)

    adapter._handle_wecom_message(_frame("text"))

    assert len(posted) == 1
    body = posted[0]
    assert body["channel_type"] == CHANNEL_WECOM
    assert body["bot_uuid"] == "wc-bot"
    assert body["channel_account_id"] == "u_1"
    assert body["conversation_id"] == "chatid_group_1"
    assert body["message_id"] == "msg_1"
    assert body["text"] == "你好"
    assert body["conversation_type"] == "group"  # chatid != userid


def test_wecom_private_chat_maps_private_type():
    adapter = _make_adapter()
    posted = _capture(adapter)

    # 单聊: chatid == 发送者 userid(SDK 文档: 单聊会话 ID = userid)。
    adapter._handle_wecom_message(_frame("text", chatid="u_1"))

    assert posted[-1]["conversation_id"] == "u_1"
    assert posted[-1]["conversation_type"] == "private"


def test_wecom_missing_msgid_dropped():
    adapter = _make_adapter()
    posted = _capture(adapter)

    adapter._handle_wecom_message(_frame("text", msgid=""))

    assert len(posted) == 0


def test_wecom_non_text_message_passthrough():
    adapter = _make_adapter()
    posted = _capture(adapter)

    # 非文本消息且未配置 media_root: 媒体无从下载, 丢弃不投递(审查 B1,
    # 不再回误导性的空文本 → 平台 "empty message ignored")。
    # 配置 media_root 且下载成功时见 test_poller_inbound_media.py。
    adapter._handle_wecom_message(_frame("image"))

    assert len(posted) == 0


# ---------------------------------------------------------------------------
# 出站: send_text / 流式均走 WSClient.send_message(chatid, body)。
# ---------------------------------------------------------------------------

class _FakeWeComClient:
    """WSClient 的测试替身: 捕获 send_message(chatid, body) 调用。"""

    def __init__(self):
        self.sent = []

    async def send_message(self, chatid, body):
        self.sent.append({"chatid": chatid, **body})


def _wire_sync(adapter, client):
    """把适配器的异步执行桥替换为同步直跑(胶水层不测, 业务帧被真实执行)。"""
    adapter._client = client
    adapter._run_coro = lambda coro: asyncio.new_event_loop().run_until_complete(coro)


def test_wecom_send_text_builds_markdown_frame():
    adapter = _make_adapter()
    client = _FakeWeComClient()
    _wire_sync(adapter, client)

    adapter.send_text("chatid_group_1", "回复内容")

    assert len(client.sent) == 1
    frame = client.sent[0]
    assert frame["chatid"] == "chatid_group_1"
    # 主动发送(SEND_MSG)只支持 markdown/template_card/media 类型(SDK
    # SendMsgBody); text 仅被动回复可用, 平台 delivery 是异步主动推送。
    assert frame["msgtype"] == "markdown"
    assert frame["markdown"] == {"content": "回复内容"}


def test_wecom_stream_open_append_commit_abort():
    adapter = _make_adapter()
    client = _FakeWeComClient()
    _wire_sync(adapter, client)

    stream_id = adapter.send_stream_open("chatid_group_1", "开始")
    assert stream_id
    adapter.send_stream_append(stream_id, "追加")
    adapter.send_stream_commit(stream_id, "")

    # open: finish=False 的首帧 + append 累积 + commit 终帧全量。
    assert len(client.sent) == 3
    open_frame, append_frame, commit_frame = client.sent
    assert open_frame["msgtype"] == "stream"
    assert open_frame["stream"]["finish"] is False
    assert open_frame["stream"]["content"] == "开始"
    assert append_frame["stream"]["content"] == "开始追加"
    assert append_frame["stream"]["id"] == stream_id
    assert commit_frame["stream"]["finish"] is True
    assert commit_frame["stream"]["content"] == "开始追加"


def test_wecom_stream_abort_marks_interrupted():
    adapter = _make_adapter()
    client = _FakeWeComClient()
    _wire_sync(adapter, client)

    stream_id = adapter.send_stream_open("u_1", "生成中")
    adapter.send_stream_abort(stream_id)

    assert len(client.sent) == 2
    abort_frame = client.sent[-1]
    assert abort_frame["stream"]["finish"] is True
    assert "中断" in abort_frame["stream"]["content"]


def test_wecom_stream_unknown_id_is_noop():
    adapter = _make_adapter()
    client = _FakeWeComClient()
    _wire_sync(adapter, client)

    adapter.send_stream_commit("no-such-stream", "x")
    adapter.send_stream_abort("no-such-stream")

    assert client.sent == []  # 未知流幂等, 不发帧


# ---------------------------------------------------------------------------
# 注册表: VALID_CHANNEL_TYPES 与 BotManager 工厂接线。
# ---------------------------------------------------------------------------

def test_wecom_registered_in_factory_and_valid_types():
    assert CHANNEL_WECOM in poller_server.VALID_CHANNEL_TYPES

    manager = poller_server.BotManager()
    adapter = manager._build_adapter(
        "wc-bot", CHANNEL_WECOM, {"app_id": "bot-1", "app_secret": "s"},
        base_url="http://poller", updates_buf="", webhook_url="http://platform/webhook",
    )
    assert isinstance(adapter, poller_server.WeComAdapter)
    assert adapter.channel_type == CHANNEL_WECOM


def test_wecom_missing_sender_not_private():
    """sender 缺失时不得判为私聊(空==空防御): 保守归群聊。"""
    adapter = _make_adapter()
    posted = _capture(adapter)

    adapter._handle_wecom_message(_frame("text", chatid="", userid=""))

    assert len(posted) == 1
    assert posted[-1]["conversation_type"] == "group"
