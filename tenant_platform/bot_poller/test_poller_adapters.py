"""新渠道 adapter 单测: 飞书/钉钉/QQ 入站消息 → webhook body 契约。

真实渠道冒烟需用户提供应用凭据(IM_CHANNEL_BINDING §9-6); 此处用与 SDK
事件结构一致的轻量 fake 对象验证消息映射(conversation_id 取值、群/单聊
区分、字段透传)。
"""

from __future__ import annotations

import json
import threading
import time

import tenant_platform.bot_poller.poller_server as poller_server
from tenant_platform.bot_poller.poller_server import (
    CHANNEL_DINGTALK,
    CHANNEL_FEISHU,
    CHANNEL_QQ,
    CHANNEL_WECHAT,
)


def _capture(adapter):
    posted = []

    def post(body, max_attempts=None):
        posted.append(body)
        return True

    adapter.post_webhook = post  # type: ignore[method-assign]
    return posted


# ---------------------------------------------------------------------------
# 飞书: chat_id = 对话单元(p2p/group 统一); 群消息只收 @(平台权限层决定)。
# ---------------------------------------------------------------------------

class _FeishuSender:
    def __init__(self, open_id):
        self.sender_id = type("SID", (), {"open_id": open_id})()


class _FeishuMessage:
    def __init__(self, message_id, chat_id, message_type, content, chat_type="group"):
        self.message_id = message_id
        self.chat_id = chat_id
        self.message_type = message_type
        self.content = content
        self.chat_type = chat_type  # 'p2p' | 'group'(lark 事件字段)


class _FeishuData:
    def __init__(self, message):
        self.event = type("EV", (), {"message": message, "sender": _FeishuSender("ou_member_1")})()


def test_feishu_text_message_maps_chat_id_as_conversation():
    adapter = poller_server.FeishuAdapter("fs-bot", {"app_id": "cli_a", "app_secret": "s"}, "http://platform/webhook")
    posted = _capture(adapter)

    adapter._handle_feishu_message(_FeishuData(_FeishuMessage(
        "om_1", "oc_group_xyz", "text", json.dumps({"text": "整理会议纪要"}))))

    assert len(posted) == 1
    body = posted[0]
    assert body["channel_type"] == CHANNEL_FEISHU
    assert body["bot_uuid"] == "fs-bot"
    assert body["channel_account_id"] == "ou_member_1"
    assert body["conversation_id"] == "oc_group_xyz"  # 群 chat_id
    assert body["message_id"] == "om_1"
    assert body["text"] == "整理会议纪要"
    assert body["conversation_type"] == "group"  # chat_type='group'


def test_feishu_p2p_and_non_text_messages():
    adapter = poller_server.FeishuAdapter("fs-bot", {"app_id": "a", "app_secret": "s"}, "http://platform/webhook")
    posted = _capture(adapter)

    # p2p 私聊: chat_id 同样存在(飞书 p2p 会话有 chat_id)。
    adapter._handle_feishu_message(_FeishuData(_FeishuMessage(
        "om_2", "oc_p2p_1", "text", json.dumps({"text": "你好"}), chat_type="p2p")))
    assert posted[-1]["conversation_id"] == "oc_p2p_1"
    assert posted[-1]["conversation_type"] == "private"  # chat_type='p2p'

    # chat_type 缺失回退 private(防御性, 不应发生)。
    adapter._handle_feishu_message(_FeishuData(_FeishuMessage(
        "om_4", "oc_x", "text", json.dumps({"text": "x"}), chat_type="")))
    assert posted[-1]["conversation_type"] == "private"

    # 非文本消息(image 等): 文本为空但消息仍透传(平台可看到 media 提示)。
    adapter._handle_feishu_message(_FeishuData(_FeishuMessage(
        "om_3", "oc_group_xyz", "image", '{"image_key":"img_1"}')))
    assert posted[-1]["text"] == ""
    assert posted[-1]["message_id"] == "om_3"

    # 无 message_id 的消息被丢弃。
    before = len(posted)
    adapter._handle_feishu_message(_FeishuData(_FeishuMessage("", "oc_x", "text", "{}")))
    assert len(posted) == before


# ---------------------------------------------------------------------------
# 钉钉: conversationId = 对话单元(群/单聊统一); 平台只推 @ 消息(硬规则)。
# ---------------------------------------------------------------------------

class _DingTalkText:
    def __init__(self, content):
        self.content = content


class _DingTalkMsg:
    def __init__(self, message_id, conversation_id, sender_staff_id, content, conversation_type="2"):
        self.message_id = message_id
        self.conversation_id = conversation_id
        self.sender_staff_id = sender_staff_id
        self.text = _DingTalkText(content)
        self.conversation_type = conversation_type


def test_dingtalk_group_message_maps_conversation_id():
    adapter = poller_server.DingTalkAdapter("dt-bot", {"app_id": "ding-app", "app_secret": "s"}, "http://platform/webhook")
    posted = _capture(adapter)

    adapter._handle_chatbot_message(_DingTalkMsg(
        "msg-g1", "cid_group_1", "staff_1", "帮我总结", conversation_type="2"))

    assert len(posted) == 1
    body = posted[0]
    assert body["channel_type"] == CHANNEL_DINGTALK
    assert body["channel_account_id"] == "staff_1"
    assert body["conversation_id"] == "cid_group_1"  # conversationId(群)
    assert body["text"] == "帮我总结"
    assert body["conversation_type"] == "group"  # conversation_type=='2'=群


def test_dingtalk_private_and_empty_messages():
    adapter = poller_server.DingTalkAdapter("dt-bot", {"app_id": "a", "app_secret": "s"}, "http://platform/webhook")
    posted = _capture(adapter)

    # 单聊: conversationId 同样存在。
    adapter._handle_chatbot_message(_DingTalkMsg(
        "msg-p1", "cid_private_1", "staff_2", "在吗", conversation_type="1"))
    assert posted[-1]["conversation_id"] == "cid_private_1"
    assert posted[-1]["conversation_type"] == "private"  # conversation_type!='2'=单聊

    # 空文本丢弃(不产生 webhook)。
    before = len(posted)
    adapter._handle_chatbot_message(_DingTalkMsg("msg-e1", "cid_x", "staff_3", "   "))
    assert len(posted) == before


# ---------------------------------------------------------------------------
# QQ: 群=group_openid, C2C=openid; 平台只推 @ 群消息(GROUP_AT_MESSAGE_CREATE)。
# ---------------------------------------------------------------------------

class _QQAuthor:
    def __init__(self, member_openid="", user_openid=""):
        self.member_openid = member_openid
        self.user_openid = user_openid


class _QQGroupMessage:
    def __init__(self, msg_id, group_openid, member_openid, content):
        self.id = msg_id
        self.group_openid = group_openid
        self.author = _QQAuthor(member_openid=member_openid)
        self.content = content


class _QQC2CMessage:
    def __init__(self, msg_id, user_openid, content):
        self.id = msg_id
        self.author = _QQAuthor(user_openid=user_openid)
        self.content = content


def test_qq_group_at_message_maps_group_openid():
    adapter = poller_server.QQAdapter("qq-bot", {"app_id": "qq-app", "app_secret": "s"}, "http://platform/webhook")
    posted = _capture(adapter)

    adapter._handle_message(_QQGroupMessage("msg-g1", "group_openid_9", "member_openid_1", "你好"), is_group=True)

    assert len(posted) == 1
    body = posted[0]
    assert body["channel_type"] == CHANNEL_QQ
    assert body["channel_account_id"] == "member_openid_1"
    assert body["conversation_id"] == "group_openid_9"  # 群桶(IM_CHANNEL_BINDING §10)
    assert body["text"] == "你好"
    assert body["conversation_type"] == "group"


def test_qq_c2c_message_maps_openid():
    adapter = poller_server.QQAdapter("qq-bot", {"app_id": "a", "app_secret": "s"}, "http://platform/webhook")
    posted = _capture(adapter)

    adapter._handle_message(_QQC2CMessage("msg-c1", "openid_7", "在吗"), is_group=False)

    assert len(posted) == 1
    body = posted[0]
    assert body["channel_account_id"] == "openid_7"
    assert body["conversation_id"] == "openid_7"  # C2C: openid 即对话单元
    assert body["text"] == "在吗"
    assert body["conversation_type"] == "private"


# ---------------------------------------------------------------------------
# BotManager 注册表: channel_type → adapter 工厂 + /send 路由 + 幂等 start。
# ---------------------------------------------------------------------------

def test_manager_registry_factories_and_send_routing(monkeypatch):
    manager = poller_server.BotManager(media_root=None)
    # 用桩替换各 adapter 的连接循环: 注册表测试不建立真实 SDK 连接。
    started = []

    class StubWeChat(poller_server.WeChatAdapter):
        def _run(self):
            started.append((self.bot_uuid, "wechat"))

    class StubFeishu(poller_server.FeishuAdapter):
        def _run(self):
            started.append((self.bot_uuid, "feishu"))

    class StubDingTalk(poller_server.DingTalkAdapter):
        def _run(self):
            started.append((self.bot_uuid, "dingtalk"))

    class StubQQ(poller_server.QQAdapter):
        def _run(self):
            started.append((self.bot_uuid, "qq"))

    monkeypatch.setattr(poller_server, "WeChatAdapter", StubWeChat)
    monkeypatch.setattr(poller_server, "FeishuAdapter", StubFeishu)
    monkeypatch.setattr(poller_server, "DingTalkAdapter", StubDingTalk)
    monkeypatch.setattr(poller_server, "QQAdapter", StubQQ)

    manager.start("b-wx", CHANNEL_WECHAT, '{"token":"t"}', webhook_url="http://p/webhook")
    manager.start("b-fs", CHANNEL_FEISHU, '{"app_id":"a","app_secret":"s"}', webhook_url="http://p/webhook")
    manager.start("b-dt", CHANNEL_DINGTALK, '{"app_id":"a","app_secret":"s"}', webhook_url="http://p/webhook")
    manager.start("b-qq", CHANNEL_QQ, '{"app_id":"a","app_secret":"s"}', webhook_url="http://p/webhook")
    # 幂等: 重复 start 同 bot_uuid 不重启。
    manager.start("b-wx", CHANNEL_WECHAT, '{"token":"t"}', webhook_url="http://p/webhook")

    health = manager.health()
    assert sorted(health["active_bots"]) == ["b-dt", "b-fs", "b-qq", "b-wx"]
    # 桩 _run 在独立线程执行: 短等待后断言启动记录。
    deadline = time.monotonic() + 3
    while len(started) < 4 and time.monotonic() < deadline:
        time.sleep(0.02)
    assert sorted(started) == [("b-dt", "dingtalk"), ("b-fs", "feishu"), ("b-qq", "qq"), ("b-wx", "wechat")]

    # 未知渠道拒绝。
    try:
        manager.start("b-x", "telegram", "{}", webhook_url="http://p/webhook")
        raise AssertionError("telegram must be rejected")
    except ValueError:
        pass

    # send 路由到对应 adapter(wechat 走 WxBotClient——替换为 fake 验证)。
    wx = manager._adapters["b-wx"]

    class FakeClient:
        def __init__(self):
            self.calls = []

        def send_text(self, target, text, context_token="", client_id=None):
            self.calls.append(("text", target, text, client_id))

        def send_file(self, target, file_path, context_token="", file_name="", client_id=None):
            self.calls.append(("file", target, file_path, file_name, client_id))

    wx.client = FakeClient()
    manager.send("b-wx", "u1", "hi", client_id="cid-1")
    assert wx.client.calls == [("text", "u1", "hi", "cid-1")]

    # 未知 bot → KeyError。
    try:
        manager.send("b-missing", "u1", "hi")
        raise AssertionError("unknown bot must raise")
    except KeyError:
        pass

    # 非微信渠道 send_file 未实现 → 明确报错(不静默)。
    try:
        manager.send("b-fs", "oc_1", "", msg_type="file", file_path="/tmp/x")
        raise AssertionError("feishu send_file must raise")
    except NotImplementedError:
        pass


# ---------------------------------------------------------------------------
# IM 流式输出(IM_STREAMING_DELIVERY §4): /send stream_action 路由 +
# FeishuAdapter 消息编辑打字机(fake lark SDK)。
# ---------------------------------------------------------------------------

class _FakeLarkV1:
    """fake lark_oapi.api.im.v1: builder 链记录调用, create/update 返回可控响应。

    CreateMessageRequest 等是类对象语义(真实 lark 中 .builder() 是类方法),
    因此以实例属性存放 _ReqFactory(builder() 起链), 不是实例方法。"""

    def __init__(self):
        self.created = []      # (receive_id, content)
        self.updated = []      # (message_id, content)
        self.create_resp = None  # 注入 create 响应(默认成功 message_id='m-stream-1')
        self.update_fail = False
        self.create_fail = False
        self.CreateMessageRequest = self._ReqFactory(self, "create")
        self.CreateMessageRequestBody = self._ReqFactory(self, "create_body")
        self.UpdateMessageRequest = self._ReqFactory(self, "update")
        self.UpdateMessageRequestBody = self._ReqFactory(self, "update_body")

    class _ReqBuilder:
        def __init__(self, v1, kind):
            self.v1 = v1
            self.kind = kind
            self.kwargs = {}

        def __getattr__(self, name):
            def setter(value):
                self.kwargs[name] = value
                return self
            return setter

        def build(self):
            return self

    class _ReqFactory:
        """模拟 lark 的 Request 类: 调用方用 .builder() 起链。"""

        def __init__(self, v1, kind):
            self._v1 = v1
            self._kind = kind

        def builder(self):
            return self._v1._req(self._kind)

    def _req(self, kind):
        return self._ReqBuilder(self, kind)


class _FakeLarkAPI:
    def __init__(self, v1):
        self._v1 = v1
        self.message = self

    @property
    def v1(self):
        return self._v1

    def create(self, request):
        v1 = self._v1
        body = request.kwargs.get('request_body', request)
        content = body.kwargs.get('content', '{}')
        # receive_id 在 CreateMessageRequestBody 上(真实 lark 同构)。
        v1.created.append((body.kwargs.get('receive_id'), content))
        if v1.create_fail:
            return type("Resp", (), {"success": lambda self=False: False, "code": 99991, "msg": "create boom"})()
        data = type("D", (), {"message_id": "m-stream-1"})()
        return type("Resp", (), {"success": lambda self=False: True, "data": data})()

    def update(self, request):
        v1 = self._v1
        body = request.kwargs.get('request_body', request)
        content = body.kwargs.get('content', '{}')
        v1.updated.append((request.kwargs.get('message_id'), content))
        if v1.update_fail:
            return type("Resp", (), {"success": lambda self=False: False, "code": 99992, "msg": "update boom"})()
        return type("Resp", (), {"success": lambda self=False: True})()


def _feishu_adapter_with_fake_lark():
    v1 = _FakeLarkV1()
    lark = type("Lark", (), {
        "Client": type("Client", (), {
            "builder": classmethod(lambda cls: type("B", (), {
                "app_id": lambda self, v: self,
                "app_secret": lambda self, v: self,
                "build": lambda self: type("C", (), {"im": type("IM", (), {"v1": None})()}),
            })()),
        }),
        "api": type("API", (), {"im": type("IM", (), {"v1": v1})()}),
    })()
    adapter = poller_server.FeishuAdapter(
        "fs-stream", {"app_id": "cli_a", "app_secret": "s"}, "http://p/webhook")
    adapter._lark = lark
    # api_client.im.v1.message.create/update → _FakeLarkAPI(v1 引用)。
    api_v1 = _FakeLarkAPI(v1)
    adapter._api_client = type("C", (), {"im": type("IM", (), {"v1": api_v1})()})()
    return adapter, v1


def test_feishu_stream_typing_lifecycle():
    adapter, v1 = _feishu_adapter_with_fake_lark()

    stream_id = adapter.send_stream_open("oc_conv_1", "占位")
    assert stream_id  # 非空句柄
    assert v1.created[0][0] == "oc_conv_1"
    assert json.loads(v1.created[0][1])["text"] == "占位"

    # append 累积后全量 PUT 更新(飞书编辑=整条替换)。
    adapter.send_stream_append(stream_id, "思考")
    adapter.send_stream_append(stream_id, "中")
    assert len(v1.updated) == 2
    assert json.loads(v1.updated[0][1])["text"] == "占位思考"
    assert json.loads(v1.updated[1][1])["text"] == "占位思考中"

    # commit 最后一次更新(最终累积文本)。
    adapter.send_stream_commit(stream_id)
    assert json.loads(v1.updated[-1][1])["text"] == "占位思考中"
    assert len(v1.updated) == 3

    # 未知 stream_id: append 报错, commit/abort 幂等。
    try:
        adapter.send_stream_append("no-such", "x")
        raise AssertionError("unknown stream must raise")
    except KeyError:
        pass
    adapter.send_stream_commit("no-such")  # 幂等不报错
    adapter.send_stream_abort("no-such")   # 幂等不报错


def test_feishu_stream_abort_rewrites_placeholder():
    adapter, v1 = _feishu_adapter_with_fake_lark()
    stream_id = adapter.send_stream_open("oc_conv_1", "…")
    adapter.send_stream_abort(stream_id)
    assert len(v1.updated) == 1
    assert "生成中断" in json.loads(v1.updated[0][1])["text"]


def test_token_bucket_acquire_timeout():
    # 无补充(refill=0) + 容量 1: 首次通过, 后续 acquire 阻塞至超时返回 False。
    bucket = poller_server._TokenBucket(capacity=1, refill_per_sec=0)
    assert bucket.acquire() is True
    import time as _t
    t0 = _t.monotonic()
    assert bucket.acquire(timeout=0.3) is False
    assert _t.monotonic() - t0 >= 0.2  # 确实等待了令牌


def test_feishu_stream_edit_cap_protects_20_edits():
    # 官方限制: 一条消息最多编辑 20 次(append 每次 PUT + commit 一次 PUT)。
    # 用实例覆盖模拟 cap(默认 _MAX_STREAM_EDITS=18): 超限后不再 PUT,
    # 文本累积到 commit 最后一次更新一次性送达。
    adapter, v1 = _feishu_adapter_with_fake_lark()
    adapter._MAX_STREAM_EDITS = 2  # 模拟 cap(默认 18)
    stream_id = adapter.send_stream_open("oc_conv_1", "")

    for _ in range(4):
        adapter.send_stream_append(stream_id, "a")
    adapter.send_stream_commit(stream_id)

    # open(create, 非编辑) + append×2(PUT) + commit×1(PUT) = 3 次 PUT(≤ cap+1)。
    # 4 次 append 中后 2 次被 cap 拦截(累积), commit 全量送达。
    assert len(v1.updated) == 3, len(v1.updated)
    assert json.loads(v1.updated[-1][1])["text"] == "aaaa"  # 累积全量(内容不丢)


def test_feishu_stream_rate_limited_append_raises():
    adapter, v1 = _feishu_adapter_with_fake_lark()
    # 令牌桶容量 1 且无补充: open 不消耗令牌, 第一个 append 通过,
    # 第二个 append acquire 超时(2s) → RuntimeError(Go 侧 abort → delivery 兜底)。
    adapter._throttle = poller_server._TokenBucket(capacity=1, refill_per_sec=0)
    stream_id = adapter.send_stream_open("oc_conv_1", "")
    adapter.send_stream_append(stream_id, "a")
    try:
        adapter.send_stream_append(stream_id, "b")
        raise AssertionError("rate limited append must raise")
    except RuntimeError:
        pass


def test_feishu_stream_open_failure_raises():
    adapter, v1 = _feishu_adapter_with_fake_lark()
    v1.create_fail = True
    try:
        adapter.send_stream_open("oc_conv_1", "")
        raise AssertionError("create failure must raise")
    except RuntimeError:
        pass


def test_manager_send_stream_routes_and_rejects_non_stream():
    manager = poller_server.BotManager(media_root=None)
    # 用桩替换 FeishuAdapter 连接循环与 SDK, 验证 manager 路由。
    started = []

    class StubFeishu(poller_server.FeishuAdapter):
        def _run(self):
            started.append(self.bot_uuid)

    class StubDingTalk(poller_server.DingTalkAdapter):
        def _run(self):
            started.append(self.bot_uuid)

    import types
    monkey = None
    # 直接构造 stub 实例注册(不走 start 线程, 避免 SDK 依赖)。
    fs = StubFeishu("b-fs", {"app_id": "a", "app_secret": "s"}, "http://p/webhook")
    fs._lark = types.SimpleNamespace()  # 不触发真实 SDK
    dt = StubDingTalk("b-dt", {"app_id": "a", "app_secret": "s"}, "http://p/webhook")
    manager._adapters["b-fs"] = fs
    manager._adapters["b-dt"] = dt

    # open 返回 stream_id(FeishuAdapter 真实实现需要 fake lark)。
    fs._lark = _feishu_adapter_with_fake_lark()[0]._lark
    fs._api_client = _feishu_adapter_with_fake_lark()[0]._api_client
    sid = manager.send_stream("b-fs", "", "open", target="oc_1", text="")
    assert sid

    # append/commit/abort 按 stream_id 路由。
    manager.send_stream("b-fs", sid, "append", text="hi")
    manager.send_stream("b-fs", sid, "commit")
    manager.send_stream("b-fs", sid, "abort")  # 已 commit 幂等

    # 未知 action 拒绝。
    try:
        manager.send_stream("b-fs", sid, "nope")
        raise AssertionError("unknown action must raise")
    except ValueError:
        pass

    # 非流渠道(钉钉)明确拒绝。
    try:
        manager.send_stream("b-dt", "", "open", target="cid_1")
        raise AssertionError("dingtalk stream open must raise")
    except NotImplementedError:
        pass


# ---------------------------------------------------------------------------
# QQAdapter 原生流式(单聊): botpy 内部 _http.request(Route) 帧序列断言。
# ---------------------------------------------------------------------------

class _FakeQQHTTP:
    """fake botpy _http: 记录 Route + json payload, 可注入响应序列。"""

    def __init__(self, responses=None):
        self.calls = []  # (route_path, payload)
        self.responses = responses or []

    async def request(self, route, json=None):
        # 存副本: 重试会原地修改 payload dict, 引用会记录最终状态。
        self.calls.append((route.path if hasattr(route, 'path') else str(route), dict(json or {})))
        if self.responses:
            return self.responses.pop(0)
        return {'id': 'stream-qq-1'}


class _FakeQQClient:
    def __init__(self, http):
        self.loop = None  # 由测试注入 event loop
        self.api = type("API", (), {"_http": http})()


class _LoopRunner:
    """后台线程跑 asyncio loop(run_coroutine_threadsafe 需要 loop 运行中,
    模拟 botpy 自身的事件循环线程)。"""

    def __init__(self):
        import asyncio as _aio
        self.loop = _aio.new_event_loop()
        self._thread = threading.Thread(target=self.loop.run_forever, daemon=True)
        self._thread.start()

    def stop(self):
        self.loop.call_soon_threadsafe(self.loop.stop)
        self._thread.join(timeout=5)
        self.loop.close()


def _qq_adapter_with_fake_client(http):
    adapter = poller_server.QQAdapter(
        "qq-stream", {"app_id": "qq-app", "app_secret": "s"}, "http://p/webhook")
    adapter._client = _FakeQQClient(http)
    return adapter


def test_qq_stream_c2c_frame_sequence():
    runner = _LoopRunner()
    try:
        http = _FakeQQHTTP()
        adapter = _qq_adapter_with_fake_client(http)
        adapter._client.loop = runner.loop

        _capture(adapter)
        adapter._handle_message(_QQC2CMessage("msg-c1", "openid_7", "在吗"), is_group=False)
        stream_id = adapter.send_stream_open("openid_7", "占位")
        assert stream_id == "stream-qq-1"

        adapter.send_stream_append(stream_id, "思考")
        adapter.send_stream_append(stream_id, "中")
        adapter.send_stream_commit(stream_id)

        paths = [c[0] for c in http.calls]
        # 官方独立端点(非 /messages); Route.path 是模板。
        assert all(p == "/v2/users/{openid}/stream_messages" for p in paths), paths
        payloads = [c[1] for c in http.calls]
        assert len(payloads) == 4

        # 首帧: input_state=1(生成中), 无 stream_msg_id, msg_id 锚点, index=0。
        p0 = payloads[0]
        assert p0["input_mode"] == "replace" and p0["input_state"] == 1
        assert p0["content_type"] == "markdown" and p0["content_raw"] == "占位"
        assert p0["msg_id"] == "msg-c1" and p0["event_id"] == "msg-c1"
        assert "stream_msg_id" not in p0 and p0["index"] == 0
        msg_seq = p0["msg_seq"]
        assert msg_seq > 0

        # 追加帧: 全量替换语义(content_raw=累积), index 递增, 同一 msg_seq。
        p1, p2 = payloads[1], payloads[2]
        assert p1["input_state"] == 1 and p1["index"] == 1
        assert p1["stream_msg_id"] == "stream-qq-1"
        assert p1["content_raw"] == "占位思考"
        assert p2["input_state"] == 1 and p2["index"] == 2
        assert p2["content_raw"] == "占位思考中"
        assert p1["msg_seq"] == msg_seq and p2["msg_seq"] == msg_seq  # 共享

        # 终帧: input_state=10(DONE), 全量最终文本, index 递增。
        p3 = payloads[3]
        assert p3["input_state"] == 10 and p3["index"] == 3
        assert p3["content_raw"] == "占位思考中"
        assert p3["msg_seq"] == msg_seq and p3["stream_msg_id"] == "stream-qq-1"
    finally:
        runner.stop()


def test_qq_stream_uses_last_c2c_msg_id_for_passive_reply():
    runner = _LoopRunner()
    try:
        http = _FakeQQHTTP()
        adapter = _qq_adapter_with_fake_client(http)
        adapter._client.loop = runner.loop
        _capture(adapter)  # 入站 webhook 不真实发送

        # 入站 C2C 消息记录 msg_id 锚点。
        adapter._handle_message(_QQC2CMessage("msg-c1", "openid_7", "在吗"), is_group=False)
        stream_id = adapter.send_stream_open("openid_7", "")
        assert http.calls[0][1].get("msg_id") == "msg-c1"
        assert http.calls[0][1].get("event_id") == "msg-c1"
    finally:
        runner.stop()


def test_qq_stream_append_cap_protects_ratelimit():
    # 业务上限(cap 生效后不再发追加帧, 文本累积到 commit 终帧一次送达)。
    # 默认 _MAX_STREAM_APPENDS=60(官方 SDK 无帧数限制, 仅 500ms 节流);
    # 用实例覆盖模拟 cap 命中。
    runner = _LoopRunner()
    try:
        http = _FakeQQHTTP()
        adapter = _qq_adapter_with_fake_client(http)
        adapter._client.loop = runner.loop
        adapter._MAX_STREAM_APPENDS = 2  # 模拟 cap(默认 60)
        _capture(adapter)
        adapter._handle_message(_QQC2CMessage("msg-cap", "openid_7", "在吗"), is_group=False)

        stream_id = adapter.send_stream_open("openid_7", "")
        for _ in range(5):
            adapter.send_stream_append(stream_id, "x")
        adapter.send_stream_commit(stream_id)

        frames = [c[1] for c in http.calls]
        # open + 2 append + commit = 4 帧。
        assert len(frames) == 4, len(frames)
        assert frames[-1]["input_state"] == 10 and frames[-1]["index"] == 3
        assert frames[-1]["content_raw"] == "xxxxx"  # 累积全量(内容不丢)
    finally:
        runner.stop()


def test_qq_stream_abort_sends_terminal_frame():
    runner = _LoopRunner()
    try:
        http = _FakeQQHTTP()
        adapter = _qq_adapter_with_fake_client(http)
        adapter._client.loop = runner.loop

        _capture(adapter)
        adapter._handle_message(_QQC2CMessage("msg-ab", "openid_7", "在吗"), is_group=False)
        stream_id = adapter.send_stream_open("openid_7", "开头")
        adapter.send_stream_abort(stream_id)

        frames = [c[1] for c in http.calls]
        assert len(frames) == 2
        assert frames[-1]["input_state"] == 10  # DONE 终帧(保证消息闭合)
        assert "生成中断" in frames[-1]["content_raw"]
        # 幂等: 再次 abort 无操作。
        adapter.send_stream_abort(stream_id)
        assert len(http.calls) == 2
    finally:
        runner.stop()


def test_qq_stream_retries_on_rate_limit():
    # 官方 SDK sendWithRetry 同构: 429/50002 指数退避重试, 重试帧 index 递增。
    runner = _LoopRunner()
    try:
        responses = [
            {'code': 429, 'message': 'rate limit'},
            {'err_code': 50002, 'message': 'rate limited'},
            {'id': 'stream-qq-1'},
            {'id': 'stream-qq-1'},
        ]
        http = _FakeQQHTTP(responses=responses)
        adapter = _qq_adapter_with_fake_client(http)
        adapter._client.loop = runner.loop
        _capture(adapter)
        adapter._handle_message(_QQC2CMessage("msg-rl", "openid_7", "在吗"), is_group=False)

        stream_id = adapter.send_stream_open("openid_7", "")
        assert stream_id == "stream-qq-1"
        # open 前两次被限流 → 重试(2 次退避), 第三次成功; index 递增避免 stale。
        payloads = [c[1] for c in http.calls]
        assert len(payloads) == 3, len(payloads)
        assert payloads[0]["index"] == 0 and payloads[1]["index"] == 1 and payloads[2]["index"] == 2
        # 重试后 msg_seq 不变(同一流共享)。
        assert payloads[0]["msg_seq"] == payloads[2]["msg_seq"]
    finally:
        runner.stop()


def test_qq_stream_open_failure_raises():
    runner = _LoopRunner()
    try:
        http = _FakeQQHTTP()
        adapter = _qq_adapter_with_fake_client(http)
        adapter._client.loop = runner.loop
        # 无入站 C2C msg_id 锚点 → 拒绝(官方 SDK 要求流式必须被动回复)。
        try:
            adapter.send_stream_open("openid_7", "")
            raise AssertionError("open without msg_id anchor must raise")
        except RuntimeError:
            pass
        # 空响应 → 无 stream id → 拒绝。
        http2 = _FakeQQHTTP(responses=[{}])
        adapter2 = _qq_adapter_with_fake_client(http2)
        adapter2._client.loop = runner.loop
        _capture(adapter2)
        adapter2._handle_message(_QQC2CMessage("msg-e", "openid_7", "在吗"), is_group=False)
        try:
            adapter2.send_stream_open("openid_7", "")
            raise AssertionError("empty stream id must raise")
        except RuntimeError:
            pass
    finally:
        runner.stop()
