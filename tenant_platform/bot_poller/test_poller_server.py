from __future__ import annotations

import re

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
