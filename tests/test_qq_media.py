"""qq_media(QQ 富媒体分片上传+直发)测试(2026-08-16 根路径媒体通道升级)。

覆盖: 官方 rich-media 4 步流程(单聊/群聊端点组)、分片偏移与 md5、
prepare 空响应失败、PUT 退避重试、频控超限、哈希参考值、
QQApp.send_done 集成(文本+媒体交付, 失败回退提示)。
"""

import asyncio
import hashlib
import os
import sys
from types import SimpleNamespace

import pytest

botpy = pytest.importorskip("botpy")  # noqa: F841  (缺 botpy 时跳过, 防 qqapp sys.exit)

sys.path.insert(0, os.path.join(os.path.dirname(os.path.dirname(os.path.abspath(__file__))), "frontends"))
import qq_media  # noqa: E402
from qq_media import QQMediaSender  # noqa: E402

_C2C_PREPARE = "/v2/users/{openid}/upload_prepare"
_C2C_FINISH = "/v2/users/{openid}/upload_part_finish"
_C2C_FILES = "/v2/users/{openid}/files"
_C2C_MSG = "/v2/users/{openid}/messages"
_GRP_PREPARE = "/v2/groups/{group_openid}/upload_prepare"
_GRP_MSG = "/v2/groups/{group_openid}/messages"

_5M = 5 * 1024 * 1024


class _Resp:
    def raise_for_status(self):
        pass


class _FakeHTTP:
    """botpy BotHttp.request 替身: route.path(模板)前缀路由 + 调用记录。"""

    def __init__(self, responses):
        self.responses = responses
        self.calls = []  # (route.path, json)

    async def request(self, route, **kwargs):
        self.calls.append((route.path, kwargs.get("json")))
        for prefix, resp in self.responses.items():
            if route.path.startswith(prefix):
                return resp
        raise AssertionError(f"unexpected route: {route.path}")


def _sender(http, monkeypatch):
    client = SimpleNamespace(api=SimpleNamespace(_http=http))
    puts = []

    def _put(url, data, timeout=300):
        puts.append((url, data))
        return _Resp()

    monkeypatch.setattr(qq_media.requests, "put", _put)
    return QQMediaSender(client), puts


def _prepare_resp(upload_id="U1", parts=None, block_size=_5M):
    return {"upload_id": upload_id, "block_size": str(block_size),
            "parts": parts if parts is not None else [{"index": 0, "presigned_url": "http://p.example/0"}]}


async def _run(coro):
    return await coro


def test_send_media_c2c_full_flow(tmp_path, monkeypatch):
    """单聊单分片全流程: prepare → PUT → part_finish → files → messages,
    msg_type=7 media.file_info, payload 与端点模板正确。"""
    f = tmp_path / "a.png"
    f.write_bytes(os.urandom(1024))
    http = _FakeHTTP({
        _C2C_PREPARE: _prepare_resp(),
        _C2C_FINISH: {"ok": True},
        _C2C_FILES: {"file_info": "FI-1"},
        _C2C_MSG: {"msg_id": "M1"},
    })
    sender, puts = _sender(http, monkeypatch)

    asyncio.run(_run(sender.send_media("openid-1", str(f), file_name="a.png", file_type=1, is_group=False)))

    paths = [c[0] for c in http.calls]
    assert paths == [_C2C_PREPARE, _C2C_FINISH, _C2C_FILES, _C2C_MSG]
    assert len(puts) == 1
    assert puts[0][0] == "http://p.example/0"
    assert puts[0][1] == f.read_bytes()
    prepare = http.calls[0][1]
    assert prepare["file_type"] == 1 and prepare["file_name"] == "a.png"
    assert prepare["file_size"] == str(f.stat().st_size)
    finish = http.calls[1][1]
    assert finish["upload_id"] == "U1" and finish["part_index"] == 0
    assert finish["md5"] == hashlib.md5(f.read_bytes()).hexdigest()
    files_json = http.calls[2][1]
    assert files_json["srv_send_msg"] is False and files_json["file_type"] == 1
    msg_json = http.calls[3][1]
    assert msg_json == {"msg_type": 7, "media": {"file_info": "FI-1"}}


def test_send_media_group_endpoints(tmp_path, monkeypatch):
    """群聊: 全部走 /v2/groups/... 组(上传与发送必须同组)。"""
    f = tmp_path / "g.png"
    f.write_bytes(b"x" * 100)
    http = _FakeHTTP({
        "/v2/groups/{group_openid}/upload_prepare": _prepare_resp(),
        "/v2/groups/{group_openid}/upload_part_finish": {"ok": True},
        "/v2/groups/{group_openid}/files": {"file_info": "FI-G"},
        _GRP_MSG: {"msg_id": "M2"},
    })
    sender, _ = _sender(http, monkeypatch)

    asyncio.run(_run(sender.send_media("g-openid", str(f), file_type=4, is_group=True)))

    assert [c[0] for c in http.calls] == [
        "/v2/groups/{group_openid}/upload_prepare",
        "/v2/groups/{group_openid}/upload_part_finish",
        "/v2/groups/{group_openid}/files",
        "/v2/groups/{group_openid}/messages",
    ]


def test_send_media_multipart_offsets(tmp_path, monkeypatch):
    """多分片: 偏移按 idx*block_size 递增, 每片 PUT 后必须 part_finish,
    各片 block_size/md5 正确(平台 I4 同款切片语义)。"""
    size = 6 * 1024 * 1024
    f = tmp_path / "big.bin"
    f.write_bytes(os.urandom(size))
    http = _FakeHTTP({
        _C2C_PREPARE: _prepare_resp(parts=[
            {"index": 0, "presigned_url": "http://p.example/0", "block_size": _5M},
            {"index": 1, "presigned_url": "http://p.example/1", "block_size": size - _5M},
        ]),
        _C2C_FINISH: {"ok": True},
        _C2C_FILES: {"file_info": "FI-M"},
        _C2C_MSG: {"msg_id": "M3"},
    })
    sender, puts = _sender(http, monkeypatch)

    asyncio.run(_run(sender.send_media("openid-2", str(f), file_type=4)))

    assert [u for u, _ in puts] == ["http://p.example/0", "http://p.example/1"]
    assert len(puts[0][1]) == _5M
    assert len(puts[1][1]) == size - _5M
    with open(f, "rb") as fh:
        assert puts[0][1] == fh.read(_5M)
        assert puts[1][1] == fh.read()
    finishes = [c for c in http.calls if c[0] == _C2C_FINISH]
    assert [c[1]["part_index"] for c in finishes] == [0, 1]
    assert [c[1]["block_size"] for c in finishes] == [str(_5M), str(size - _5M)]
    assert finishes[0][1]["md5"] == hashlib.md5(puts[0][1]).hexdigest()
    assert finishes[1][1]["md5"] == hashlib.md5(puts[1][1]).hexdigest()


def test_send_media_prepare_empty_raises(tmp_path, monkeypatch):
    """prepare 无 upload_id/parts → RuntimeError(不进入 PUT/发送)。"""
    f = tmp_path / "x.bin"
    f.write_bytes(b"x")
    http = _FakeHTTP({_C2C_PREPARE: {}})
    sender, puts = _sender(http, monkeypatch)

    with pytest.raises(RuntimeError, match="upload_prepare returned no"):
        asyncio.run(_run(sender.send_media("openid-3", str(f), file_type=4)))
    assert puts == []


def test_send_media_rate_limited_raises(tmp_path, monkeypatch):
    """主动消息频控超限 → RuntimeError(调用方回退文本, 内容不丢)。"""
    f = tmp_path / "r.bin"
    f.write_bytes(b"x")
    http = _FakeHTTP({
        _C2C_PREPARE: _prepare_resp(),
        _C2C_FINISH: {"ok": True},
        _C2C_FILES: {"file_info": "FI-R"},
        _C2C_MSG: {"msg_id": "M4"},
    })
    sender, _ = _sender(http, monkeypatch)
    monkeypatch.setattr(qq_media._TokenBucket, "acquire", lambda self, timeout=2.0: False)

    with pytest.raises(RuntimeError, match="rate limited"):
        asyncio.run(_run(sender.send_media("openid-4", str(f), file_type=4)))


def test_qq_request_rate_limit_response_raises(tmp_path, monkeypatch):
    """限流响应(官方 err_code 50002)必须 fail-closed 抛错, 不当作正常
    响应继续解析(平台 _is_qq_rate_limit 同构)。"""
    f = tmp_path / "rl.bin"
    f.write_bytes(b"x")
    http = _FakeHTTP({_C2C_PREPARE: {"err_code": 50002, "message": "rate limit exceeded"}})
    sender, _ = _sender(http, monkeypatch)

    with pytest.raises(RuntimeError, match="rate limited"):
        asyncio.run(_run(sender.send_media("openid-rl", str(f), file_type=4)))


def test_put_part_retries_then_raises(tmp_path, monkeypatch):
    """PUT 持续失败: 3 次退避重试后抛错(幂等重试, 不静默)。"""
    monkeypatch.setattr(qq_media.time, "sleep", lambda s: None)
    attempts = []
    import requests as _r

    def _bad_put(url, data, timeout=300):
        attempts.append(url)
        raise OSError("net down")

    monkeypatch.setattr(qq_media.requests, "put", _bad_put)
    sender = QQMediaSender(SimpleNamespace(api=SimpleNamespace(_http=None)))

    with pytest.raises(RuntimeError, match="part 0 failed after retries"):
        sender._put_part_with_retry("http://p.example/0", b"x", 0)
    assert len(attempts) == 3


def test_hashes_match_reference(tmp_path):
    """md5/sha1 与直接计算一致; 小文件 md5_10m 截断为全量(md5_10m 官方
    定义为文件前 10002432 字节)。"""
    data = bytes(range(256)) * 1000  # 256KB < 10002432
    f = tmp_path / "h.bin"
    f.write_bytes(data)
    md5, sha1, md5_10m = QQMediaSender._hashes(str(f))
    assert md5 == hashlib.md5(data).hexdigest()
    assert sha1 == hashlib.sha1(data).hexdigest()
    assert md5_10m == md5


def test_send_done_delivers_media_and_text(tmp_path, monkeypatch):
    """QQApp.send_done 集成: 文本照常发送, [FILE:] 产出按扩展名分类
    (txt → file_type=4)分片直发; 媒体失败回退提示不丢文本。"""
    import qqapp  # noqa: E402 (botpy 已 importorskip)

    f = tmp_path / "out.txt"
    f.write_bytes(b"hello qq")
    http = _FakeHTTP({
        _C2C_PREPARE: _prepare_resp(),
        _C2C_FINISH: {"ok": True},
        _C2C_FILES: {"file_info": "FI-D"},
        _C2C_MSG: {"msg_id": "M5"},
    })
    app = qqapp.QQApp()
    app.client = SimpleNamespace(api=SimpleNamespace(_http=http))
    sent = []

    async def fake_send_text(chat_id, content, **ctx):
        sent.append(content)

    app.send_text = fake_send_text
    monkeypatch.setattr(qq_media.requests, "put", lambda *a, **k: _Resp())

    asyncio.run(_run(app.send_done("u-1", f"任务完成\n[FILE:{f}]", is_group=False)))

    assert sent == ["任务完成"]
    assert http.calls[0][1]["file_type"] == 4
    assert http.calls[-1][1] == {"msg_type": 7, "media": {"file_info": "FI-D"}}
    assert len(http.calls) == 4


def test_send_done_reuses_media_sender(tmp_path, monkeypatch):
    """发送器按 app 生命周期单例(2026-08-16 复审): 每次 send_done 新建
    QQMediaSender 会重置令牌桶, 15 QPM 频控形同虚设——必须复用。"""
    import qqapp  # noqa: E402

    f = tmp_path / "s.txt"
    f.write_bytes(b"x")
    http = _FakeHTTP({
        _C2C_PREPARE: _prepare_resp(),
        _C2C_FINISH: {"ok": True},
        _C2C_FILES: {"file_info": "FI-S"},
        _C2C_MSG: {"msg_id": "M6"},
    })
    app = qqapp.QQApp()
    app.client = SimpleNamespace(api=SimpleNamespace(_http=http))

    async def fake_send_text(chat_id, content, **ctx):
        pass

    app.send_text = fake_send_text
    monkeypatch.setattr(qq_media.requests, "put", lambda *a, **k: _Resp())

    asyncio.run(_run(app.send_done("u-3", f"完成\n[FILE:{f}]", is_group=False)))
    assert app._media_sender is not None
    assert app._media() is app._media_sender  # 复用同一实例, 桶不重置


def test_send_done_media_failure_falls_back_to_text(tmp_path, monkeypatch):
    """媒体发送失败: 回退一句提示, 文本内容不丢(与 wxbot 失败回退同语义)。"""
    import qqapp  # noqa: E402

    f = tmp_path / "out.png"
    f.write_bytes(b"not really png")
    http = _FakeHTTP({_C2C_PREPARE: {}})  # prepare 空 → 上传失败
    app = qqapp.QQApp()
    app.client = SimpleNamespace(api=SimpleNamespace(_http=http))
    sent = []

    async def fake_send_text(chat_id, content, **ctx):
        sent.append(content)

    app.send_text = fake_send_text
    monkeypatch.setattr(qq_media.requests, "put", lambda *a, **k: _Resp())

    asyncio.run(_run(app.send_done("u-2", f"生图完成\n[FILE:{f}]", is_group=False)))

    assert sent[0] == "生图完成"
    assert len(sent) == 2 and "发送失败" in sent[1]
