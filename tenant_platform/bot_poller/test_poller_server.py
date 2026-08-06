from __future__ import annotations

import hashlib
import hmac
import re
import threading

import requests

import tenant_platform.bot_poller.poller_server as poller_server
from tenant_platform.bot_poller.poller_server import WxBotClient, coalesce_webhook_bodies


def _body(mid: str, text: str = "", *, user: str = "u1", token: str = "", at_ms: int = 1000, media: str = ""):
    paths = [media] if media else []
    items = [{"storage_path": media, "file_name": media.rsplit("/", 1)[-1]}] if media else []
    return {
        "bot_uuid": "b1",
        "ilink_user_id": user,
        "message_id": mid,
        "text": text,
        "context_token": token,
        "updates_buf": "cursor",
        "media_paths": paths,
        "media_items": items,
        "_received_at_ms": at_ms,
    }


def test_coalesces_multi_part_text_without_context_token():
    merged = coalesce_webhook_bodies([
        _body("m1", "把这个文件"),
        _body("m2", "整理成 Word", at_ms=1600),
        _body("m3", "发给我", at_ms=2100),
    ], window_ms=1500)

    assert len(merged) == 1
    assert merged[0]["text"] == "把这个文件\n整理成 Word\n发给我"
    assert merged[0]["source_message_ids"] == ["m1", "m2", "m3"]
    assert merged[0]["message_id"].startswith("coalesced:")
    assert "_received_at_ms" not in merged[0]


def test_coalesces_text_and_file_with_same_context_token():
    merged = coalesce_webhook_bodies([
        _body("m1", "整理一下", token="ctx-1"),
        _body("m2", token="ctx-1", at_ms=1800, media="b1/resume.txt"),
    ], window_ms=2500)

    assert len(merged) == 1
    assert merged[0]["text"] == "整理一下"
    assert merged[0]["media_paths"] == ["b1/resume.txt"]
    assert merged[0]["context_token"] == "ctx-1"


def test_different_context_tokens_merge_and_keep_latest_reply_token():
    merged = coalesce_webhook_bodies([
        _body("m1", "第一件事", token="ctx-1"),
        _body("m2", "第二件事", token="ctx-2", at_ms=1100),
    ], window_ms=2500)

    assert len(merged) == 1
    assert merged[0]["text"] == "第一件事\n第二件事"
    assert merged[0]["context_token"] == "ctx-2"


def test_commands_bypass_aggregation():
    merged = coalesce_webhook_bodies([
        _body("m1", "/stop"),
        _body("m2", "后续文字", at_ms=1100),
    ], window_ms=2500)

    assert [item["message_id"] for item in merged] == ["m1", "m2"]


def test_messages_outside_window_do_not_merge():
    merged = coalesce_webhook_bodies([
        _body("m1", "第一段", at_ms=1000),
        _body("m2", "第二段", at_ms=4000),
    ], window_ms=1500)

    assert [item["message_id"] for item in merged] == ["m1", "m2"]


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


def test_collect_media_items_restores_original_file_name():
    """Round8 审查: 落盘名含内容 hash 前缀时, media_items.file_name 必须恢复
    为发送者原始文件名(不得暴露 hash 前缀)。"""
    mgr = poller_server.BotManager(media_root="/media")
    paths = ["/media/b1/a1b2c3d4e5_resume.txt"]
    names = ["resume.txt"]
    items = mgr._collect_media_items(paths, names)
    assert len(items) == 1
    assert items[0]["file_name"] == "resume.txt"
    assert items[0]["storage_path"] == "b1/a1b2c3d4e5_resume.txt"
    # 无 names 时回退 basename。
    fallback = mgr._collect_media_items(paths)
    assert fallback[0]["file_name"] == "a1b2c3d4e5_resume.txt"


def test_collect_media_items_names_align_with_paths_not_item_list():
    """Round8(review): names 与 paths 同序对齐——item_list 中下载失败的项
    不产生 path, 若按位置索引 item_list 会错位(张冠李戴)。"""
    mgr = poller_server.BotManager(media_root="/media")
    # item_list 3 项, 中间项下载失败 → 只返回 2 个 path。
    paths = ["/media/b1/a1b2c3d4e5_a.txt", "/media/b1/f6e7d8c9b0a1_c.txt"]
    names = ["a.txt", "c.txt"]
    items = mgr._collect_media_items(paths, names)
    assert [it["file_name"] for it in items] == ["a.txt", "c.txt"]
    # names 短于 paths 时剩余回退 basename。
    short = mgr._collect_media_items(paths, ["a.txt"])
    assert short[1]["file_name"] == "f6e7d8c9b0a1_c.txt"


def test_collect_media_items_restores_original_name_without_media_root():
    """Round8: 未配置 media_root 时同样恢复原始文件名。"""
    mgr = poller_server.BotManager(media_root=None)
    paths = ["/tmp/x/a1b2c3d4e5_report.docx"]
    items = mgr._collect_media_items(paths, ["report.docx"])
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
    entry = poller_server.BotEntry(client, "http://platform/webhook", "b1", 2500)
    entry.media_dir = None
    manager = poller_server.BotManager(inbound_coalesce_window_ms=2500)
    posted = []

    def post(_entry, body, max_attempts=None):
        posted.append(body)
        entry.stop_event.set()
        return True

    monkeypatch.setattr(manager, "_post_webhook_body", post)
    manager._run(entry)

    assert client.timeouts == [None, 2.5, 2.5]
    assert len(posted) == 1
    assert posted[0]["source_message_ids"] == ["m1", "m2"]
    assert posted[0]["updates_buf"] == "cursor-2"
    assert posted[0]["context_token"] == "ctx-2"


def test_webhook_success_advances_committed_cursor(monkeypatch):
    class Response:
        status_code = 200
        text = "ok"

    monkeypatch.setattr(poller_server.requests, "post", lambda *args, **kwargs: Response())
    entry = poller_server.BotEntry(object(), "http://platform/webhook", "b1", 2500)
    entry.committed_updates_buf = "cursor-before"
    manager = poller_server.BotManager(inbound_coalesce_window_ms=2500)

    delivered = manager._post_webhook_body(entry, _body("m1") | {"updates_buf": "cursor-after"})

    assert delivered is True
    assert entry.committed_updates_buf == "cursor-after"


def test_stop_waits_for_inflight_webhook_before_returning_cursor():
    class Client:
        updates_buf = "cursor-old"

    class Thread:
        def join(self, timeout):
            return None

    manager = poller_server.BotManager(inbound_coalesce_window_ms=2500)
    entry = poller_server.BotEntry(Client(), "http://platform/webhook", "b1", 2500)
    entry.thread = Thread()
    entry.committed_updates_buf = "cursor-old"
    entry.webhook_idle = poller_server.threading.Event()
    manager._bots["b1"] = entry

    def finish_webhook():
        poller_server.time.sleep(0.05)
        entry.committed_updates_buf = "cursor-new"
        entry.webhook_idle.set()

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
    entry = poller_server.BotEntry(Client(), "http://platform/webhook", "b1", 2500)
    entry.thread = Thread()
    entry.committed_updates_buf = "cursor-before-pending"
    entry.coalescer.push([_body("m1") | {"updates_buf": "cursor-after-pending"}], now_ms=1000)
    manager._bots["b1"] = entry

    returned_cursor = manager.stop("b1")

    assert returned_cursor == "cursor-before-pending"


def test_zero_window_disables_aggregation():
    merged = coalesce_webhook_bodies([
        _body("m1", "第一段"),
        _body("m2", "第二段", at_ms=1001),
    ], window_ms=0)

    assert [item["message_id"] for item in merged] == ["m1", "m2"]


# 审查 R5-I10: /send 的 file_name 必须透传到 WxBotClient.send_file——显示名
# 由 Platform 显式传入, 不能回退到快照临时路径 basename(含 marker hash 前缀)。
def test_send_passes_explicit_file_name_to_client(monkeypatch):
    class Client:
        def __init__(self):
            self.calls = []

        def send_file(self, ilink_user_id, file_path, context_token="", file_name="", client_id=None):
            self.calls.append((ilink_user_id, file_path, context_token, file_name, client_id))

        def send_text(self, ilink_user_id, text, context_token="", client_id=None):
            self.calls.append(("text", ilink_user_id, text, context_token, client_id))

    manager = poller_server.BotManager(inbound_coalesce_window_ms=2500)
    entry = poller_server.BotEntry(Client(), "http://platform/webhook", "b1", 2500)
    manager._bots["b1"] = entry

    manager.send("b1", "u1", "", msg_type="file", file_path="/tmp/abc123_report.docx", file_name="report.docx")
    assert entry.client.calls == [("u1", "/tmp/abc123_report.docx", "", "report.docx", "")]

    # 未传 file_name 时回退为空串(客户端侧回退本地 basename), 不报错。
    entry.client.calls.clear()
    manager.send("b1", "u1", "", msg_type="file", file_path="/tmp/abc123_other.docx")
    assert entry.client.calls == [("u1", "/tmp/abc123_other.docx", "", "", "")]

    # round9 审查: client_id 透传到客户端(稳定幂等键)。
    entry.client.calls.clear()
    manager.send("b1", "u1", "hi", client_id="ga-abc123")
    assert entry.client.calls == [("text", "u1", "hi", "", "ga-abc123")]


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
