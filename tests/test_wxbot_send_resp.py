"""wxbot_client 出站消息业务级校验测试(2026-08-15 生产事故根因: iLink
sendmessage 业务失败返回 HTTP 200 + 非零错误码, 此前被静默吞掉 → Go
delivery acked 但用户收不到。修复 = 与钉钉 adapter/getupdates 同构的
errcode/ret fail-closed 检查)。"""

import os
import sys
from pathlib import Path

import pytest

sys.path.insert(0, os.path.join(os.path.dirname(os.path.dirname(os.path.abspath(__file__))), "frontends"))
import wxbot_client  # noqa: E402


def test_check_send_resp_accepts_success_shapes():
    c = wxbot_client.WxBotClient.__new__(wxbot_client.WxBotClient)
    # 成功形态: 无错误字段 / errcode=0 / ret=0 均通过。
    assert c._check_send_resp("sendmessage", {}) == {}
    assert c._check_send_resp("sendmessage", {"errcode": 0, "errmsg": ""})["errcode"] == 0
    assert c._check_send_resp("sendmessage", {"ret": 0})["ret"] == 0
    assert c._check_send_resp("sendmessage", {"errcode": 0, "ret": 0}) is not None


def test_check_send_resp_rejects_business_errors():
    c = wxbot_client.WxBotClient.__new__(wxbot_client.WxBotClient)
    with pytest.raises(RuntimeError, match="sendmessage errcode=400"):
        c._check_send_resp("sendmessage", {"errcode": 400, "errmsg": "no permission"})
    with pytest.raises(RuntimeError, match="getuploadurl errcode=-14"):
        c._check_send_resp("getuploadurl", {"errcode": -14})
    with pytest.raises(RuntimeError, match="sendmessage ret=5"):
        c._check_send_resp("sendmessage", {"ret": 5})


def test_send_text_raises_on_business_error(monkeypatch):
    c = wxbot_client.WxBotClient.__new__(wxbot_client.WxBotClient)
    c.token = "tok"
    c._api = "https://example.invalid"

    def _fake_post(ep, body, timeout=15):
        assert ep == "ilink/bot/sendmessage"
        return {"errcode": 400, "errmsg": "active message window closed"}

    monkeypatch.setattr(c, "_post", _fake_post)
    with pytest.raises(RuntimeError, match="active message window closed"):
        c.send_text("user@im.wechat", "hi", client_id="t1")


def test_send_media_raises_on_upload_url_error(monkeypatch, tmp_path: Path):
    c = wxbot_client.WxBotClient.__new__(wxbot_client.WxBotClient)
    c.token = "tok"
    c._api = "https://example.invalid"

    f = tmp_path / "a.txt"
    f.write_bytes(b"hello")

    def _fake_post(ep, body, timeout=15):
        if ep == "ilink/bot/getuploadurl":
            return {"errcode": 2002, "errmsg": "receiver not reachable"}
        raise AssertionError(f"unexpected ep {ep}")

    monkeypatch.setattr(c, "_post", _fake_post)
    with pytest.raises(RuntimeError, match="receiver not reachable"):
        c.send_file("user@im.wechat", str(f), file_name="a.txt", client_id="t2")


def test_send_text_strips_stale_token_and_retries_on_ret_minus2(monkeypatch):
    """事故根因修复: 带过期 context_token 的 sendmessage 被 iLink 拒
    (ret=-2)——先去 token 降级重试一次(tokenless 在会话活跃期可成功);
    两次都失败则抛错(不再静默假 ack)。"""
    c = wxbot_client.WxBotClient.__new__(wxbot_client.WxBotClient)
    c.token = "tok"
    c._api = "https://example.invalid"
    calls = []

    def _fake_post(ep, body, timeout=15):
        calls.append(body["msg"].get("context_token", "<absent>"))
        if len(calls) == 1:
            return {"ret": -2, "errmsg": ""}
        return {"ret": 0}

    monkeypatch.setattr(c, "_post", _fake_post)
    c.send_text("user@im.wechat", "hi", context_token="stale-token", client_id="t1")
    assert calls == ["stale-token", "<absent>"], f"must retry tokenless after ret=-2: {calls}"


def test_send_text_raises_when_tokenless_retry_also_fails(monkeypatch):
    c = wxbot_client.WxBotClient.__new__(wxbot_client.WxBotClient)
    c.token = "tok"
    c._api = "https://example.invalid"

    def _fake_post(ep, body, timeout=15):
        return {"ret": -2}

    monkeypatch.setattr(c, "_post", _fake_post)
    with pytest.raises(RuntimeError, match="sendmessage ret=-2"):
        c.send_text("user@im.wechat", "hi", context_token="stale", client_id="t2")


def test_send_text_no_token_failure_propagates(monkeypatch):
    """无 token 发送(ret=-2)不应无限重试——直接抛错显形。"""
    c = wxbot_client.WxBotClient.__new__(wxbot_client.WxBotClient)
    c.token = "tok"
    c._api = "https://example.invalid"
    calls = []

    def _fake_post(ep, body, timeout=15):
        calls.append(1)
        return {"ret": -2}

    monkeypatch.setattr(c, "_post", _fake_post)
    with pytest.raises(RuntimeError, match="sendmessage ret=-2"):
        c.send_text("user@im.wechat", "hi", client_id="t3")
    assert len(calls) == 1, "tokenless ret=-2 must not retry"
