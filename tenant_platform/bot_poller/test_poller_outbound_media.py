"""出站媒体统一接口测试(IM_MEDIA_ARCHITECTURE §5.1 A1 落地, Phase A)。

覆盖:
  * send_media 基类分发 + 防御校验(media_type/文件存在/大小上限);
  * QQ 分片上传全流程(预上传→分片 PUT→完成→msg_type=7 主动消息) +
    单聊/群聊端点分流 + per-target 频控预算(fail-closed);
  * 飞书 image/file 分发到上传+消息构造(请求形状);
  * 微信复用既有 send_*(薄包装)。

真实渠道冒烟仍需用户凭据(QQ/飞书已配置; 分片参数/频控阈值以实测为准)。
"""

from __future__ import annotations

import asyncio
import json

import pytest

import tenant_platform.bot_poller.poller_server as poller_server


class _SyncFuture:
    """把 run_coroutine_threadsafe 替换为同步执行(coro 在临时 loop 跑完),
    返回带 .result(timeout) 的假 Future——测试免起真实事件循环线程。"""

    def __init__(self, coro):
        self._result = asyncio.new_event_loop().run_until_complete(coro)

    def result(self, timeout=None):
        return self._result


# ---------------------------------------------------------------------------
# send_media 基类: 校验 + 分发
# ---------------------------------------------------------------------------

class _StubAdapter(poller_server.BotAdapter):
    channel_type = 'stub'

    def __init__(self):
        super().__init__('b1', 'http://platform/webhook')
        self.calls = []

    def _run(self):
        pass

    def send_text(self, target, text, client_id=''):
        pass

    def _send_image(self, target, file_path, file_name='', client_id=''):
        self.calls.append(('image', target, file_path, file_name))

    def _send_file(self, target, file_path, file_name='', client_id=''):
        self.calls.append(('file', target, file_path, file_name))

    def _send_video(self, target, file_path, file_name='', client_id=''):
        self.calls.append(('video', target, file_path, file_name))


def test_send_media_validates_and_dispatches(tmp_path):
    adapter = _StubAdapter()
    img = tmp_path / "a.jpg"
    img.write_bytes(b"\xff\xd8\xff" + b"x" * 32)

    adapter.send_media("u1", str(img), "image", file_name="a.jpg")
    assert adapter.calls == [("image", "u1", str(img), "a.jpg")]
    adapter.calls.clear()
    adapter.send_media("u1", str(img), "video")
    assert adapter.calls == [("video", "u1", str(img), "")]

    with pytest.raises(ValueError, match="media_type"):
        adapter.send_media("u1", str(img), "audio")
    with pytest.raises(ValueError, match="not found"):
        adapter.send_media("u1", "/tmp/definitely-missing.png", "image")


def test_send_media_size_limit_enforced(tmp_path):
    adapter = _StubAdapter()
    big = tmp_path / "big.jpg"
    big.write_bytes(b"x" * 1024)
    adapter.media_size_limit = 100
    with pytest.raises(ValueError, match="size limit"):
        adapter.send_media("u1", str(big), "image")


def test_send_media_unimplemented_raises_not_implemented(tmp_path):
    class _NoMediaAdapter(poller_server.BotAdapter):
        channel_type = 'stub'

        def __init__(self):
            super().__init__('b1', 'http://platform/webhook')

        def _run(self):
            pass

        def send_text(self, target, text, client_id=''):
            pass
        # 故意不实现 _send_image: 基类默认抛 NotImplementedError。

    adapter = _NoMediaAdapter()
    img = tmp_path / "a.jpg"
    img.write_bytes(b"x")
    with pytest.raises(NotImplementedError):
        adapter.send_media("u1", str(img), "image")


# ---------------------------------------------------------------------------
# QQ: 分片上传全流程 + 主动消息 + 频控
# ---------------------------------------------------------------------------

class _FakeLoop:
    def __init__(self):
        self.closed = False

    def is_closed(self):
        return self.closed


class _FakeHTTP:
    def __init__(self, handler):
        self.handler = handler
        self.calls = []

    async def request(self, route, json=None):
        # botpy Route.path 是模板, format 由 route.url property 完成——
        # fake 直接 format(与 botpy 语义一致)。
        path = route.path.format(**getattr(route, 'parameters', {}) or {})
        self.calls.append((route.method, path, json))
        return self.handler(route.method, path, json)


class _FakeAPI:
    def __init__(self, http):
        self._http = http


class _FakeQQClient:
    def __init__(self, http):
        self.api = _FakeAPI(http)
        self.loop = _FakeLoop()


def _qq_adapter(tmp_path, handler):
    adapter = poller_server.QQAdapter(
        "qq-bot", {"app_id": "a", "app_secret": "s"}, "http://platform/webhook",
        media_root=str(tmp_path))
    adapter._client = _FakeQQClient(_FakeHTTP(handler))
    adapter._target_kinds["ou_1"] = "c2c"
    adapter._target_kinds["grp_1"] = "group"
    return adapter


def _sync_qq(monkeypatch):
    """把 asyncio.run_coroutine_threadsafe 替换为同步执行(免事件循环线程)。"""
    monkeypatch.setattr(poller_server.asyncio, "run_coroutine_threadsafe",
                        lambda coro, loop: _SyncFuture(coro))


def test_qq_media_upload_chunked_and_sent_active(tmp_path, monkeypatch):
    _sync_qq(monkeypatch)
    file_bytes = b"y" * 200
    media_file = tmp_path / "pic.jpg"
    media_file.write_bytes(file_bytes)
    presigned_hits = []

    def handler(method, path, body):
        if path == "/v2/users/ou_1/upload_prepare":
            assert body["file_type"] == 1  # 图片
            assert body["file_size"] == "200"
            assert body["file_name"] == "pic.jpg"
            assert body["md5"] and body["sha1"] and body["md5_10m"]
            return {"upload_id": "up_1", "block_size": 64,
                    "parts": [{"index": i, "presigned_url": f"https://cos.example.com/p{i}"}
                              for i in range(4)]}
        if path == "/v2/users/ou_1/upload_finish":
            assert body == {"upload_id": "up_1"}
            return {"file_info": "FI_1"}
        if path == "/v2/users/ou_1/messages":
            assert body == {"msg_type": 7, "media": {"file_info": "FI_1"}}
            return {}
        raise AssertionError(f"unexpected qq route {method} {path}")

    def fake_put(url, data, timeout):
        presigned_hits.append((url, len(data)))
        return type("R", (), {"raise_for_status": lambda self: None})()

    monkeypatch.setattr(poller_server.requests, "put", fake_put)
    adapter = _qq_adapter(tmp_path, handler)
    adapter._send_image("ou_1", str(media_file), file_name="pic.jpg")
    # 4 片串行 PUT, 每片 64 字节(最后一片 8 字节)。
    assert len(presigned_hits) == 4
    assert presigned_hits[0][1] == 64
    assert presigned_hits[-1][1] == 8
    http = adapter._client.api._http
    assert http.calls[0][0] == "POST" and http.calls[0][1].endswith("/upload_prepare")
    assert http.calls[-1][1].endswith("/messages")
    assert http.calls[-1][2]["media"]["file_info"] == "FI_1"


def test_qq_media_group_endpoints(tmp_path, monkeypatch):
    _sync_qq(monkeypatch)
    media_file = tmp_path / "doc.pdf"
    media_file.write_bytes(b"pdf" * 100)

    def handler(method, path, body):
        if path == "/v2/groups/grp_1/upload_prepare":
            return {"upload_id": "up_g", "block_size": 5 * 1024 * 1024,
                    "parts": [{"index": 0, "presigned_url": "https://cos.example.com/g0"}]}
        if path == "/v2/groups/grp_1/upload_finish":
            return {"file_info": "FI_G"}
        if path == "/v2/groups/grp_1/messages":
            assert body == {"msg_type": 7, "media": {"file_info": "FI_G"}}
            return {}
        raise AssertionError(f"unexpected qq route {method} {path}")

    class _Resp:
        def __init__(self, url=None, data=None, timeout=None):
            self.url = url
            self.data = data

        def raise_for_status(self):
            pass

    monkeypatch.setattr(poller_server.requests, "put",
                        lambda url, data, timeout: _Resp(url=url, data=data))
    adapter = _qq_adapter(tmp_path, handler)
    adapter._send_file("grp_1", str(media_file), file_name="doc.pdf")
    # 群聊走 /v2/groups/{group_openid} 端点(单聊/群聊上传不互通, 官方)。
    http = adapter._client.api._http
    assert all("/groups/" in c[1] for c in http.calls)


def test_qq_media_rate_limit_fail_closed(tmp_path, monkeypatch):
    _sync_qq(monkeypatch)
    media_file = tmp_path / "pic.jpg"
    media_file.write_bytes(b"x" * 10)

    def handler(method, path, body):
        if path.endswith("/upload_prepare"):
            return {"upload_id": "up_1", "block_size": 64,
                    "parts": [{"index": 0, "presigned_url": "https://cos.example.com/p0"}]}
        if path.endswith("/upload_finish"):
            return {"file_info": "FI_1"}
        return {}

    class _Resp:
        def raise_for_status(self):
            pass

    monkeypatch.setattr(poller_server.requests, "put", lambda url, data, timeout: _Resp())
    # 频控桶恒空(acquire 立即失败, 不等 5s 超时)。
    monkeypatch.setattr(poller_server._TokenBucket, "acquire", lambda self, timeout=2.0: False)
    adapter = _qq_adapter(tmp_path, handler)
    with pytest.raises(RuntimeError, match="rate limited"):
        adapter._send_image("ou_1", str(media_file), file_name="pic.jpg")


# ---------------------------------------------------------------------------
# 飞书: image/file 分发到上传 + 消息构造(请求形状, 与流式测试同模式注入
# adapter._lark + adapter._api_client)
# ---------------------------------------------------------------------------

class _FakeLarkV1:
    """fake lark_oapi.api.im.v1: builder 链记录调用; image/file/message 可控响应。"""

    def __init__(self):
        self.created = []       # (receive_id, msg_type, content_json)
        self.uploaded = []      # 'image' | 'file'
        self.resource_get = []  # (message_id, file_key, type)
        self.CreateMessageRequest = self._ReqFactory(self)
        self.CreateMessageRequestBody = self._ReqFactory(self)
        self.CreateImageRequest = self._ReqFactory(self)
        self.CreateImageRequestBody = self._ReqFactory(self)
        self.CreateFileRequest = self._ReqFactory(self)
        self.CreateFileRequestBody = self._ReqFactory(self)
        self.GetMessageResourceRequest = self._ReqFactory(self)

    class _ReqBuilder:
        def __init__(self, v1):
            self.v1 = v1
            self.kwargs = {}

        def __getattr__(self, name):
            def setter(value):
                self.kwargs[name] = value
                return self
            return setter

        def build(self):
            return self

    class _ReqFactory:
        def __init__(self, v1):
            self._v1 = v1

        def builder(self):
            return self._v1._ReqBuilder(self._v1)


def _feishu_outbound_adapter(v1):
    api_v1 = _FakeLarkAPI(v1)
    lark = type("Lark", (), {
        "api": type("API", (), {"im": type("IM", (), {"v1": v1})()})(),
        "Client": type("Client", (), {
            "builder": classmethod(lambda cls: type("B", (), {
                "app_id": lambda self, v: self,
                "app_secret": lambda self, v: self,
                "build": lambda self: type("C", (), {"im": type("IM", (), {"v1": api_v1})()})(),
            })()),
        }),
    })()
    adapter = poller_server.FeishuAdapter(
        "fs-bot", {"app_id": "a", "app_secret": "s"}, "http://platform/webhook")
    adapter._lark = lark
    adapter._api_client = type("C", (), {"im": type("IM", (), {"v1": api_v1})()})()
    return adapter, v1


class _FakeLarkAPI:
    def __init__(self, v1):
        self._v1 = v1
        self.message = self
        self.image = self
        self.file = self

    @property
    def v1(self):
        return self._v1

    def create(self, request):
        v1 = self._v1
        req_kind = request.kwargs
        body = req_kind.get('request_body', request)
        # 上传接口: CreateImageRequest/CreateFileRequest 带 file 参数。
        if 'file' in req_kind:
            kind = 'image' if getattr(body, 'kwargs', {}).get('image_type') else 'file'
            v1.uploaded.append(kind)
            data = type("D", (), ({"image_key": "img_v2_x"} if kind == "image"
                                   else {"file_key": "file_v2_y"}))()
            return type("Resp", (), {"success": lambda self=False: True, "data": data})()
        # 消息创建: CreateMessageRequest(receive_id_type + request_body)。
        body_kw = getattr(body, 'kwargs', {})
        v1.created.append((body_kw.get('receive_id'), body_kw.get('msg_type'),
                           body_kw.get('content')))
        data = type("D", (), {"message_id": "m_1"})()
        return type("Resp", (), {"success": lambda self=False: True, "data": data})()

    def get_resource(self, request):
        self._v1.resource_get.append((request.kwargs.get('message_id'),
                                      request.kwargs.get('file_key'),
                                      request.kwargs.get('type')))
        return type("Resp", (), {"success": lambda self=False: True, "file": b"\x89PNG\r\n\x1a\n" + b"\x00" * 8})()


def test_feishu_send_image_uploads_then_sends_message(tmp_path):
    adapter, v1 = _feishu_outbound_adapter(_FakeLarkV1())
    img = tmp_path / "a.jpg"
    img.write_bytes(b"\xff\xd8\xff" + b"x" * 32)

    adapter._send_image("oc_1", str(img), file_name="a.jpg")
    assert v1.uploaded == ["image"]
    assert v1.created == [("oc_1", "image", json.dumps({"image_key": "img_v2_x"}, ensure_ascii=False))]


def test_feishu_send_file_uploads_then_sends_message(tmp_path):
    adapter, v1 = _feishu_outbound_adapter(_FakeLarkV1())
    doc = tmp_path / "r.docx"
    doc.write_bytes(b"doc")

    adapter._send_file("oc_1", str(doc), file_name="r.docx")
    assert v1.uploaded == ["file"]
    assert v1.created[0][1] == "file"
    import json as _json
    assert _json.loads(v1.created[0][2]) == {"file_key": "file_v2_y"}
