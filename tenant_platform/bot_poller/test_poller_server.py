from __future__ import annotations

import hashlib
import hmac
import os
import random
import re
import json
import threading
import time

import requests
from PIL import Image

import tenant_platform.bot_poller.poller_server as poller_server
from tenant_platform.bot_poller.poller_server import WxBotClient, coalesce_webhook_bodies


def _body(mid: str, text: str = "", *, user: str = "u1", token: str = "", at_ms: int = 1000, media: str = ""):
    paths = [media] if media else []
    items = [{"storage_path": media, "file_name": media.rsplit("/", 1)[-1]}] if media else []
    return {
        "bot_uuid": "b1",
        "channel_type": "wechat",
        "channel_account_id": user,
        "conversation_id": "",
        "message_id": mid,
        "text": text,
        "context_token": token,
        "updates_buf": "cursor",
        "media_paths": paths,
        "media_items": items,
        "_received_at_ms": at_ms,
    }


def test_coalesces_multi_part_text_without_context_token():
    # 审查 C2: 跨批次窗口合并由生产实现 InboundCoalescingBuffer 承担;
    # coalesce_webhook_bodies 已收敛为恒等路径。
    buffer = poller_server.InboundCoalescingBuffer(window_ms=1500)
    ready = buffer.push([
        _body("m1", "把这个文件"),
        _body("m2", "整理成 Word", at_ms=1600),
        _body("m3", "发给我", at_ms=2100),
    ], now_ms=1000)
    ready += buffer.flush_due(now_ms=10000)

    assert len(ready) == 1
    assert ready[0]["text"] == "把这个文件\n整理成 Word\n发给我"
    assert ready[0]["source_message_ids"] == ["m1", "m2", "m3"]
    assert ready[0]["message_id"].startswith("coalesced:")
    assert "_received_at_ms" not in ready[0]


def test_coalesces_text_and_file_with_same_context_token():
    buffer = poller_server.InboundCoalescingBuffer(window_ms=2500)
    ready = buffer.push([
        _body("m1", "整理一下", token="ctx-1"),
        _body("m2", token="ctx-1", at_ms=1800, media="b1/resume.txt"),
    ], now_ms=1000)
    ready += buffer.flush_due(now_ms=10000)

    assert len(ready) == 1
    assert ready[0]["text"] == "整理一下"
    assert ready[0]["media_paths"] == ["b1/resume.txt"]
    assert ready[0]["context_token"] == "ctx-1"


def test_different_context_tokens_merge_and_keep_latest_reply_token():
    buffer = poller_server.InboundCoalescingBuffer(window_ms=2500)
    ready = buffer.push([
        _body("m1", "第一件事", token="ctx-1"),
        _body("m2", "第二件事", token="ctx-2", at_ms=1100),
    ], now_ms=1000)
    ready += buffer.flush_due(now_ms=10000)

    assert len(ready) == 1
    assert ready[0]["text"] == "第一件事\n第二件事"
    assert ready[0]["context_token"] == "ctx-2"


def test_commands_bypass_aggregation():
    buffer = poller_server.InboundCoalescingBuffer(window_ms=2500)
    ready = buffer.push([
        _body("m1", "/stop"),
        _body("m2", "后续文字", at_ms=1100),
    ], now_ms=1000)
    ready += buffer.flush_due(now_ms=10000)

    assert [item["message_id"] for item in ready] == ["m1", "m2"]


def test_messages_outside_window_do_not_merge():
    buffer = poller_server.InboundCoalescingBuffer(window_ms=1500)
    ready = buffer.push([
        _body("m1", "第一段", at_ms=1000),
        _body("m2", "第二段", at_ms=4000),
    ], now_ms=1000)
    ready += buffer.flush_due(now_ms=10000)

    assert [item["message_id"] for item in ready] == ["m1", "m2"]


def test_coalescing_buffer_merges_text_and_file_across_poll_batches():
    buffer_type = getattr(poller_server, "InboundCoalescingBuffer", None)
    assert buffer_type is not None, "cross-batch coalescing buffer is required"
    buffer = buffer_type(window_ms=2500)

    assert buffer.push([_body("m1", "整理成 Word", token="ctx-1", at_ms=1000)], now_ms=1000) == []
    assert buffer.push([
        _body("m2", token="ctx-2", at_ms=1800, media="b1/resume.txt")
    ], now_ms=1800) == []
    assert buffer.flush_due(now_ms=4299) == []

    ready = buffer.flush_due(now_ms=4300)
    assert len(ready) == 1
    assert ready[0]["text"] == "整理成 Word"
    assert ready[0]["media_paths"] == ["b1/resume.txt"]
    assert ready[0]["source_message_ids"] == ["m1", "m2"]
    assert ready[0]["context_token"] == "ctx-2"


def _media_adapter(media_root):
    return poller_server.WeChatAdapter(
        "b1", {"token": "test"}, "http://platform/webhook",
        media_root=media_root, webhook_secret="",
    )


def test_collect_media_items_restores_original_file_name(tmp_path):
    """Round8 审查: 落盘名含内容 hash 前缀时, media_items.file_name 必须恢复
    为发送者原始文件名(不得暴露 hash 前缀)。"""
    adapter = _media_adapter(str(tmp_path))
    paths = [str(tmp_path / "b1" / "a1b2c3d4e5_resume.txt")]
    names = ["resume.txt"]
    items = adapter._collect_media_items(paths, names)
    assert len(items) == 1
    assert items[0]["file_name"] == "resume.txt"
    assert items[0]["storage_path"] == "b1/a1b2c3d4e5_resume.txt"
    # 无 names 时回退 basename。
    fallback = adapter._collect_media_items(paths)
    assert fallback[0]["file_name"] == "a1b2c3d4e5_resume.txt"


def test_collect_media_items_names_align_with_paths_not_item_list(tmp_path):
    """Round8(review): names 与 paths 同序对齐——item_list 中下载失败的项
    不产生 path, 若按位置索引 item_list 会错位(张冠李戴)。"""
    adapter = _media_adapter(str(tmp_path))
    # item_list 3 项, 中间项下载失败 → 只返回 2 个 path。
    paths = [str(tmp_path / "b1" / "a1b2c3d4e5_a.txt"), str(tmp_path / "b1" / "f6e7d8c9b0a1_c.txt")]
    names = ["a.txt", "c.txt"]
    items = adapter._collect_media_items(paths, names)
    assert [it["file_name"] for it in items] == ["a.txt", "c.txt"]
    # names 短于 paths 时剩余回退 basename。
    short = adapter._collect_media_items(paths, ["a.txt"])
    assert short[1]["file_name"] == "f6e7d8c9b0a1_c.txt"


def test_collect_media_items_restores_original_name_without_media_root():
    """Round8: 未配置 media_root 时同样恢复原始文件名。"""
    adapter = _media_adapter(None)
    paths = ["/tmp/x/a1b2c3d4e5_report.docx"]
    items = adapter._collect_media_items(paths, ["report.docx"])
    assert items[0]["file_name"] == "report.docx"


def test_file_upload_key_is_random_hex_not_filename(tmp_path):
    path = tmp_path / "简历方法.docx"
    path.write_bytes(b"docx")
    client = WxBotClient(token="test", persist=False)

    body = client._build_upload_body(
        path,
        path.read_bytes(),
        bytes.fromhex("00" * 16),
        "file_item",
        ciphertext_size=16,
        thumb_raw=b"",
        thumb_ciphertext_size=0,
    )

    assert re.fullmatch(r"[0-9a-f]{32}", body["filekey"])


def test_fit_static_image_for_upload_compresses_oversized_png(tmp_path):
    """2026-08-14 生产事故回归: 微信 CDN 大文件上传被限速/断连, 超限静态图
    必须在交付侧转 JPEG 压缩到 ≤300KB 再上传。超限 PNG → 合规 JPEG;
    小图/非图片/动图不转换返回 None。"""
    client = WxBotClient(token="test", persist=False)
    big = tmp_path / "big.png"
    # 1024x1024 噪声 PNG ≈ 1.4MB, 必然超限(稀疏噪声会被 PNG 压缩, 必须逐像素随机)
    im = Image.frombytes("RGB", (1024, 1024), random.Random(42).randbytes(1024 * 1024 * 3))
    im.save(big, format="PNG")
    assert big.stat().st_size > client._IMAGE_UPLOAD_MAX_BYTES

    out = client._fit_static_image_for_upload(str(big))
    assert out is not None, "oversized png must be adapted"
    try:
        assert out.stat().st_size <= client._IMAGE_UPLOAD_MAX_BYTES
        with Image.open(str(out)) as adapted:
            assert adapted.format == "JPEG"
    finally:
        out.unlink(missing_ok=True)

    # 小图不转换
    small = tmp_path / "small.png"
    Image.new("RGB", (64, 64), (10, 20, 30)).save(small, format="PNG")
    assert client._fit_static_image_for_upload(str(small)) is None

    # 非图片/不存在 → None(调用方回退原图, 错误语义不变)
    assert client._fit_static_image_for_upload(str(tmp_path / "nope.bin")) is None
    assert client._fit_static_image_for_upload(str(tmp_path / "missing.png")) is None

    # 源目录只读时仍可适配(mkstemp 落可写临时目录, 2026-08-14 生产实证:
    # poller 只读 rootfs + 只读 spool 卷, 写源文件旁目录会静默失败回退原图)
    try:
        os.chmod(tmp_path, 0o555)
        ro = tmp_path / "big.png"
        out_ro = client._fit_static_image_for_upload(str(ro))
        assert out_ro is not None, "must adapt even when source dir is read-only"
        assert out_ro.stat().st_size <= client._IMAGE_UPLOAD_MAX_BYTES
        assert str(out_ro).startswith(str(out_ro.parent))  # 落在可写目录
        out_ro.unlink(missing_ok=True)
    finally:
        os.chmod(tmp_path, 0o755)


def test_fit_image_for_upload_channel_format_whitelists(tmp_path):
    """2026-08-14 官方文档对齐: 企微图片仅 JPG/PNG、钉钉 jpg/gif/png/bmp(无
    webp)。白名单外格式转 JPEG; 企微动图取首帧; 钉钉动图保留。"""
    from wxbot_client import fit_image_for_upload

    webp = tmp_path / "a.webp"
    Image.new("RGB", (64, 64), (10, 20, 30)).save(webp, format="WEBP")
    gif = tmp_path / "a.gif"
    Image.new("RGB", (32, 32), (1, 2, 3)).save(gif, format="GIF")
    png = tmp_path / "a.png"
    Image.new("RGB", (32, 32), (4, 5, 6)).save(png, format="PNG")

    # 企微(仅 JPG/PNG): webp → JPEG, png 原样
    w1 = fit_image_for_upload(str(webp), max_bytes=10 << 20,
                              allowed_formats={"JPEG", "PNG"}, animated_ok=False)
    assert w1 is not None
    try:
        with Image.open(str(w1)) as _im:
            assert _im.format == "JPEG"
    finally:
        w1.unlink(missing_ok=True)
    assert fit_image_for_upload(str(png), max_bytes=10 << 20,
                                allowed_formats={"JPEG", "PNG"}, animated_ok=False) is None
    # 企微 GIF(动图) → 首帧 JPEG(animated_ok=False)
    w2 = fit_image_for_upload(str(gif), max_bytes=10 << 20,
                              allowed_formats={"JPEG", "PNG"}, animated_ok=False)
    assert w2 is not None
    try:
        with Image.open(str(w2)) as _im:
            assert _im.format == "JPEG"
    finally:
        w2.unlink(missing_ok=True)

    # 钉钉(jpg/gif/png/bmp): webp → JPEG, gif 保留
    d1 = fit_image_for_upload(str(webp), max_bytes=20 << 20,
                              allowed_formats={"JPEG", "PNG", "GIF", "BMP"}, animated_ok=True)
    assert d1 is not None
    try:
        with Image.open(str(d1)) as _im:
            assert _im.format == "JPEG"
    finally:
        d1.unlink(missing_ok=True)
    assert fit_image_for_upload(str(gif), max_bytes=20 << 20,
                                allowed_formats={"JPEG", "PNG", "GIF", "BMP"}, animated_ok=True) is None


def test_build_upload_body_defaults_no_need_thumb(tmp_path):
    """2026-08-14 官方协议对齐: openclaw 默认 no_need_thumb=true(只传主图,
    不上传缩略图)。无 thumb_raw 时请求体必须 no_need_thumb=True。"""
    path = tmp_path / "img.png"
    path.write_bytes(b"png")
    client = WxBotClient(token="test", persist=False)
    body = client._build_upload_body(path, b"png", b"\x00" * 16, "image_item",
                                     ciphertext_size=32, thumb_raw=b"", thumb_ciphertext_size=0)
    assert body["no_need_thumb"] is True
    body2 = client._build_upload_body(path, b"png", b"\x00" * 16, "image_item",
                                      ciphertext_size=32, thumb_raw=b"t", thumb_ciphertext_size=16)
    assert body2["no_need_thumb"] is False


def test_send_image_adapts_oversized_static_image(tmp_path):
    """send_image 对超限静态图走适配路径(JPEG ≤300KB), 协议调用面不变。"""
    client = WxBotClient(token="test", persist=False)
    big = tmp_path / "big.png"
    Image.frombytes("RGB", (1024, 1024), random.Random(7).randbytes(1024 * 1024 * 3)).save(big, format="PNG")
    assert big.stat().st_size > client._IMAGE_UPLOAD_MAX_BYTES

    calls = []
    uploaded = []

    def fake_post(ep, body, timeout=15):
        calls.append(ep)
        if ep == "ilink/bot/getuploadurl":
            return {"upload_param": "UP", "upload_full_url": ""}
        if ep == "ilink/bot/sendmessage":
            assert "image_item" in body["msg"]["item_list"][0]
            return {"errcode": 0}
        raise AssertionError(ep)

    client._post = fake_post
    client._upload = lambda *a, **k: uploaded.append(a[2]) or {
        "encrypt_query_param": "EQ", "aes_key": "x", "encrypt_type": 1}

    client.send_image("u1", str(big), context_token="ctx")
    assert calls == ["ilink/bot/getuploadurl", "ilink/bot/sendmessage"]
    assert len(uploaded) == 1
    raw = uploaded[0]
    assert len(raw) <= client._IMAGE_UPLOAD_MAX_BYTES
    assert raw[:2] == b"\xff\xd8", "adapted upload must be JPEG"


def test_run_coalesces_across_polls_and_uses_deadline_as_request_timeout(monkeypatch):
    clock = [1.0]

    class Client:
        def __init__(self):
            self.calls = 0
            self.timeouts = []
            self.updates_buf = "cursor-0"

        def get_updates(self, timeout, request_timeout=None):
            self.timeouts.append(request_timeout)
            self.calls += 1
            if self.calls == 1:
                self.updates_buf = "cursor-1"
                return [{
                    "message_id": "m1", "message_type": 1, "from_user_id": "u1",
                    "context_token": "ctx-1", "create_time_ms": 1000,
                    "text": "整理成 Word", "item_list": [],
                }]
            if self.calls == 2:
                clock[0] = 1.8
                self.updates_buf = "cursor-2"
                return [{
                    "message_id": "m2", "message_type": 1, "from_user_id": "u1",
                    "context_token": "ctx-2", "create_time_ms": 1800,
                    "text": "", "item_list": [],
                }]
            clock[0] = 4.3
            return []

        @staticmethod
        def is_user_msg(msg):
            return msg.get("message_type") == 1

        @staticmethod
        def extract_text(msg):
            return msg.get("text", "")

    monkeypatch.setattr(poller_server.time, "time", lambda: clock[0])
    client = Client()
    adapter = poller_server.WeChatAdapter(
        "b1", {"token": "test"}, "http://platform/webhook",
        updates_buf="cursor-0",
        coalesce_window_provider=lambda: 2500,
    )
    adapter.client = client
    adapter._media_dir = None
    posted = []

    def post(body, max_attempts=None):
        posted.append(body)
        adapter.stop_event.set()
        return True

    monkeypatch.setattr(adapter, "post_webhook", post)
    adapter._run()

    assert client.timeouts == [None, 2.5, 2.5]
    assert len(posted) == 1
    assert posted[0]["source_message_ids"] == ["m1", "m2"]
    assert posted[0]["updates_buf"] == "cursor-2"
    assert posted[0]["context_token"] == "ctx-2"
    assert posted[0]["channel_type"] == "wechat"
    assert posted[0]["conversation_id"] == ""
    assert posted[0]["channel_account_id"] == "u1"


def test_webhook_success_advances_committed_cursor(monkeypatch):
    class Response:
        status_code = 200
        text = "ok"

    monkeypatch.setattr(poller_server.requests, "post", lambda *args, **kwargs: Response())
    adapter = poller_server.WeChatAdapter(
        "b1", {"token": "test"}, "http://platform/webhook", updates_buf="cursor-before")

    delivered = adapter.post_webhook(_body("m1") | {"updates_buf": "cursor-after"})

    assert delivered is True
    assert adapter.committed_updates_buf == "cursor-after"


def test_stop_waits_for_inflight_webhook_before_returning_cursor():
    class Client:
        updates_buf = "cursor-old"

    class Thread:
        def join(self, timeout):
            return None

    manager = poller_server.BotManager(inbound_coalesce_window_ms=2500)
    adapter = poller_server.WeChatAdapter(
        "b1", {"token": "test"}, "http://platform/webhook", updates_buf="cursor-old")
    adapter.thread = Thread()
    adapter.webhook_idle = poller_server.threading.Event()
    adapter.client = Client()
    manager._adapters["b1"] = adapter

    def finish_webhook():
        poller_server.time.sleep(0.05)
        adapter.committed_updates_buf = "cursor-new"
        adapter.webhook_idle.set()

    worker = poller_server.threading.Thread(target=finish_webhook)
    worker.start()
    returned_cursor = manager.stop("b1")
    worker.join(timeout=1)

    assert returned_cursor == "cursor-new"


def test_stop_does_not_commit_cursor_for_pending_debounce_message():
    class Client:
        updates_buf = "cursor-after-pending"

    class Thread:
        def join(self, timeout):
            return None

    manager = poller_server.BotManager(inbound_coalesce_window_ms=2500)
    adapter = poller_server.WeChatAdapter(
        "b1", {"token": "test"}, "http://platform/webhook",
        updates_buf="cursor-before-pending",
        coalesce_window_provider=lambda: 2500,
    )
    adapter.thread = Thread()
    adapter.client = Client()
    # 审查 I-3: 合并缓冲统一到基类 _coalescer(微信不再自建)。
    adapter._coalescer.push([_body("m1") | {"updates_buf": "cursor-after-pending"}], now_ms=1000)
    manager._adapters["b1"] = adapter

    returned_cursor = manager.stop("b1")

    assert returned_cursor == "cursor-before-pending"


def test_zero_window_disables_aggregation():
    merged = coalesce_webhook_bodies([
        _body("m1", "第一段"),
        _body("m2", "第二段", at_ms=1001),
    ])

    assert [item["message_id"] for item in merged] == ["m1", "m2"]


# 审查 R5-I10: /send 的 file_name 必须透传到 WxBotClient.send_file——显示名
# 由 Platform 显式传入, 不能回退到快照临时路径 basename(含 marker hash 前缀)。
def test_send_passes_explicit_file_name_to_client(monkeypatch, tmp_path):
    class Client:
        def __init__(self):
            self.calls = []

        def send_file(self, ilink_user_id, file_path, context_token="", file_name="", client_id=None):
            self.calls.append((ilink_user_id, file_path, context_token, file_name, client_id))

        def send_text(self, ilink_user_id, text, context_token="", client_id=None):
            self.calls.append(("text", ilink_user_id, text, context_token, client_id))

    manager = poller_server.BotManager(inbound_coalesce_window_ms=2500)
    adapter = poller_server.WeChatAdapter(
        "b1", {"token": "test"}, "http://platform/webhook", updates_buf="")
    adapter.client = Client()
    manager._adapters["b1"] = adapter

    docx = tmp_path / "report.docx"
    docx.write_bytes(b"doc")
    manager.send("b1", "u1", "", msg_type="file", file_path=str(docx), file_name="report.docx")
    assert adapter.client.calls == [("u1", str(docx), "", "report.docx", "")]

    # 未传 file_name 时回退为空串(客户端侧回退本地 basename), 不报错。
    other = tmp_path / "other.docx"
    other.write_bytes(b"doc")
    adapter.client.calls.clear()
    manager.send("b1", "u1", "", msg_type="file", file_path=str(other))
    assert adapter.client.calls == [("u1", str(other), "", "", "")]

    # round9 审查: client_id 透传到客户端(稳定幂等键)。
    adapter.client.calls.clear()
    manager.send("b1", "u1", "hi", client_id="ga-abc123")
    assert adapter.client.calls == [("text", "u1", "hi", "", "ga-abc123")]


# ---------------------------------------------------------------------------
# 审查修复测试: HTTP 控制面安全(请求体上限 / 签名校验 / fail-closed 门禁 /
# 内部错误不透出)。此前无限流、空 secret 放行、异常细节透出。
# ---------------------------------------------------------------------------

def _start_handler_server(api_secret: str = ""):
    """起一个 PollerHandler 直连的 ThreadingHTTPServer(等价 serve() 装配)。"""
    from http.server import ThreadingHTTPServer

    poller_server.PollerHandler.manager = poller_server.BotManager(
        media_root=None, webhook_secret=""
    )
    poller_server.PollerHandler.api_secret = api_secret
    server = ThreadingHTTPServer(("127.0.0.1", 0), poller_server.PollerHandler)
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    return server, f"http://127.0.0.1:{server.server_port}"


def test_serve_refuses_non_loopback_without_secret():
    """空 secret + 非回环绑定必须 fail-closed 拒绝启动(审查 I-2)。"""
    try:
        poller_server.serve("0.0.0.0:18080", api_secret="")
    except SystemExit as exc:
        assert "refuses to listen" in str(exc)
    else:
        raise AssertionError("serve() should have refused non-loopback bind without secret")


def test_http_rejects_oversized_body():
    server, base = _start_handler_server()
    try:
        # 服务器拒读超限 body 直接回 413——客户端可能因连接被关闭而抛
        # ConnectionError(与 Go http.Server 行为一致: 超限不读 body)。
        # 两种形态都证明请求被拒绝, 而不是被处理。
        try:
            resp = requests.post(
                base + "/config", data=b"x" * (poller_server.MAX_BODY_BYTES + 1), timeout=5
            )
            assert resp.status_code == 413, resp.text
        except requests.exceptions.ConnectionError:
            pass  # 服务器拒收即防御生效
    finally:
        server.shutdown()
        server.server_close()


def test_http_requires_signature_when_secret_configured():
    secret = "test-secret"
    server, base = _start_handler_server(api_secret=secret)
    try:
        # 无签名 -> 401
        resp = requests.post(base + "/config", json={}, timeout=5)
        assert resp.status_code == 401, resp.text
        # 错误签名 -> 401
        resp = requests.post(
            base + "/config", json={},
            headers={"X-API-Signature": "deadbeef"}, timeout=5,
        )
        assert resp.status_code == 401, resp.text
        # 正确签名 -> 200
        body = b'{"inbound_coalesce_window_ms": 0}'
        sig = hmac.new(secret.encode(), body, hashlib.sha256).hexdigest()
        resp = requests.post(
            base + "/config", data=body,
            headers={"X-API-Signature": sig}, timeout=5,
        )
        assert resp.status_code == 200, resp.text
    finally:
        server.shutdown()
        server.server_close()


def test_http_hides_internal_error_details():
    server, base = _start_handler_server()
    try:
        # /send 传非法 msg_type -> 400, 不带内部细节
        resp = requests.post(base + "/send", json={"bot_uuid": "b1", "msg_type": "evil"}, timeout=5)
        assert resp.status_code == 400, resp.text
        assert "invalid msg_type" in resp.json()["error"]
        # /config 传不可转 int 的值触发内部异常 -> 500 固定文案, 不透出 trace
        resp = requests.post(base + "/config", json={"inbound_coalesce_window_ms": "abc"}, timeout=5)
        assert resp.status_code in (400, 500)
        assert "internal error" in resp.json().get("error", "").lower() or "invalid request body" in resp.json().get("error", "").lower()
    finally:
        server.shutdown()
        server.server_close()


def test_coalesces_out_of_order_timestamps_in_same_batch():
    """审查回归: 微信"文件+文字"一起发送时, iLink 同一批次内消息的时间戳
    可能乱序(文件 create_time 晚于文字, 但返回顺序文字在前或反之)。
    _can_coalesce 曾要求 current_at >= previous_at(严格递增), 乱序时拒绝
    合并 → 文件/文字各成一个任务(用户侧表现为"回复两个")。窗口语义应是
    时间接近即合并, 不要求顺序。"""
    buffer = poller_server.InboundCoalescingBuffer(window_ms=2500)
    # 模拟真实时序: 文字 create_time=1000, 文件 create_time=1765(差 765ms < 窗口),
    # 同一批次内按 [text, file] 顺序 push(与 iLink 返回顺序一致)。
    ready = buffer.push([
        _body("m1", "这个文档排版美观点转成word 给我", at_ms=1000),
        _body("m2", at_ms=1765, media="b1/resume.docx"),
    ], now_ms=1765)
    ready += buffer.flush_due(now_ms=10000)

    assert len(ready) == 1, f"out-of-order timestamps must still coalesce, got {len(ready)}"
    assert ready[0]["message_id"].startswith("coalesced:")
    assert ready[0]["text"] == "这个文档排版美观点转成word 给我"
    assert ready[0]["media_paths"] == ["b1/resume.docx"]


def test_coalesces_reversed_batch_order():
    """同窗口内 iLink 返回顺序与时间戳顺序相反(文件在前文字在后)。"""
    buffer = poller_server.InboundCoalescingBuffer(window_ms=2500)
    ready = buffer.push([
        _body("m2", at_ms=1765, media="b1/resume.docx"),
        _body("m1", "整理一下", at_ms=1000),
    ], now_ms=1765)
    ready += buffer.flush_due(now_ms=10000)

    assert len(ready) == 1, f"reversed order must still coalesce, got {len(ready)}"
    assert ready[0]["message_id"].startswith("coalesced:")
    assert ready[0]["media_paths"] == ["b1/resume.docx"]
    assert ready[0]["text"] == "整理一下"


# ---------------------------------------------------------------------------
# 媒体留存清扫(2026-08-13 审查 I4/D7): media_root 按 mtime 90d 清扫。
# ---------------------------------------------------------------------------

def test_media_sweep_removes_expired_keeps_fresh(tmp_path):
    manager = poller_server.BotManager(media_root=str(tmp_path))
    bot_dir = tmp_path / "bot-1"
    bot_dir.mkdir()
    old = bot_dir / "old.jpg"
    fresh = bot_dir / "fresh.jpg"
    old.write_bytes(b"x")
    fresh.write_bytes(b"x")
    past = time.time() - 91 * 86400
    os.utime(old, (past, past))

    removed = manager._sweep_media(90)
    assert removed == 1
    assert not old.exists()
    assert fresh.exists()


def test_media_sweep_reclaims_empty_bot_dir(tmp_path):
    manager = poller_server.BotManager(media_root=str(tmp_path))
    bot_dir = tmp_path / "bot-2"
    bot_dir.mkdir()
    (bot_dir / "gone.jpg").write_bytes(b"x")
    past = time.time() - 91 * 86400
    os.utime(bot_dir / "gone.jpg", (past, past))
    manager._sweep_media(90)
    assert not bot_dir.exists()  # 文件清空后目录回收


def test_media_sweep_without_root_is_noop():
    manager = poller_server.BotManager(media_root=None)
    assert manager._sweep_media(90) == 0


# ---------------------------------------------------------------------------
# 2026-08-14 审查 I-3: 入站合并投递统一到基类(四渠道 + 微信共用)
# ---------------------------------------------------------------------------

class _DeliverAdapter(poller_server.BotAdapter):
    """最小事件渠道适配器: 只验证基类合并投递语义。"""

    channel_type = "test"

    def _run(self):
        pass

    def send_text(self, target, text, client_id=''):
        pass


def test_deliver_inbound_zero_window_posts_immediately(monkeypatch):
    adapter = _DeliverAdapter("b1", "http://platform/webhook",
                              coalesce_window_provider=lambda: 0)
    posted = []
    monkeypatch.setattr(adapter, "post_webhook", lambda body: posted.append(body))
    adapter.deliver_inbound([_body("m1", "第一段")], now_ms=1000)
    adapter.deliver_inbound([_body("m2", "第二段", at_ms=1100)], now_ms=1100)
    assert [b["message_id"] for b in posted] == ["m1", "m2"]  # 零延迟, 不合并


def test_deliver_inbound_coalesces_adjacent_and_flushes_on_timer(monkeypatch):
    window_ms = 300
    adapter = _DeliverAdapter("b1", "http://platform/webhook",
                              coalesce_window_provider=lambda: window_ms)
    posted = []
    monkeypatch.setattr(adapter, "post_webhook", lambda body: posted.append(body))
    now = int(time.time() * 1000)
    # 图消息 + 相邻文本消息(同一会话): 合并为一组, 不立即投递。
    adapter.deliver_inbound([_body("m1", "", at_ms=now, media="attachments/F001_x.png")], now_ms=now)
    adapter.deliver_inbound([_body("m2", "这是啥", at_ms=now + 50)], now_ms=now + 50)
    assert posted == []
    # 窗口到期(定时器)整组 flush: 文本 + 媒体合并在同一 body。
    deadline = now + window_ms + 1000
    for _ in range(100):
        if posted:
            break
        time.sleep(0.05)
    assert len(posted) == 1
    merged = posted[0]
    assert merged["message_id"].startswith("coalesced:")
    assert merged["text"] == "这是啥"
    assert merged["media_paths"] == ["attachments/F001_x.png"]
    assert merged["source_message_ids"] == ["m1", "m2"]


def test_deliver_inbound_command_message_never_delayed(monkeypatch):
    adapter = _DeliverAdapter("b1", "http://platform/webhook",
                              coalesce_window_provider=lambda: 300)
    posted = []
    monkeypatch.setattr(adapter, "post_webhook", lambda body: posted.append(body))
    now = int(time.time() * 1000)
    adapter.deliver_inbound([_body("m1", "/session.max_turns=8")], now_ms=now)
    assert [b["message_id"] for b in posted] == ["m1"]  # 命令立即 flush


def test_deliver_inbound_different_conversations_never_merge(monkeypatch):
    adapter = _DeliverAdapter("b1", "http://platform/webhook",
                              coalesce_window_provider=lambda: 300)
    posted = []
    monkeypatch.setattr(adapter, "post_webhook", lambda body: posted.append(body))
    now = int(time.time() * 1000)
    body1 = _body("m1", "甲图", at_ms=now, media="attachments/F001_a.png")
    body1["conversation_id"] = "conv-a"
    body2 = _body("m2", "乙问", at_ms=now + 50)
    body2["conversation_id"] = "conv-b"
    adapter.deliver_inbound([body1], now_ms=now)
    adapter.deliver_inbound([body2], now_ms=now + 50)
    deadline = now + 300 + 1000
    for _ in range(100):
        if len(posted) == 2:
            break
        time.sleep(0.05)
    assert len(posted) == 2  # 不同会话各自成组
    assert [b["message_id"] for b in posted] == ["m1", "m2"]


def test_deliver_inbound_stop_cancels_flush_timer():
    adapter = _DeliverAdapter("b1", "http://platform/webhook",
                              coalesce_window_provider=lambda: 300)
    now = int(time.time() * 1000)
    adapter.deliver_inbound([_body("m1", "待合并")], now_ms=now)
    assert adapter._flush_timer is not None
    adapter.stop()
    assert adapter._flush_timer is None
    # 定时器已取消: pending 不再投递(无 post_webhook 路径可触发)。
    assert adapter._coalescer.flush_all() != []  # 数据仍在缓冲中, 由下次启动承接


def test_health_exposes_inbound_coalesce_window():
    """2026-08-15 可观测性: /health 必须暴露当前合并窗口, 平台对账/诊断依赖
    它(曾因 poller 重启窗口回 0 且不可见, 静默失效 18 小时)。"""
    import urllib.request

    server, base = _start_handler_server()
    try:
        # 默认窗口 0(未配置); /config 设置后 health 应反映新值。
        with urllib.request.urlopen(base + "/health", timeout=5) as resp:
            body = json.loads(resp.read().decode("utf-8"))
        assert body["inbound_coalesce_window_ms"] == 0, body

        resp = requests.post(base + "/config",
                             json={"inbound_coalesce_window_ms": 2500}, timeout=5)
        assert resp.status_code == 200, resp.text

        with urllib.request.urlopen(base + "/health", timeout=5) as resp:
            body = json.loads(resp.read().decode("utf-8"))
        assert body["inbound_coalesce_window_ms"] == 2500, body
        assert body["healthy"] is True
    finally:
        server.shutdown()
