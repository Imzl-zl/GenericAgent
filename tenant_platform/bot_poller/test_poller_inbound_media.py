"""四渠道入站媒体提取测试(IM_MEDIA_ARCHITECTURE §3.1 落地, 审查 B1)。

覆盖:
  * media_downloader 公共下载器: host 白名单/https/大小上限/原子落盘/
    魔数嗅探/路径穿越清洗;
  * QQ attachments[].url 直下(host 白名单) → webhook media_paths/media_items;
  * 飞书 image_key/file_key → 资源下载 API(monkeypatch) → 落盘;
  * 钉钉 downloadCode → v1.0 下载端点(monkeypatch) + 语音 ASR 文本注入;
  * 企微直链 URL 下载 / media_id 无 token 时丢弃;
  * 统一行为: 无文本且媒体提取失败 → 不投递(不再回 "empty message ignored")。

真实渠道冒烟仍需用户凭据(QQ/飞书已配置, 钉钉/企微下载 API 待实测)。
"""

from __future__ import annotations

import json

import pytest

import tenant_platform.bot_poller.media_downloader as media_dl
import tenant_platform.bot_poller.poller_server as poller_server


# ---------------------------------------------------------------------------
# media_downloader 公共下载器
# ---------------------------------------------------------------------------

def test_safe_filename_strips_path_traversal():
    assert media_dl.safe_filename("..\\..\\evil.py") == "evil.py"
    assert media_dl.safe_filename("../../etc/passwd", ".jpg") == "passwd.jpg"
    assert media_dl.safe_filename("") != ""  # 随机回退
    assert media_dl.safe_filename("..") != ".."
    # ext 缺失时补扩展名(飞书 image 无文件名, 靠魔数嗅探的 ext 落盘)。
    assert media_dl.safe_filename("img_v2_abc", ".jpg") == "img_v2_abc.jpg"
    assert media_dl.safe_filename("photo.jpg", ".jpg") == "photo.jpg"


def test_sniff_image_ext():
    assert media_dl.sniff_image_ext(b"\xff\xd8\xff\xe0" + b"\x00" * 8) == ".jpg"
    assert media_dl.sniff_image_ext(b"\x89PNG\r\n\x1a\n" + b"\x00" * 8) == ".png"
    assert media_dl.sniff_image_ext(b"GIF89a" + b"\x00" * 8) == ".gif"
    assert media_dl.sniff_image_ext(b"RIFF\x10\x00\x00\x00WEBPVP8 ") == ".webp"
    assert media_dl.sniff_image_ext(b"BM\x36\x00\x00\x00") == ".bmp"
    assert media_dl.sniff_image_ext(b"not an image") == ""


def test_download_url_bounded_rejects_host_and_scheme(monkeypatch):
    calls = []

    def fake_get(url, **kwargs):
        calls.append(url)
        raise AssertionError("must not be reached")

    monkeypatch.setattr(media_dl.requests, "get", fake_get)
    with pytest.raises(ValueError, match="host"):
        media_dl.download_url_bounded(
            "https://evil.example.com/x.jpg", "/tmp", allowed_hosts=media_dl.QQ_MEDIA_HOSTS)
    with pytest.raises(ValueError, match="scheme"):
        media_dl.download_url_bounded(
            "http://multimedia.nt.qq.com.cn/x.jpg", "/tmp", allowed_hosts=media_dl.QQ_MEDIA_HOSTS)
    # 审查 I-1: 空白名单 = 拒绝全部(fail-closed)——docstring 承诺与实现
    # 曾相反(漏传白名单即放行任意 https 主机), 回归测试钉死契约。
    with pytest.raises(ValueError, match="whitelist is empty"):
        media_dl.download_url_bounded("https://evil.example.com/x.jpg", "/tmp")
    assert calls == []


def test_download_url_bounded_size_cap_and_atomic_write(monkeypatch, tmp_path):
    payload = b"\xff\xd8\xff" + b"x" * 100

    class _Resp:
        headers = {"Content-Length": str(len(payload))}

        def __enter__(self):
            return self

        def __exit__(self, *exc):
            return False

        def raise_for_status(self):
            pass

        def iter_content(self, chunk_size):
            yield payload

    monkeypatch.setattr(media_dl.requests, "get",
                        lambda url, **kw: _Resp())
    path = media_dl.download_url_bounded(
        "https://multimedia.nt.qq.com.cn/download?appid=1&fileid=2&rkey=3",
        str(tmp_path), file_name="photo.jpg",
        allowed_hosts=media_dl.QQ_MEDIA_HOSTS, max_bytes=1024)
    assert path.startswith(str(tmp_path))
    with open(path, "rb") as f:
        assert f.read() == payload
    assert path.endswith("photo.jpg")  # hash 前缀 + 原文件名
    # 同内容重试: 复用已有落盘(不产生重复残留)。
    path2 = media_dl.download_url_bounded(
        "https://multimedia.nt.qq.com.cn/download?appid=1&fileid=2&rkey=4",
        str(tmp_path), file_name="photo.jpg",
        allowed_hosts=media_dl.QQ_MEDIA_HOSTS, max_bytes=1024)
    assert path2 == path


def test_download_url_bounded_streaming_size_cap_and_tmp_cleanup(monkeypatch, tmp_path):
    """无 Content-Length 时按流式累计上限中断, 且不留临时文件
    (2026-08-14 审查 I4: 流式落盘, 内存峰值=缓冲块)。"""
    class _Resp:
        headers = {}  # 无 Content-Length: 走累计上限

        def __enter__(self):
            return self

        def __exit__(self, *exc):
            return False

        def raise_for_status(self):
            pass

        def iter_content(self, chunk_size):
            yield b"a" * 512
            yield b"b" * 512
            yield b"c" * 512

    monkeypatch.setattr(media_dl.requests, "get", lambda url, **kw: _Resp())
    with pytest.raises(ValueError, match="exceeded"):
        media_dl.download_url_bounded(
            "https://multimedia.nt.qq.com.cn/download?appid=1&fileid=2&rkey=3",
            str(tmp_path), file_name="photo.jpg",
            allowed_hosts=media_dl.QQ_MEDIA_HOSTS, max_bytes=1024)
    # 失败路径无残留临时文件(原子性)。
    assert list(tmp_path.iterdir()) == []


def test_download_url_bounded_sniffs_ext_from_stream_head(monkeypatch, tmp_path):
    """魔数嗅探用流头部字节(落盘名在流结束后确定, 语义与 save 一致)。"""
    png = b"\x89PNG\r\n\x1a\n" + b"\x00" * 200

    class _Resp:
        headers = {"Content-Length": str(len(png))}

        def __enter__(self):
            return self

        def __exit__(self, *exc):
            return False

        def raise_for_status(self):
            pass

        def iter_content(self, chunk_size):
            yield png[:7]
            yield png[7:]

    monkeypatch.setattr(media_dl.requests, "get", lambda url, **kw: _Resp())
    path = media_dl.download_url_bounded(
        "https://multimedia.nt.qq.com.cn/download?appid=1&fileid=2&rkey=3",
        str(tmp_path), file_name="img_v2_key",
        allowed_hosts=media_dl.QQ_MEDIA_HOSTS)
    assert path.endswith(".png")  # 跨 chunk 边界嗅探(前 16 字节累积)


def test_save_bytes_bounded_sniffs_extension(tmp_path):
    png = b"\x89PNG\r\n\x1a\n" + b"\x00" * 16
    path = media_dl.save_bytes_bounded(png, str(tmp_path), file_name="img_v2_key")
    assert path.endswith(".png")  # 魔数嗅探扩展名(GA 注入层按扩展名判断)
    assert media_dl.build_media_item(path, str(tmp_path), "img_v2_key")["content_type"] == "image/png"
    with pytest.raises(ValueError, match="exceeded"):
        media_dl.save_bytes_bounded(b"x" * 10, str(tmp_path), max_bytes=5)


def test_build_media_item_storage_path_relative_to_root(tmp_path):
    path = media_dl.save_bytes_bounded(b"abc", str(tmp_path), file_name="a.txt")
    item = media_dl.build_media_item(path, str(tmp_path), "a.txt")
    # 落盘直接在 media_root 下: storage_path 即 basename; 前斜杠/反斜杠
    # 归一(Windows 兼容, 同 DB 行可跨挂载移植)。
    assert item["storage_path"] == item["storage_path"].replace("\\", "/")
    assert item["storage_path"].endswith("a.txt")
    assert item["size"] == 3
    assert item["content_type"] == "text/plain"


# ---------------------------------------------------------------------------
# QQ: attachments[].url 直下
# ---------------------------------------------------------------------------

class _QQAttachment:
    def __init__(self, url, filename, content_type="image/jpeg"):
        self.url = url
        self.filename = filename
        self.content_type = content_type


class _QQMsg:
    def __init__(self, mid, content, attachments=None, is_group=False):
        self.id = mid
        self.content = content
        self.attachments = attachments or []
        self.group_openid = "grp_1" if is_group else ""
        self.author = type("A", (), {"member_openid": "mem_1", "user_openid": "ou_1"})()


def _qq_adapter(tmp_path):
    return poller_server.QQAdapter(
        "qq-bot", {"app_id": "a", "app_secret": "s"}, "http://platform/webhook",
        media_root=str(tmp_path))


def test_qq_attachments_downloaded_into_webhook(tmp_path, monkeypatch):
    adapter = _qq_adapter(tmp_path)
    posted = []
    adapter.post_webhook = lambda body, max_attempts=None: posted.append(body) or True  # type: ignore[method-assign]

    def fake_download(url, dest_dir, **kw):
        p = media_dl.save_bytes_bounded(b"\xff\xd8\xff" + b"a" * 32, dest_dir,
                                        file_name=kw.get("file_name", ""))
        return p

    monkeypatch.setattr(poller_server.media_dl, "download_url_bounded", fake_download)
    adapter._handle_message(_QQMsg(
        "m1", "看看这个", [_QQAttachment("https://multimedia.nt.qq.com.cn/download?appid=1&rkey=x", "dog.jpg")]),
        is_group=False)
    assert len(posted) == 1
    body = posted[0]
    assert body["text"] == "看看这个"
    assert len(body["media_paths"]) == 1
    assert len(body["media_items"]) == 1
    assert body["media_items"][0]["file_name"] == "dog.jpg"
    assert body["media_items"][0]["content_type"] == "image/jpeg"


def test_qq_media_download_failure_keeps_text_and_drops_media(tmp_path, monkeypatch):
    adapter = _qq_adapter(tmp_path)
    posted = []
    adapter.post_webhook = lambda body, max_attempts=None: posted.append(body) or True  # type: ignore[method-assign]

    def failing_download(url, dest_dir, **kw):
        raise ValueError("host not allowed")

    monkeypatch.setattr(poller_server.media_dl, "download_url_bounded", failing_download)
    adapter._handle_message(_QQMsg(
        "m2", "文字还在", [_QQAttachment("https://multimedia.nt.qq.com.cn/x.jpg", "x.jpg")]),
        is_group=False)
    assert len(posted) == 1
    assert posted[0]["text"] == "文字还在"
    assert posted[0]["media_paths"] == []


def test_qq_image_only_download_failure_drops_message(tmp_path, monkeypatch):
    adapter = _qq_adapter(tmp_path)
    posted = []
    adapter.post_webhook = lambda body, max_attempts=None: posted.append(body) or True  # type: ignore[method-assign]

    def failing_download(url, dest_dir, **kw):
        raise ValueError("host not allowed")

    monkeypatch.setattr(poller_server.media_dl, "download_url_bounded", failing_download)
    # 无文本且媒体下载失败: 不投递(审查 B1, 不再回 "empty message ignored")。
    adapter._handle_message(_QQMsg("m3", "", [_QQAttachment("https://multimedia.nt.qq.com.cn/x.jpg", "x.jpg")]),
                            is_group=False)
    assert posted == []


# ---------------------------------------------------------------------------
# 飞书: image_key / file_key → resources API
# ---------------------------------------------------------------------------

class _FeishuSender:
    def __init__(self, open_id):
        self.sender_id = type("SID", (), {"open_id": open_id})()


class _FeishuMessage:
    def __init__(self, message_id, message_type, content):
        self.message_id = message_id
        self.message_type = message_type
        self.content = content
        self.chat_id = "oc_1"
        self.chat_type = "p2p"


class _FeishuData:
    def __init__(self, message):
        self.event = type("EV", (), {"message": message, "sender": _FeishuSender("ou_1")})()


def _feishu_adapter(tmp_path):
    return poller_server.FeishuAdapter(
        "fs-bot", {"app_id": "a", "app_secret": "s"}, "http://platform/webhook",
        media_root=str(tmp_path))


def test_feishu_image_message_downloaded_into_webhook(tmp_path, monkeypatch):
    adapter = _feishu_adapter(tmp_path)
    posted = []
    adapter.post_webhook = lambda body, max_attempts=None: posted.append(body) or True  # type: ignore[method-assign]
    png = b"\x89PNG\r\n\x1a\n" + b"\x00" * 32
    monkeypatch.setattr(adapter, "_download_resource", lambda mid, key, rtype: png)

    adapter._handle_feishu_message(_FeishuData(_FeishuMessage(
        "om_1", "image", json.dumps({"image_key": "img_v2_key"}))))
    assert len(posted) == 1
    body = posted[0]
    assert body["text"] == ""
    assert len(body["media_paths"]) == 1
    assert body["media_paths"][0].endswith(".png")  # 魔数嗅探扩展名
    assert body["media_items"][0]["content_type"] == "image/png"


def test_feishu_file_message_uses_file_key(tmp_path, monkeypatch):
    adapter = _feishu_adapter(tmp_path)
    posted = []
    adapter.post_webhook = lambda body, max_attempts=None: posted.append(body) or True  # type: ignore[method-assign]
    captured = {}

    def fake_download(mid, key, rtype):
        captured["mid"], captured["key"], captured["type"] = mid, key, rtype
        return b"pdf-bytes"

    monkeypatch.setattr(adapter, "_download_resource", fake_download)
    adapter._handle_feishu_message(_FeishuData(_FeishuMessage(
        "om_2", "file", json.dumps({"file_key": "file_v2_xyz", "file_name": "report.pdf"}))))
    assert captured == {"mid": "om_2", "key": "file_v2_xyz", "type": "file"}
    body = posted[0]
    assert body["media_items"][0]["file_name"] == "report.pdf"
    assert body["media_items"][0]["content_type"] == "application/pdf"


def test_feishu_image_download_failure_drops_message(tmp_path, monkeypatch):
    adapter = _feishu_adapter(tmp_path)
    posted = []
    adapter.post_webhook = lambda body, max_attempts=None: posted.append(body) or True  # type: ignore[method-assign]

    def failing(mid, key, rtype):
        raise RuntimeError("resource get failed: code=234037")

    monkeypatch.setattr(adapter, "_download_resource", failing)
    adapter._handle_feishu_message(_FeishuData(_FeishuMessage(
        "om_3", "image", json.dumps({"image_key": "img_v2_bad"}))))
    assert posted == []  # 无文本且下载失败: 不投递


# ---------------------------------------------------------------------------
# 钉钉: downloadCode → /v1.0/robot/messageFiles/download
# ---------------------------------------------------------------------------

class _DingTalkText:
    def __init__(self, content):
        self.content = content


class _DingTalkMsg:
    def __init__(self, raw):
        self._raw = raw
        self.text = _DingTalkText(raw.get("text", "")) if raw.get("text") else None
        self.sender_staff_id = raw.get("senderStaffId", "staff_1")
        self.conversation_id = raw.get("conversationId", "cid_1")
        self.conversation_type = raw.get("conversationType", "2")
        self.message_id = raw.get("msgId", "dm_1")


def _dingtalk_adapter(tmp_path):
    return poller_server.DingTalkAdapter(
        "dt-bot", {"app_id": "key", "app_secret": "sec"}, "http://platform/webhook",
        media_root=str(tmp_path))


def test_dingtalk_picture_download_code(tmp_path, monkeypatch):
    adapter = _dingtalk_adapter(tmp_path)
    posted = []
    adapter.post_webhook = lambda body, max_attempts=None: posted.append(body) or True  # type: ignore[method-assign]
    monkeypatch.setattr(adapter, "_access_token", lambda: "tok")

    class _Resp:
        status_code = 200
        content = b"\xff\xd8\xff" + b"z" * 64
        text = ""

    def fake_post(url, **kw):
        assert url.endswith("/v1.0/robot/messageFiles/download")
        assert kw["json"]["robotCode"] == "key"
        assert kw["json"]["downloadCode"] == "code_1"
        assert kw["json"]["openConversationId"] == "cid_1"
        return _Resp()

    monkeypatch.setattr(poller_server.requests, "post", fake_post)
    adapter._handle_chatbot_message(
        _DingTalkMsg({"msgtype": "picture", "content": json.dumps({"downloadCode": "code_1"})}),
        {"msgtype": "picture", "content": json.dumps({"downloadCode": "code_1"})})
    assert len(posted) == 1
    body = posted[0]
    assert len(body["media_paths"]) == 1
    assert body["media_items"][0]["content_type"] == "image/jpeg"


def test_dingtalk_audio_asr_text_injected(tmp_path, monkeypatch):
    adapter = _dingtalk_adapter(tmp_path)
    posted = []
    adapter.post_webhook = lambda body, max_attempts=None: posted.append(body) or True  # type: ignore[method-assign]
    monkeypatch.setattr(adapter, "_access_token", lambda: "tok")

    class _Resp:
        status_code = 200
        content = b"\x00" * 16
        text = ""

    monkeypatch.setattr(poller_server.requests, "post", lambda url, **kw: _Resp())
    adapter._handle_chatbot_message(
        _DingTalkMsg({"msgtype": "audio", "content": json.dumps(
            {"downloadCode": "code_a", "recognition": "钉钉，让进步发生"})}),
        {"msgtype": "audio", "content": json.dumps(
            {"downloadCode": "code_a", "recognition": "钉钉，让进步发生"})})
    assert len(posted) == 1
    assert "钉钉，让进步发生" in posted[0]["text"]  # 官方 ASR 文本注入(审查 S2)


def test_dingtalk_image_only_download_failure_drops(tmp_path, monkeypatch):
    adapter = _dingtalk_adapter(tmp_path)
    posted = []
    adapter.post_webhook = lambda body, max_attempts=None: posted.append(body) or True  # type: ignore[method-assign]
    monkeypatch.setattr(adapter, "_access_token", lambda: "")

    adapter._handle_chatbot_message(
        _DingTalkMsg({"msgtype": "picture", "content": json.dumps({"downloadCode": "code_1"})}),
        {"msgtype": "picture", "content": json.dumps({"downloadCode": "code_1"})})
    assert posted == []  # token 失败 → 媒体丢弃 + 无文本 → 不投递


# ---------------------------------------------------------------------------
# 企微: 直链 URL / media_id
# ---------------------------------------------------------------------------

def _wecom_frame(msgtype, media=None):
    return {"cmd": "message", "headers": {}, "body": {
        "msgid": "msg_1", "chatid": "chat_1", "msgtype": msgtype,
        "from": {"userid": "u1"}, msgtype: media or {},
    }}


def test_wecom_image_direct_url_download(tmp_path, monkeypatch):
    adapter = poller_server.WeComAdapter(
        "wc-bot", {"app_id": "bot1", "app_secret": "s"}, "http://platform/webhook",
        media_root=str(tmp_path))
    posted = []
    adapter.post_webhook = lambda body, max_attempts=None: posted.append(body) or True  # type: ignore[method-assign]

    def fake_download(url, dest_dir, **kw):
        return media_dl.save_bytes_bounded(b"\xff\xd8\xff" + b"b" * 32, dest_dir,
                                           file_name=kw.get("file_name", ""))

    monkeypatch.setattr(poller_server.media_dl, "download_url_bounded", fake_download)
    adapter._handle_wecom_message(_wecom_frame(
        "image", {"media_id": "mid_1", "image_url": "https://wework.qpic.cn/xx.jpg"}))
    assert len(posted) == 1
    body = posted[0]
    assert len(body["media_paths"]) == 1
    assert body["media_items"][0]["content_type"] == "image/jpeg"


def test_wecom_media_id_without_token_drops_media_keeps_none(tmp_path):
    adapter = poller_server.WeComAdapter(
        "wc-bot", {"app_id": "bot1", "app_secret": "s"}, "http://platform/webhook",
        media_root=str(tmp_path))
    posted = []
    adapter.post_webhook = lambda body, max_attempts=None: posted.append(body) or True  # type: ignore[method-assign]

    # 无直链 + 无 token(未接 client): 媒体下载不可用 → 无文本 → 不投递。
    adapter._handle_wecom_message(_wecom_frame("image", {"media_id": "mid_1"}))
    assert posted == []
