"""Phase B 生图（image_gen）回归测试（2026-08-14 定稿）。

覆盖：请求形状（response_format/output_format 按 model 裁剪断言）、b64
落盘路径、FILE marker 格式、API 4xx/5xx 重试、空响应、流式中断降级、超限
>20MiB、未配置 image_gen、n=2 命名与多 marker。mock API（monkeypatch，
无真实密钥，CI 安全约束）。

设计真值：.tasks/im-media-pipeline/PHASE_B_IMAGE_GEN_PLAN.zh-CN.md §3.3/§6.5
+ 二轮审查 I-3（response_format 按 model）/I-4（错误前缀 [Error: image_gen]）。
"""

import base64
import json
import pathlib
import re

import pytest

import ga
import llmcore

_1PX_PNG = bytes.fromhex(
    "89504e470d0a1a0a0000000d4948445200000000010000000108060000001f15c489"
    "0000000d4944415478da63fcffff3f0300050001ff1aa1e66e0000000049454e44ae426082"
)

# 未建档模型（catalog 无此名）: 档案为"宽松集 + 未验证"，是**兜底自愈**类用例的被测通道。
# 已建档模型在构造期就把已知不支持的参数裁掉（见 TestChannelProfileCatalog），
# 因此那些用例必须改用未建档通道才能触发协商路径。
_UNLISTED_MODEL = "relay-unlisted-image-9"


class _FakeResponse:
    def __init__(self, status_code=200, headers=None, text="", _json=None, _lines=(), _chunks=()):
        self.status_code = status_code
        self.headers = headers or {}
        self.text = text
        self._json = _json
        self._lines = _lines
        self._chunks = _chunks
        self.closed = 0

    def __enter__(self):
        return self

    def __exit__(self, exc_type, exc, tb):
        self.close()
        return False

    def json(self):
        if self._json is None:
            raise ValueError("no json body")
        return self._json

    def iter_lines(self, decode_unicode=True):
        for ln in self._lines:
            yield ln

    def iter_content(self, chunk_size=1 << 16):
        for ch in self._chunks:
            yield ch

    def close(self):
        self.closed += 1


class _FakeHTTP:
    """按调用顺序返回预置响应；记录 (url, kwargs)。"""

    def __init__(self, responses):
        self.responses = list(responses)
        self.calls = []
        self.get_calls = []

    def post(self, url, *args, **kwargs):
        self.calls.append((url, kwargs))
        resp = self.responses.pop(0) if self.responses else _FakeResponse()
        return resp

    def get(self, url, *args, **kwargs):
        self.get_calls.append(url)
        resp = self.responses.pop(0) if self.responses else _FakeResponse()
        return resp


def _install_fake_http(monkeypatch, responses):
    fake = _FakeHTTP(responses)
    monkeypatch.setattr(llmcore, "_build_http_session", lambda: fake)
    return fake


def _client(cfg, fake):
    # _build_http_session 已被 monkeypatch, 客户端拿到的就是 fake
    return llmcore.OpenAIImageGenClient(cfg)


def _sync_response(b64_list):
    return _FakeResponse(_json={"data": [{"b64_json": b} for b in b64_list]})


def _drain(gen):
    try:
        while True:
            next(gen)
    except StopIteration as e:
        return e.value


def _handler(tmp_path):
    h = ga.GenericAgentHandler.__new__(ga.GenericAgentHandler)
    h.cwd = str(tmp_path)
    h.working = {}
    h.history_info = []
    h.current_turn = 1
    h.parent = None
    return h


# ─────────────────────────── 客户端：请求形状 ───────────────────────────

class TestClientRequestShape:
    CFG = {"name": "openai", "apibase": "https://api.openai.com/v1",
           "apikey": "sk-test", "model": "gpt-image-1", "max_retries": 0}

    def test_sync_payload_gpt_image_omits_default_output_format(self, monkeypatch):
        fake = _install_fake_http(monkeypatch, [_sync_response([base64.b64encode(_1PX_PNG).decode()])])
        client = _client(self.CFG, fake)
        images, err = client.generate("a cat", size="1024x1024", quality="high",
                                      output_format="png")
        assert err is None and images == [_1PX_PNG]
        url, kwargs = fake.calls[0]
        assert url == "https://api.openai.com/v1/images/generations"
        body = kwargs["json"]
        assert body["model"] == "gpt-image-1"
        assert body["prompt"] == "a cat"
        assert body["size"] == "1024x1024" and body["quality"] == "high"
        assert body["n"] == 1
        # png 是协议默认值 → 不发(显式发 png 只是多一个上游 400 借口)
        assert "output_format" not in body
        # 二轮审查 I-3: gpt-image 恒返回 b64_json, 不发 response_format(会 400)
        assert "response_format" not in body

    def test_sync_payload_sends_non_default_output_format(self, monkeypatch):
        fake = _install_fake_http(monkeypatch, [_sync_response([base64.b64encode(_1PX_PNG).decode()])])
        client = _client(self.CFG, fake)
        client.generate("a cat", output_format="webp")
        assert fake.calls[0][1]["json"]["output_format"] == "webp"

    def test_sync_payload_dalle_sends_response_format_and_trims_output_format(self, monkeypatch):
        fake = _install_fake_http(monkeypatch, [_sync_response([base64.b64encode(_1PX_PNG).decode()])])
        client = _client({**self.CFG, "model": "dall-e-3"}, fake)
        images, err = client.generate("a dog", output_format="png", size="1024x1024")
        assert err is None and images == [_1PX_PNG]
        body = fake.calls[0][1]["json"]
        assert body["response_format"] == "b64_json"
        # dall-e-3 无 output_format 概念: 裁剪, 否则上游 400
        assert "output_format" not in body

    def test_model_override_in_payload(self, monkeypatch):
        fake = _install_fake_http(monkeypatch, [_sync_response([base64.b64encode(_1PX_PNG).decode()])])
        client = _client(self.CFG, fake)
        client.generate("x", model="dall-e-2")
        assert fake.calls[0][1]["json"]["model"] == "dall-e-2"

    def test_defaults_n_and_omits_optionals(self, monkeypatch):
        fake = _install_fake_http(monkeypatch, [_sync_response([base64.b64encode(_1PX_PNG).decode()])])
        client = _client(self.CFG, fake)
        client.generate("x")
        body = fake.calls[0][1]["json"]
        assert body["n"] == 1
        # new-api 类中转实测(2026-08-14): size 必传(计费), 缺省默认 1024x1024
        assert body["size"] == "1024x1024"
        for k in ("quality", "output_format", "response_format", "stream"):
            assert k not in body


# ─────────────────────────── 客户端：失败语义 ───────────────────────────

class TestClientFailureSemantics:
    CFG = {"name": "openai", "apibase": "https://api.openai.com/v1",
           "apikey": "sk-test", "model": "gpt-image-1"}

    def test_4xx_immediate_error_no_marker_text(self, monkeypatch):
        resp = _FakeResponse(status_code=400, text="bad request")
        fake = _install_fake_http(monkeypatch, [resp])
        client = _client({**self.CFG, "max_retries": 2}, fake)
        images, err = client.generate("x")
        assert images is None
        assert err.startswith("[Error: image_gen HTTP 400")
        assert "!!!Error:" not in err
        assert len(fake.calls) == 1  # 400 非重试集合

    def test_5xx_retries_then_errors(self, monkeypatch):
        resp = _FakeResponse(status_code=503, headers={"retry-after": "0.5"})
        fake = _install_fake_http(monkeypatch, [resp, resp])
        client = _client({**self.CFG, "max_retries": 1}, fake)
        images, err = client.generate("x")
        assert images is None
        assert err.startswith("[Error: image_gen HTTP 503")
        assert len(fake.calls) == 2

    def test_429_retry_after_over_cap_no_retry(self, monkeypatch):
        resp = _FakeResponse(status_code=429, headers={"retry-after": "9999"})
        fake = _install_fake_http(monkeypatch, [resp])
        client = _client({**self.CFG, "max_retries": 2}, fake)
        images, err = client.generate("x")
        assert images is None
        assert "retry-after > cap" in err
        assert len(fake.calls) == 1

    def test_empty_data_returns_error(self, monkeypatch):
        fake = _install_fake_http(monkeypatch, [_sync_response([])])
        client = _client(self.CFG, fake)
        images, err = client.generate("x")
        assert images is None
        assert "空响应" in err

    def test_missing_b64_json_returns_error(self, monkeypatch):
        fake = _install_fake_http(monkeypatch, [_FakeResponse(_json={"data": [{}]})])
        client = _client(self.CFG, fake)
        images, err = client.generate("x")
        assert images is None
        assert "b64_json 与 url" in err

    def test_url_only_response_downloads_fallback(self, monkeypatch):
        # 实测(2026-08-14): sensenova/agnes 只返回 url 直链 → 直下兜底
        img_url = "https://cdn.example.test/img/abc.png"
        ok = _FakeResponse(_json={"data": [{"url": img_url}]})
        dl = _FakeResponse(_json=None, _chunks=[_1PX_PNG])
        fake = _install_fake_http(monkeypatch, [ok, dl])
        client = _client(self.CFG, fake)
        images, err = client.generate("x")
        assert err is None and images == [_1PX_PNG]
        assert fake.get_calls == [img_url]

    def test_url_download_http_error(self, monkeypatch):
        ok = _FakeResponse(_json={"data": [{"url": "https://cdn.example.test/img/abc.png"}]})
        dl = _FakeResponse(status_code=403)
        fake = _install_fake_http(monkeypatch, [ok, dl])
        client = _client(self.CFG, fake)
        images, err = client.generate("x")
        assert images is None
        assert "直链下载失败 HTTP 403" in err

    def test_url_download_over_20mib_rejected(self, monkeypatch):
        ok = _FakeResponse(_json={"data": [{"url": "https://cdn.example.test/img/big.png"}]})
        big = b"x" * (20 * 1024 * 1024 + 1)
        dl = _FakeResponse(_json=None, _chunks=[big])
        fake = _install_fake_http(monkeypatch, [ok, dl])
        client = _client(self.CFG, fake)
        images, err = client.generate("x")
        assert images is None
        assert "20MiB" in err

    def test_connection_error_returns_error_text(self, monkeypatch):
        class _ConnFail:
            def post(self, *a, **k):
                raise llmcore.requests.ConnectionError("boom")
        monkeypatch.setattr(llmcore, "_build_http_session", lambda: _ConnFail())
        client = llmcore.OpenAIImageGenClient({**self.CFG, "max_retries": 0})
        images, err = client.generate("x")
        assert images is None
        assert err.startswith("[Error: image_gen ConnectionError")


# ─────────────────────────── 客户端：流式路径 ───────────────────────────

class TestClientStreaming:
    CFG = {"name": "openai", "apibase": "https://api.openai.com/v1",
           "apikey": "sk-test", "model": "gpt-image-1", "stream": True, "max_retries": 0}
    B64 = base64.b64encode(_1PX_PNG).decode()

    def test_stream_uses_partial_images_and_takes_final_frame(self, monkeypatch):
        resp = _FakeResponse(headers={"content-type": "text/event-stream"}, _lines=[
            'data: {"data": [{}]}',
            f'data: {{"data": [{{"b64_json": "{self.B64}"}}]}}',
            "data: [DONE]",
        ])
        fake = _install_fake_http(monkeypatch, [resp])
        client = _client(self.CFG, fake)
        images, err = client.generate("a cat")
        assert err is None and images == [_1PX_PNG]
        assert len(fake.calls) == 1
        assert fake.calls[0][1]["json"]["stream"] is True
        assert fake.calls[0][1]["json"]["partial_images"] == 0

    def test_stream_interrupted_falls_back_to_sync_once(self, monkeypatch):
        # SSE 无最终帧 → 自动降级同步路径一次(§6.5)
        bad = _FakeResponse(headers={"content-type": "text/event-stream"}, _lines=[
            'data: {"data": [{}]}', "data: [DONE]",
        ])
        ok = _sync_response([self.B64])
        fake = _install_fake_http(monkeypatch, [bad, ok])
        client = _client(self.CFG, fake)
        images, err = client.generate("a cat")
        assert err is None and images == [_1PX_PNG]
        assert len(fake.calls) == 2

    def test_stream_error_event_falls_back_to_sync(self, monkeypatch):
        bad = _FakeResponse(headers={"content-type": "text/event-stream"}, _lines=[
            'data: {"error": {"message": "upstream refused"}}',
        ])
        ok = _sync_response([self.B64])
        fake = _install_fake_http(monkeypatch, [bad, ok])
        client = _client(self.CFG, fake)
        images, err = client.generate("a cat")
        assert err is None and images == [_1PX_PNG]
        assert len(fake.calls) == 2

    def test_stream_fallback_failure_returns_error(self, monkeypatch):
        bad = _FakeResponse(headers={"content-type": "text/event-stream"}, _lines=[
            'data: {"data": [{}]}', "data: [DONE]",
        ])
        err_resp = _FakeResponse(status_code=500, headers={"retry-after": "0.5"})
        fake = _install_fake_http(monkeypatch, [bad, err_resp, err_resp])
        client = _client({**self.CFG, "max_retries": 1}, fake)
        images, err = client.generate("x")
        assert images is None
        assert err.startswith("[Error: image_gen HTTP 500")

    def test_gateway_folds_sse_to_json_parsed_directly(self, monkeypatch):
        # 中转网关把 SSE 折叠回普通 JSON: 直接解析, 不二次请求重复计费
        resp = _FakeResponse(headers={"content-type": "application/json"},
                             _json={"data": [{"b64_json": self.B64}]})
        fake = _install_fake_http(monkeypatch, [resp])
        client = _client(self.CFG, fake)
        images, err = client.generate("a cat")
        assert err is None and images == [_1PX_PNG]
        assert len(fake.calls) == 1

    def test_stream_n_gt_1_goes_sync_directly(self, monkeypatch):
        # 流式只取单最终帧: n>1 直接走同步路径
        fake = _install_fake_http(monkeypatch, [_sync_response([self.B64, self.B64])])
        client = _client(self.CFG, fake)
        images, err = client.generate("x", n=2)
        assert err is None and len(images) == 2
        body = fake.calls[0][1]["json"]
        assert "stream" not in body and body["n"] == 2

    def test_dalle_with_stream_true_goes_sync_directly(self, monkeypatch):
        # 流式仅 gpt-image 系列: dall-e 不支持 stream/partial_images, 恒走同步
        fake = _install_fake_http(monkeypatch, [_sync_response([self.B64])])
        client = _client({**self.CFG, "model": "dall-e-3"}, fake)
        images, err = client.generate("x")
        assert err is None and images == [_1PX_PNG]
        assert len(fake.calls) == 1
        body = fake.calls[0][1]["json"]
        assert "stream" not in body and body["response_format"] == "b64_json"


# ─────────────────────────── resolve_image_gen ───────────────────────────

class TestResolveImageGen:
    def test_unconfigured_raises_value_error(self, monkeypatch):
        monkeypatch.setattr(llmcore, "reload_mykeys", lambda: ({}, True))
        with pytest.raises(ValueError):
            llmcore.resolve_image_gen("image_gen")

    def test_dispatch_openai_and_unknown(self, monkeypatch):
        monkeypatch.setattr(llmcore, "reload_mykeys",
                            lambda: ({"image_gen": {"name": "openai", "apibase": "http://x/v1",
                                                    "apikey": "k", "model": "gpt-image-1"}}, True))
        client = llmcore.resolve_image_gen("image_gen")
        assert isinstance(client, llmcore.OpenAIImageGenClient)
        monkeypatch.setattr(llmcore, "reload_mykeys",
                            lambda: ({"image_gen": {"name": "fal"}}, True))
        with pytest.raises(ValueError):
            llmcore.resolve_image_gen("image_gen")


# ─────────────────────────── do_image_gen 工具 ───────────────────────────

class TestDoImageGen:
    CFG = {"name": "openai", "apibase": "https://api.openai.com/v1",
           "apikey": "sk-test", "model": "gpt-image-1", "max_retries": 0}
    B64 = base64.b64encode(_1PX_PNG).decode()

    def _client_with(self, monkeypatch, responses):
        fake = _install_fake_http(monkeypatch, responses)
        return llmcore.OpenAIImageGenClient(self.CFG)

    def test_success_writes_file_and_returns_marker(self, monkeypatch, tmp_path):
        h = _handler(tmp_path)
        monkeypatch.setattr(ga, "resolve_image_gen",
                            lambda name, operation='generate': self._client_with(monkeypatch, [_sync_response([self.B64])]))
        outcome = _drain(h.do_image_gen({"prompt": "a cat"}, None))
        assert re.match(r"^\[FILE:outputs/image_\d{8}_\d{6}_\d{6}\.png\]$", outcome.data)
        files = list((tmp_path / "outputs").glob("image_*.png"))
        assert len(files) == 1
        assert files[0].read_bytes() == _1PX_PNG
        # marker 回显提示注入 next_prompt(I-2)
        assert "[FILE:outputs/" in outcome.next_prompt

    def test_n2_names_and_two_markers(self, monkeypatch, tmp_path):
        h = _handler(tmp_path)
        monkeypatch.setattr(ga, "resolve_image_gen",
                            lambda name, operation='generate': self._client_with(monkeypatch, [_sync_response([self.B64, self.B64])]))
        outcome = _drain(h.do_image_gen({"prompt": "a cat", "n": 2}, None))
        markers = outcome.data.split("\n")
        assert len(markers) == 2
        m1, m2 = markers
        assert re.match(r"^\[FILE:outputs/image_\d{8}_\d{6}_\d{6}_1\.png\]$", m1)
        assert re.match(r"^\[FILE:outputs/image_\d{8}_\d{6}_\d{6}_2\.png\]$", m2)
        assert m1 != m2
        files = sorted(p.name for p in (tmp_path / "outputs").glob("image_*.png"))
        assert len(files) == 2 and files[0].endswith("_1.png") and files[1].endswith("_2.png")

    def test_missing_prompt_errors(self, tmp_path):
        h = _handler(tmp_path)
        outcome = _drain(h.do_image_gen({}, None))
        assert "prompt" in outcome.data and outcome.data.startswith("[Error: image_gen")

    def test_unconfigured_returns_error_with_no_retry_hint(self, monkeypatch, tmp_path):
        h = _handler(tmp_path)

        def _raise_unconfigured(name, operation='generate'):
            raise ValueError("Config 'image_gen' not in mykey")

        monkeypatch.setattr(ga, "resolve_image_gen", _raise_unconfigured)
        outcome = _drain(h.do_image_gen({"prompt": "a cat"}, None))
        assert outcome.data.startswith("[Error: image_gen 未配置")
        assert "不要重试" in outcome.data
        assert "[FILE:" not in outcome.data

    def test_empty_response_no_marker(self, monkeypatch, tmp_path):
        h = _handler(tmp_path)
        monkeypatch.setattr(ga, "resolve_image_gen",
                            lambda name, operation='generate': self._client_with(monkeypatch, [_sync_response([])]))
        outcome = _drain(h.do_image_gen({"prompt": "a cat"}, None))
        assert "空响应" in outcome.data
        assert "[FILE:" not in outcome.data
        assert not (tmp_path / "outputs").exists()

    def test_api_500_returns_error_text(self, monkeypatch, tmp_path):
        h = _handler(tmp_path)
        resp = _FakeResponse(status_code=500, headers={"retry-after": "0.5"})
        fake = _install_fake_http(monkeypatch, [resp, resp])
        monkeypatch.setattr(ga, "resolve_image_gen",
                            lambda name, operation='generate': llmcore.OpenAIImageGenClient({**self.CFG, "max_retries": 1}))
        outcome = _drain(h.do_image_gen({"prompt": "a cat"}, None))
        assert outcome.data.startswith("[Error: image_gen HTTP 500")
        assert "!!!Error:" not in outcome.data
        assert not (tmp_path / "outputs").exists()

    def test_over_20mib_rejected_before_write(self, monkeypatch, tmp_path):
        h = _handler(tmp_path)
        big = b"x" * (20 * 1024 * 1024 + 1)
        monkeypatch.setattr(ga, "resolve_image_gen",
                            lambda name, operation='generate': self._client_with(monkeypatch, [_sync_response([base64.b64encode(big).decode()])]))
        outcome = _drain(h.do_image_gen({"prompt": "a cat"}, None))
        assert outcome.data.startswith("[Error: image_gen 产物")
        assert "20MiB" in outcome.data
        assert "[FILE:" not in outcome.data
        assert not list((tmp_path / "outputs").glob("image_*"))

    def test_output_format_follows_returned_bytes(self, monkeypatch, tmp_path):
        # 2026-09-13: agnes 系队列拒绝 output_format(客户端已裁剪), 返回字节可能是
        # 请求之外的容器格式 → 交付扩展名必须以真实魔数为准, 否则 IM 侧 MIME 失配。
        h = _handler(tmp_path)
        monkeypatch.setattr(ga, "resolve_image_gen",
                            lambda name, operation='generate': self._client_with(monkeypatch, [_sync_response([self.B64])]))
        outcome = _drain(h.do_image_gen({"prompt": "a cat", "output_format": "jpeg"}, None))
        lines = outcome.data.strip().split("\n")
        assert re.match(r"^\[FILE:outputs/image_\d{8}_\d{6}_\d{6}\.png\]$", lines[-1])
        # 工具结果里如实告知（不只显示流）: 上游忽略了请求的格式
        assert "上游返回格式与请求不符" in outcome.data
        assert list((tmp_path / "outputs").glob("image_*.png"))
        assert not list((tmp_path / "outputs").glob("image_*.jpeg"))

    def test_output_format_kept_when_magic_unknown(self, monkeypatch, tmp_path):
        # 魔数嗅探失败(非标准容器)才回退到请求的扩展名
        h = _handler(tmp_path)
        blob = b"\x00" * 64
        monkeypatch.setattr(ga, "resolve_image_gen",
                            lambda name, operation='generate': self._client_with(
                                monkeypatch, [_sync_response([base64.b64encode(blob).decode()])]))
        outcome = _drain(h.do_image_gen({"prompt": "a cat", "output_format": "jpeg"}, None))
        assert re.match(r"^\[FILE:outputs/image_\d{8}_\d{6}_\d{6}\.jpeg\]$", outcome.data)
        assert list((tmp_path / "outputs").glob("image_*.jpeg"))


# ───────────── 客户端：参数协商自愈（2026-09-13 真实上游实测） ─────────────

class TestClientParamNegotiation:
    """上游按"队列"裁剪参数(实测 new-api 中转 → agnes 的 text image queue):
    收到 output_format/quality 直接 400 invalid_request，而 gpt-image-2 恰好接受。

    2026-09-14 档案化后，**已建档模型不会再把已知不支持的参数发出去**（构造期裁剪，
    见 TestChannelProfileCatalog），所以本类改测**未建档通道**的兜底自愈：
    上游拒绝时按错误文本裁剪重试，而不是按模型名黑名单。"""

    CFG = {"name": "openai", "apibase": "https://api.openai.com/v1",
           "apikey": "sk-test", "model": _UNLISTED_MODEL}
    B64 = base64.b64encode(_1PX_PNG).decode()

    @staticmethod
    def _err400(param):
        return _FakeResponse(status_code=400, text=json.dumps(
            {"error": {"message": f"{param} is not supported by text image queue",
                       "type": "invalid_request", "code": "invalid_request"}}))

    def test_unsupported_output_format_trimmed_and_retried(self, monkeypatch):
        fake = _install_fake_http(monkeypatch, [self._err400("output_format"), _sync_response([self.B64])])
        client = _client({**self.CFG, "max_retries": 0}, fake)
        images, err = client.generate("a cat", size="1024x1024", quality="high",
                                      output_format="webp")
        assert err is None and images == [_1PX_PNG]
        assert len(fake.calls) == 2
        first, second = fake.calls[0][1]["json"], fake.calls[1][1]["json"]
        assert first["output_format"] == "webp" and first["quality"] == "high"
        # 只裁被点名的参数，其余原样（不能连带把 quality 也丢掉）
        assert "output_format" not in second
        assert second["quality"] == "high" and second["size"] == "1024x1024"
        assert second["model"] == _UNLISTED_MODEL and second["prompt"] == "a cat"

    def test_quality_trimmed_then_success(self, monkeypatch):
        fake = _install_fake_http(monkeypatch, [self._err400("quality"), _sync_response([self.B64])])
        client = _client({**self.CFG, "max_retries": 0}, fake)
        images, err = client.generate("a cat", quality="high", output_format="webp")
        assert err is None and images == [_1PX_PNG]
        second = fake.calls[1][1]["json"]
        assert "quality" not in second and second["output_format"] == "webp"

    def test_two_params_trimmed_in_sequence(self, monkeypatch):
        fake = _install_fake_http(monkeypatch, [self._err400("output_format"),
                                                self._err400("quality"),
                                                _sync_response([self.B64])])
        client = _client({**self.CFG, "max_retries": 0}, fake)
        images, err = client.generate("a cat", quality="high", output_format="webp")
        assert err is None and images == [_1PX_PNG]
        assert len(fake.calls) == 3
        last = fake.calls[2][1]["json"]
        assert "output_format" not in last and "quality" not in last

    def test_trim_never_drops_size_or_n(self, monkeypatch):
        # size(计费必传/模型必需) 与 n(张数属用户契约) 在可裁剪清单之外：
        # 不静默缩水，如实报错让模型改参重试。
        resp = _FakeResponse(status_code=400, text=json.dumps({"error": {
            "message": "size is not supported by text image queue; n is not supported",
            "type": "invalid_request"}}))
        fake = _install_fake_http(monkeypatch, [resp])
        client = _client({**self.CFG, "max_retries": 0}, fake)
        images, err = client.generate("a cat", size="1024x1024", n=2)
        assert images is None and err.startswith("[Error: image_gen HTTP 400")
        assert len(fake.calls) == 1

    def test_unrelated_400_not_trimmed(self, monkeypatch):
        # 非"参数不被支持"话术族的 400 一律不裁剪（防误裁）
        resp = _FakeResponse(status_code=400, text="bad request: prompt too long")
        fake = _install_fake_http(monkeypatch, [resp])
        client = _client({**self.CFG, "max_retries": 0}, fake)
        images, err = client.generate("a cat", output_format="png")
        assert images is None and err.startswith("[Error: image_gen HTTP 400")
        assert len(fake.calls) == 1

    def test_trim_budget_bounded_and_error_is_upstream_text(self, monkeypatch):
        monkeypatch.setattr(llmcore, "_IMAGE_GEN_MAX_PARAM_TRIMS", 1)
        fake = _install_fake_http(monkeypatch, [self._err400("output_format"),
                                                self._err400("quality")])
        client = _client({**self.CFG, "max_retries": 0}, fake)
        images, err = client.generate("a cat", quality="high", output_format="webp")
        assert images is None
        assert len(fake.calls) == 2  # 预算 1 → 不无限协商
        # 如实返回上游 message（供模型自愈改参），不返回合成文本
        assert err.startswith("[Error: image_gen HTTP 400") and "quality is not supported" in err

    def test_stream_path_also_negotiates(self, monkeypatch):
        sse = _FakeResponse(headers={"content-type": "text/event-stream"}, _lines=[
            f'data: {{"data": [{{"b64_json": "{self.B64}"}}]}}',
            "data: [DONE]",
        ])
        fake = _install_fake_http(monkeypatch, [self._err400("output_format"), sse])
        client = _client({**self.CFG, "stream": True, "max_retries": 0}, fake)
        images, err = client.generate("a cat", output_format="webp")
        assert err is None and images == [_1PX_PNG]
        assert len(fake.calls) == 2
        assert "output_format" not in fake.calls[1][1]["json"]

    def test_4xx_stream_response_closed(self, monkeypatch):
        # stream=True 的 4xx 响应也要关闭（此前只关非流式，句柄泄漏）
        resp = _FakeResponse(status_code=500, text="boom")
        fake = _install_fake_http(monkeypatch, [resp])
        client = _client({**self.CFG, "stream": True, "max_retries": 0}, fake)
        out, err = client._post({"model": "m", "prompt": "p"}, stream=True)
        assert out is None and err.startswith("[Error: image_gen HTTP 500")
        assert resp.closed == 1


# ───────────────────────── 魔数嗅探（落盘扩展名） ─────────────────────────

class TestSniffImageFormat:

    def test_png(self):
        assert llmcore.sniff_image_format(_1PX_PNG) == "png"

    def test_jpeg(self):
        assert llmcore.sniff_image_format(b"\xff\xd8\xff\xe0" + b"\x00" * 16) == "jpeg"

    def test_webp(self):
        assert llmcore.sniff_image_format(b"RIFF\x00\x00\x00\x00WEBPVP8 ") == "webp"

    def test_gif(self):
        assert llmcore.sniff_image_format(b"GIF89a" + b"\x00" * 8) == "gif"

    def test_unknown_and_short(self):
        assert llmcore.sniff_image_format(b"\x00" * 64) is None
        assert llmcore.sniff_image_format(b"") is None
        assert llmcore.sniff_image_format(b"\x89PNG") is None  # 短于魔数窗口


class TestConservativeFallbackOnOpaque4xx:
    """llm-proxy 为安全边界默认清洗上游错误体(只回 {"code":"UPSTREAM_ERROR"}),
    实测后果: 协商拿不到参数名 → 模型只看到裸 400 → 盲重试 4 次后放弃。
    客户端因此补一层不依赖上游话术的兜底: 4xx 不可判读且仍带装饰性参数时,
    退回保守参数集重试**一次**(纯装饰参数, 不影响请求语义/交付契约)。"""

    CFG = {"name": "openai", "apibase": "https://api.openai.com/v1",
           "apikey": "sk-test", "model": _UNLISTED_MODEL, "max_retries": 0}
    B64 = base64.b64encode(_1PX_PNG).decode()
    _OPAQUE = json.dumps({"code": "UPSTREAM_ERROR", "message": "upstream request failed"})

    def test_opaque_4xx_falls_back_to_conservative_payload(self, monkeypatch):
        fake = _install_fake_http(monkeypatch, [
            _FakeResponse(status_code=400, text=self._OPAQUE),
            _sync_response([self.B64]),
        ])
        client = _client(self.CFG, fake)
        images, err = client.generate("a cat", size="1024x1024", quality="high",
                                      output_format="webp", n=1)
        assert err is None and images == [_1PX_PNG]
        assert len(fake.calls) == 2
        first, second = fake.calls[0][1]["json"], fake.calls[1][1]["json"]
        assert first["quality"] == "high" and first["output_format"] == "webp"
        # 保守集: 装饰性参数全丢, 但请求语义(必需参数)保持
        for k in llmcore._IMAGE_GEN_TRIMMABLE:
            assert k not in second
        assert second["model"] == _UNLISTED_MODEL and second["prompt"] == "a cat"
        assert second["size"] == "1024x1024" and second["n"] == 1

    def test_conservative_fallback_happens_only_once(self, monkeypatch):
        # 硬失败(如内容策略)不能被变成无限重试: 两次请求后如实报错
        fake = _install_fake_http(monkeypatch, [
            _FakeResponse(status_code=400, text=self._OPAQUE),
            _FakeResponse(status_code=400, text=self._OPAQUE),
        ])
        client = _client(self.CFG, fake)
        images, err = client.generate("a cat", quality="high", output_format="webp")
        assert images is None
        assert len(fake.calls) == 2
        assert err.startswith("[Error: image_gen HTTP 400")

    def test_no_conservative_retry_without_trimmable_params(self, monkeypatch):
        fake = _install_fake_http(monkeypatch, [_FakeResponse(status_code=400, text=self._OPAQUE)])
        client = _client(self.CFG, fake)
        images, err = client.generate("a cat")
        assert images is None and len(fake.calls) == 1

    def test_5xx_unaffected_by_conservative_fallback(self, monkeypatch):
        # 5xx 走既有重试语义(非参数协商状态码), 不触发保守重试
        resp = _FakeResponse(status_code=503, headers={"retry-after": "0.5"})
        fake = _install_fake_http(monkeypatch, [resp, resp])
        client = _client({**self.CFG, "max_retries": 1}, fake)
        images, err = client.generate("a cat", quality="high")
        assert images is None and err.startswith("[Error: image_gen HTTP 503")
        assert len(fake.calls) == 2  # max_retries=1, 未额外保守重试
        assert fake.calls[1][1]["json"]["quality"] == "high"


@pytest.fixture(autouse=True)
def _clear_image_gen_trim_memo():
    """参数协商记忆是模块级状态: 每个用例前后清空, 避免跨用例串味。"""
    llmcore._IMAGE_GEN_TRIM_MEMO.clear()
    yield
    llmcore._IMAGE_GEN_TRIM_MEMO.clear()


class TestTrimMemoRemovesRepeatedWastedCalls:
    """实测一次任务可调 20+ 次生图: 每次都"撞一次 400 再裁剪"= 白付 ~1.3s +
    一条上游 400(WARN 噪音/上游失败计数)。协商结果按 (apibase, model) 记在进程内,
    后续请求直接不发那些装饰性参数。"""

    CFG = {"name": "openai", "apibase": "https://relay.example/v1",
           "apikey": "sk-test", "model": _UNLISTED_MODEL, "max_retries": 0}
    B64 = base64.b64encode(_1PX_PNG).decode()

    @staticmethod
    def _err400(param):
        return _FakeResponse(status_code=400, text=json.dumps(
            {"error": {"message": f"{param} is not supported by text image queue"}}))

    def test_second_call_skips_rejected_param(self, monkeypatch):
        fake = _install_fake_http(monkeypatch, [
            self._err400("quality"), _sync_response([self.B64]),   # 第 1 次: 400 → 裁剪 → 成功
            _sync_response([self.B64]),                            # 第 2 次: 直接成功
        ])
        client = _client(self.CFG, fake)
        assert client.generate("a cat", quality="high")[0] == [_1PX_PNG]
        assert len(fake.calls) == 2
        assert "quality" in fake.calls[0][1]["json"]

        assert client.generate("a cat", quality="high")[0] == [_1PX_PNG]
        assert len(fake.calls) == 3                      # 只多一次请求, 不再有 400
        assert "quality" not in fake.calls[2][1]["json"]
        # 请求语义不受影响
        assert fake.calls[2][1]["json"]["prompt"] == "a cat"

    def test_memo_is_per_model(self, monkeypatch):
        # 同一网关下 gpt-image 系接受 quality: 记忆必须按 model 隔离
        fake = _install_fake_http(monkeypatch, [
            self._err400("quality"), _sync_response([self.B64]),
            _sync_response([self.B64]),
        ])
        client = _client(self.CFG, fake)
        client.generate("a cat", quality="high")
        client.generate("a cat", quality="high", model="gpt-image-2")
        assert fake.calls[2][1]["json"]["quality"] == "high"
        assert fake.calls[2][1]["json"]["model"] == "gpt-image-2"

    def test_memo_is_per_gateway(self, monkeypatch):
        fake = _install_fake_http(monkeypatch, [
            self._err400("quality"), _sync_response([self.B64]),
            _sync_response([self.B64]),
        ])
        _client(self.CFG, fake).generate("a cat", quality="high")
        other = _client({**self.CFG, "apibase": "https://other.example/v1"}, fake)
        other.generate("a cat", quality="high")
        assert fake.calls[2][1]["json"]["quality"] == "high"

    def test_conservative_fallback_is_also_memoized(self, monkeypatch):
        opaque = json.dumps({"code": "UPSTREAM_ERROR", "message": "upstream request failed"})
        fake = _install_fake_http(monkeypatch, [
            _FakeResponse(status_code=400, text=opaque), _sync_response([self.B64]),
            _sync_response([self.B64]),
        ])
        client = _client(self.CFG, fake)
        assert client.generate("a cat", quality="high", output_format="webp")[0] == [_1PX_PNG]
        assert len(fake.calls) == 2
        client.generate("a cat", quality="high", output_format="webp")
        assert len(fake.calls) == 3
        body = fake.calls[2][1]["json"]
        assert "quality" not in body and "output_format" not in body

    def test_memo_never_drops_required_params(self, monkeypatch):
        # 记忆只可能记住装饰性参数: 必需参数即便上游点名也不裁(no size/n)
        fake = _install_fake_http(monkeypatch, [
            _FakeResponse(status_code=400, text=json.dumps({"error": {"message": "size is not supported"}})),
        ])
        client = _client(self.CFG, fake)
        images, err = client.generate("a cat", size="1024x1024")
        assert images is None and len(fake.calls) == 1
        assert llmcore._IMAGE_GEN_TRIM_MEMO == {}        # 无记忆写入


# ═══════════ 改图/参考图(image.edit)：2026-09-13 实测新增 ═══════════

class TestEditProtocolAndOperationGate:
    """`protocol=images_edits` → multipart POST /images/edits(文件字段 image)；
    `operations` 声明本通道能做什么，未声明一律 fail-closed（防"静默丢弃参数"通道
    假装成功——实测 agnes 的 extra_body.image 就是静默无效）。"""

    CFG_EDIT = {"name": "openai", "apibase": "https://relay.example/v1", "apikey": "sk-test",
                "model": "gemini-3.1-flash-image", "protocol": "images_edits",
                "operations": ["edit"], "max_retries": 0}
    CFG_GEN = {"name": "openai", "apibase": "https://relay.example/v1", "apikey": "sk-test",
               "model": "agnes-image-2.5-flash", "max_retries": 0}
    B64 = base64.b64encode(_1PX_PNG).decode()

    def test_edit_uses_multipart_images_edits_endpoint(self, monkeypatch):
        fake = _install_fake_http(monkeypatch, [_sync_response([self.B64])])
        client = _client(self.CFG_EDIT, fake)
        images, err = client.generate("把方块改成绿色", size="1024x1024",
                                      images=[("ref.png", _1PX_PNG, "image/png")])
        assert err is None and images == [_1PX_PNG]
        url, kwargs = fake.calls[0]
        assert url.endswith("/images/edits")
        assert "json" not in kwargs                     # multipart, 不能走 json body
        assert "Content-Type" not in kwargs["headers"]  # boundary 由 requests 生成
        assert kwargs["files"] == [("image", ("ref.png", _1PX_PNG, "image/png"))]
        assert kwargs["data"]["model"] == "gemini-3.1-flash-image"
        assert kwargs["data"]["prompt"] == "把方块改成绿色"
        assert kwargs["data"]["size"] == "1024x1024"

    def test_generate_path_still_json_generations(self, monkeypatch):
        fake = _install_fake_http(monkeypatch, [_sync_response([self.B64])])
        client = _client(self.CFG_GEN, fake)
        assert client.generate("a cat")[0] == [_1PX_PNG]
        url, kwargs = fake.calls[0]
        assert url.endswith("/images/generations")
        assert kwargs["headers"]["Content-Type"] == "application/json"
        assert "files" not in kwargs

    def test_edit_without_declared_operation_fails_closed(self, monkeypatch):
        # 通道只声明 generate: 带参考图调用必须知情失败, 不许"悄悄按文生图出图"
        fake = _install_fake_http(monkeypatch, [_sync_response([self.B64])])
        client = _client(self.CFG_GEN, fake)
        images, err = client.generate("x", images=[("ref.png", _1PX_PNG, "image/png")])
        assert images is None and fake.calls == []
        assert "未声明" in err and "image.edit" in err and "不要重试" in err

    def test_edit_protocol_without_images_fails_closed(self, monkeypatch):
        # 反向: 只声明 edit 的通道收到纯文生图请求 → 也 fail-closed
        fake = _install_fake_http(monkeypatch, [_sync_response([self.B64])])
        client = _client(self.CFG_EDIT, fake)
        images, err = client.generate("x")
        assert images is None and fake.calls == []
        assert "未声明" in err and "image.generate" in err

    def test_edit_image_budget_and_multi_image_guard(self, monkeypatch):
        fake = _install_fake_http(monkeypatch, [_sync_response([self.B64])])
        client = _client(self.CFG_EDIT, fake)
        # 多图: new-api 源码只读 formData.Get("image")(单值) → 多图未实测, 明确拒绝
        images, err = client.generate("x", images=[("a.png", _1PX_PNG, "image/png"),
                                                   ("b.png", _1PX_PNG, "image/png")])
        assert images is None and "仅支持 1 张" in err and fake.calls == []
        # 单张超限
        big = b"\x89PNG\r\n\x1a\n" + b"x" * (client.MAX_EDIT_IMAGE_BYTES + 1)
        images, err = client.generate("x", images=[("big.png", big, "image/png")])
        assert images is None and "超过" in err and fake.calls == []
        # 空图
        images, err = client.generate("x", images=[("e.png", b"", "image/png")])
        assert images is None and "为空" in err

    def test_unions_declared_operations_allow_both(self, monkeypatch):
        # 一条通道声明两种 operation: 带图走 edits, 不带图走 generations
        fake = _install_fake_http(monkeypatch, [_sync_response([self.B64]), _sync_response([self.B64])])
        cfg = {**self.CFG_EDIT, "operations": ["generate", "edit"]}
        client = _client(cfg, fake)
        client.generate("x")
        client.generate("x", images=[("ref.png", _1PX_PNG, "image/png")])
        assert fake.calls[0][0].endswith("/images/generations")
        assert fake.calls[1][0].endswith("/images/edits")

    def test_bad_protocol_rejected(self):
        with pytest.raises(ValueError):
            llmcore.OpenAIImageGenClient({**self.CFG_EDIT, "protocol": "fal"})


class TestResolveImageGenByOperation:
    """operation='edit' 时优先 <name>_edit 配置(便宜/免费的文生图与付费改图分开配置)。"""

    def _mykeys(self, monkeypatch, mapping):
        monkeypatch.setattr(llmcore, "reload_mykeys", lambda: (mapping, False))

    def test_edit_prefers_sibling_config(self, monkeypatch):
        self._mykeys(monkeypatch, {
            "image_gen": {"name": "openai", "apibase": "https://a/v1", "apikey": "k", "model": "agnes-image-2.5-flash"},
            "image_edit": {"name": "openai", "apibase": "https://b/v1", "apikey": "k2",
                           "model": "gemini-3.1-flash-image", "protocol": "images_edits"},
        })
        assert llmcore.resolve_image_gen("image_gen").model == "agnes-image-2.5-flash"
        edit_client = llmcore.resolve_image_gen("image_gen", operation="edit")
        assert edit_client.model == "gemini-3.1-flash-image"
        assert edit_client.protocol == "images_edits"

    def test_edit_falls_back_to_base_config_then_fails_closed_at_call(self, monkeypatch):
        self._mykeys(monkeypatch, {
            "image_gen": {"name": "openai", "apibase": "https://a/v1", "apikey": "k", "model": "agnes-image-2.5-flash"},
        })
        client = llmcore.resolve_image_gen("image_gen", operation="edit")
        assert client.model == "agnes-image-2.5-flash"      # 找不到 image_edit → 用基础配置
        images, err = client.generate("x", images=[("r.png", _1PX_PNG, "image/png")])
        assert images is None and "未声明" in err            # 由 operation gate 兜住

    def test_missing_config_still_raises(self, monkeypatch):
        self._mykeys(monkeypatch, {})
        with pytest.raises(ValueError):
            llmcore.resolve_image_gen("image_gen", operation="edit")


class TestDoImageGenEditTool:
    """工具层负责参考图读盘/路径安全/格式校验(llmcore 只管发送)。"""

    CFG_EDIT = {"name": "openai", "apibase": "https://relay.example/v1", "apikey": "sk-test",
                "model": "gemini-3.1-flash-image", "protocol": "images_edits",
                "operations": ["edit"], "max_retries": 0}
    B64 = base64.b64encode(_1PX_PNG).decode()

    def _edit_client(self, monkeypatch, responses):
        fake = _install_fake_http(monkeypatch, responses)
        return fake, llmcore.OpenAIImageGenClient(self.CFG_EDIT)

    def test_reference_image_is_sent_and_output_delivered(self, monkeypatch, tmp_path):
        (tmp_path / "ref.png").write_bytes(_1PX_PNG)
        fake, client = self._edit_client(monkeypatch, [_sync_response([self.B64])])
        h = _handler(tmp_path)
        monkeypatch.setattr(ga, "resolve_image_gen", lambda name, operation='generate': client)
        outcome = _drain(h.do_image_gen({"prompt": "改成绿色", "image": "ref.png"}, None))
        assert re.match(r"^\[FILE:outputs/image_\d{8}_\d{6}_\d{6}\.png\]$", outcome.data)
        assert fake.calls[0][1]["files"][0][0] == "image"
        assert fake.calls[0][1]["files"][0][1][0] == "ref.png"
        assert fake.calls[0][0].endswith("/images/edits")

    def test_path_escape_rejected(self, monkeypatch, tmp_path):
        outside = tmp_path.parent / "secret.png"
        outside.write_bytes(_1PX_PNG)
        fake, client = self._edit_client(monkeypatch, [])
        h = _handler(tmp_path)
        monkeypatch.setattr(ga, "resolve_image_gen", lambda name, operation='generate': client)
        outcome = _drain(h.do_image_gen({"prompt": "x", "image": "../secret.png"}, None))
        assert outcome.data.startswith("[Error: image_gen 参考图必须位于工作区内")
        assert fake.calls == []

    def test_missing_and_non_image_reference_rejected(self, monkeypatch, tmp_path):
        (tmp_path / "notimg.png").write_bytes(b"definitely not an image............")
        fake, client = self._edit_client(monkeypatch, [])
        h = _handler(tmp_path)
        monkeypatch.setattr(ga, "resolve_image_gen", lambda name, operation='generate': client)
        missing = _drain(h.do_image_gen({"prompt": "x", "image": "nope.png"}, None))
        assert "参考图不存在" in missing.data
        bad = _drain(h.do_image_gen({"prompt": "x", "image": "notimg.png"}, None))
        assert "不是可识别的图片" in bad.data
        assert fake.calls == []

    def test_edit_config_missing_multi_reference_message(self, monkeypatch, tmp_path):
        (tmp_path / "a.png").write_bytes(_1PX_PNG)
        (tmp_path / "b.png").write_bytes(_1PX_PNG)
        fake, client = self._edit_client(monkeypatch, [])
        h = _handler(tmp_path)
        monkeypatch.setattr(ga, "resolve_image_gen", lambda name, operation='generate': client)
        outcome = _drain(h.do_image_gen({"prompt": "x", "image": "a.png,b.png"}, None))
        assert "仅支持 1 张" in outcome.data and fake.calls == []


class TestJsonEditProtocol:
    """sensenova 等上游的改图是 **JSON**: `images:[{image_url: <url|Data-URL>}]`，
    与 OpenAI 的 multipart 文件上传不同（发 multipart 会被上游回 invalid arguments）。
    官方依据：platform.sensenova.cn/docs → SenseNova U1.5 Lite → 图片编辑接口。"""

    CFG = {"name": "openai", "apibase": "https://relay.example/v1", "apikey": "sk-test",
           "model": "sensenova-u1.5-lite", "protocol": "images_edits_json",
           "operations": ["edit"], "extra_params": {"watermark": False}, "max_retries": 0}
    B64 = base64.b64encode(_1PX_PNG).decode()

    def test_json_edit_sends_images_data_url(self, monkeypatch):
        fake = _install_fake_http(monkeypatch, [_sync_response([self.B64])])
        client = _client(self.CFG, fake)
        images, err = client.generate("把方块改成绿色", size="2048x2048",
                                      images=[("ref.png", _1PX_PNG, "image/png")])
        assert err is None and images == [_1PX_PNG]
        url, kwargs = fake.calls[0]
        assert url.endswith("/images/edits")
        assert "files" not in kwargs                      # JSON 形态: 不走 multipart
        body = kwargs["json"]
        assert isinstance(body["images"], list) and len(body["images"]) == 1
        data_url = body["images"][0]["image_url"]
        assert data_url.startswith("data:image/png;base64,")
        assert base64.b64decode(data_url.split(",", 1)[1]) == _1PX_PNG
        assert body["model"] == "sensenova-u1.5-lite"
        assert body["prompt"] == "把方块改成绿色"
        assert body["size"] == "2048x2048"
        assert body["watermark"] is False                 # extra_params 透传

    def test_extra_params_cannot_override_semantics(self):
        for key in ("model", "prompt", "n", "images", "image"):
            with pytest.raises(ValueError):
                llmcore.OpenAIImageGenClient({**self.CFG, "extra_params": {key: "x"}})

    def test_extra_params_must_be_dict(self):
        with pytest.raises(ValueError):
            llmcore.OpenAIImageGenClient({**self.CFG, "extra_params": ["watermark"]})

    def test_json_edit_still_gated_by_operations(self, monkeypatch):
        fake = _install_fake_http(monkeypatch, [])
        client = _client({**self.CFG, "operations": ["generate"]}, fake)
        images, err = client.generate("x", images=[("r.png", _1PX_PNG, "image/png")])
        assert images is None and "未声明" in err and fake.calls == []

    def test_multipart_protocol_unchanged_by_json_support(self, monkeypatch):
        fake = _install_fake_http(monkeypatch, [_sync_response([self.B64])])
        client = _client({**self.CFG, "protocol": "images_edits"}, fake)
        client.generate("x", images=[("r.png", _1PX_PNG, "image/png")])
        assert "files" in fake.calls[0][1] and "json" not in fake.calls[0][1]

    def test_data_url_mime_propagates(self):
        assert llmcore._image_gen_data_url(b"abc", "image/webp").startswith("data:image/webp;base64,")


class TestToolSurfacesOmittedParams:
    """档案省下的装饰参数必须进**工具结果**（不只是显示流）——模型据此知道本次实际生效
    的参数集，不会以为 webp/quality 已经生效。"""

    B64 = base64.b64encode(_1PX_PNG).decode()

    def test_omitted_decorative_params_reported_to_model(self, monkeypatch, tmp_path):
        fake = _install_fake_http(monkeypatch, [_sync_response([self.B64])])
        client = llmcore.OpenAIImageGenClient({"name": "openai", "apibase": "https://relay.example/v1",
                                               "apikey": "sk-test", "model": "agnes-image-2.5-flash",
                                               "max_retries": 0})
        h = _handler(tmp_path)
        monkeypatch.setattr(ga, "resolve_image_gen", lambda name, operation='generate': client)
        outcome = _drain(h.do_image_gen({"prompt": "a cat", "quality": "high", "output_format": "webp"}, None))
        assert "[FILE:outputs/" in outcome.data          # 产物照常交付
        assert "已省略" in outcome.data and "quality" in outcome.data
        body = fake.calls[0][1]["json"]
        assert "quality" not in body and "output_format" not in body
        assert body["size"] == "1024x1024"


# ═══════════ 通道能力档案（2026-09-14，成熟产品形态；DESIGN §8） ═══════════

class TestChannelProfileCatalog:
    """能力是**数据**：catalog 逐模型声明"支持什么 / 实测被拒什么"，请求在构造期就
    裁干净，不再"发出去撞 400 再裁"。对齐成熟做法（pi 的 ImagesModel.api + registry、
    OpenRouter 的 supported_parameters、LiteLLM 的 get_supported_openai_params：不支持
    的参数默认报错，静默丢弃是显式 opt-in）。"""

    B64 = base64.b64encode(_1PX_PNG).decode()

    def _c(self, monkeypatch, responses, model, **extra):
        fake = _install_fake_http(monkeypatch, responses)
        cfg = {"name": "openai", "apibase": "https://relay.example/v1", "apikey": "sk-test",
               "model": model, "max_retries": 0, **extra}
        return fake, llmcore.OpenAIImageGenClient(cfg)

    def test_rejected_decorative_params_not_sent_and_reported(self, monkeypatch):
        # agnes text image queue 实测拒收 output_format/quality → 构造期就不发,
        # 且必须明示"省略了什么"(装饰参数可省, 但不能静默)
        fake, client = self._c(monkeypatch, [_sync_response([self.B64])], "agnes-image-2.5-flash")
        images, err = client.generate("a cat", size="1K", quality="high", output_format="webp")
        assert err is None and images == [_1PX_PNG]
        assert len(fake.calls) == 1
        body = fake.calls[0][1]["json"]
        assert "quality" not in body and "output_format" not in body
        assert body["size"] == "1K" and body["n"] == 1
        assert {p for p, _ in client.last_notices} == {"quality", "output_format"}

    def test_catalog_decides_api_and_edit_shape_without_config(self, monkeypatch):
        # 配置里没有任何 protocol/operations: 端点与改图形态由档案决定
        fake, client = self._c(monkeypatch, [_sync_response([self.B64])], "sensenova-u1.5-lite")
        images, err = client.generate("把方块改成绿色", images=[("ref.png", _1PX_PNG, "image/png")])
        assert err is None and images == [_1PX_PNG]
        url, kwargs = fake.calls[0]
        assert url.endswith("/images/edits") and "json" in kwargs and "files" not in kwargs
        assert kwargs["json"]["images"][0]["image_url"].startswith("data:image/png;base64,")
        assert kwargs["json"]["watermark"] is False      # 档案声明的 fixed/extra 参数

    def test_catalog_decides_edit_capability(self, monkeypatch):
        # agnes 档案只声明 generate → 带参考图必须 fail-closed(不许悄悄按文生图出图)
        fake, client = self._c(monkeypatch, [], "agnes-image-2.5-flash")
        images, err = client.generate("x", images=[("r.png", _1PX_PNG, "image/png")])
        assert images is None and fake.calls == []
        assert "未声明" in err and "image.edit" in err

    def test_semantic_limit_from_profile_fails_loud(self, monkeypatch):
        # n 是语义参数(张数属用户契约): 档案 max_n=1 时 n=2 必须如实拒绝, 不静默只出 1 张
        fake, client = self._c(monkeypatch, [], "sensenova-u1.5-lite")
        images, err = client.generate("x", n=2)
        assert images is None and fake.calls == []
        assert err.startswith("[Error: image_gen") and "n" in err and "1" in err

    def test_unlisted_model_permissive_but_declared_unverified(self, monkeypatch):
        # 未建档 ≠ 静默降级: 宽松集发送 + 自愈, 但档案被显式标为未验证(可被工具告知模型)
        fake, client = self._c(monkeypatch, [_sync_response([self.B64])], _UNLISTED_MODEL)
        assert client.profile.source == "unverified"
        client.generate("x", quality="high", output_format="webp")
        body = fake.calls[0][1]["json"]
        assert body["quality"] == "high" and body["output_format"] == "webp"

    def test_config_explicit_declaration_beats_catalog(self, monkeypatch):
        # 运营显式声明优先于内置档案(catalog 只是默认值)
        fake, client = self._c(monkeypatch, [], "agnes-image-2.5-flash",
                               protocol="images_edits", operations=["edit"])
        assert client.protocol == "images_edits"
        assert "edit" in client.operations
        assert client.generate("x")[1].startswith("[Error: image_gen")   # 只声明 edit → 文生图 fail-closed
        assert fake.calls == []

    def test_per_request_model_override_switches_profile(self, monkeypatch):
        # 模型决定能力: 请求里覆写 model 时档案随之切换
        fake, client = self._c(monkeypatch, [_sync_response([self.B64])], "agnes-image-2.5-flash")
        client.generate("x", quality="high", model="gpt-image-1")
        body = fake.calls[0][1]["json"]
        assert body["model"] == "gpt-image-1" and body["quality"] == "high"

    def test_size_form_is_per_channel_and_never_silently_replaced(self, monkeypatch):
        # 2026-09-14 实测：agnes 接受档位 1K；gemini/gpt-image/sensenova 只接受 WxH
        # （传档位被网关回「图片尺寸格式错误，应为 宽x高」且该错被标成 500）。
        # size 是语义参数 → 形态不符必须 fail-loud，不静默替成 1024x1024。
        fake, client = self._c(monkeypatch, [], "gemini-3.1-flash-image")
        images, err = client.generate("a cat", size="1K")
        assert images is None and fake.calls == []
        assert err.startswith("[Error: image_gen") and "WxH" in err and "1K" in err
        # 同一条通道的 WxH 请求正常发出
        fake2, client2 = self._c(monkeypatch, [_sync_response([self.B64])], "gemini-3.1-flash-image")
        assert client2.generate("a cat", size="1536x1024")[0] == [_1PX_PNG]
        assert fake2.calls[0][1]["json"]["size"] == "1536x1024"

    def test_reference_limit_is_per_channel(self, monkeypatch):
        # sensenova 档案 max_refs=5（实测网关回 invalid images, should contain between 1 and 5 items）
        fake, client = self._c(monkeypatch, [_sync_response([self.B64])], "sensenova-u1.5-lite")
        images, err = client.generate("합성", images=[("a.png", _1PX_PNG, "image/png"),
                                                     ("b.png", _1PX_PNG, "image/png")])
        assert err is None
        assert len(fake.calls[0][1]["json"]["images"]) == 2

    def test_describe_states_size_form(self, monkeypatch):
        prof = llmcore.resolve_image_profile("gemini-3.1-flash-image", {})
        assert "WxH" in prof.describe("gemini-3.1-flash-image", "relay", zh=True)
        prof2 = llmcore.resolve_image_profile("agnes-image-2.5-flash", {})
        assert "1K/2K" in prof2.describe("agnes-image-2.5-flash", "relay", zh=True)

    def test_learned_trim_logs_profile_proposal(self, monkeypatch, capsys):
        # 有界自愈学到的结论必须可固化成 catalog 补丁(不再只躲在进程内记忆里)
        err400 = _FakeResponse(status_code=400, text=json.dumps(
            {"error": {"message": "quality is not supported by this queue"}}))
        fake, client = self._c(monkeypatch, [err400, _sync_response([self.B64])], _UNLISTED_MODEL)
        images, err = client.generate("x", quality="high")
        assert err is None and len(fake.calls) == 2
        out = capsys.readouterr().out
        assert "profile-proposal" in out and "quality" in out


class TestCapabilityRendering:
    """工具能力描述由档案渲染（消除"schema 写死能力 + 代码改了两边不一致"）。"""

    def _mykeys(self, monkeypatch, mapping):
        monkeypatch.setattr(llmcore, "reload_mykeys", lambda: (mapping, False))

    def test_render_reports_ops_limits_and_cost(self, monkeypatch):
        self._mykeys(monkeypatch, {
            "image_gen": {"name": "openai", "apibase": "https://relay/v1", "apikey": "k",
                          "model": "sensenova-u1.5-lite"},
        })
        text = llmcore.render_image_gen_capabilities("zh")
        assert "sensenova-u1.5-lite" in text
        assert "改图" in text and "参考图" in text and "免费" in text

    def test_render_reports_unconfigured(self, monkeypatch):
        self._mykeys(monkeypatch, {})
        assert "未配置" in llmcore.render_image_gen_capabilities("zh")

    def test_render_flags_unlisted_model(self, monkeypatch):
        self._mykeys(monkeypatch, {
            "image_gen": {"name": "openai", "apibase": "https://relay/v1", "apikey": "k",
                          "model": _UNLISTED_MODEL},
        })
        assert "未建档" in llmcore.render_image_gen_capabilities("zh")

    def test_schema_static_text_uses_placeholder_and_has_no_capability_claim(self):
        for f in ("assets/tools_schema.json", "assets/tools_schema_cn.json"):
            raw = pathlib.Path(f).read_text(encoding="utf-8")
            assert "{{IMAGE_GEN_CAPABILITIES}}" in raw, f
            for stale in ("No reference-image", "不存在参考图", "Only ONE reference image"):
                assert stale not in raw, (f, stale)

    def test_injection_replaces_placeholder_only_in_image_gen(self):
        schema = [{"type": "function", "function": {"name": "image_gen",
                                                    "description": "A {{IMAGE_GEN_CAPABILITIES}} B"}},
                  {"type": "function", "function": {"name": "code_run", "description": "{{IMAGE_GEN_CAPABILITIES}}"}}]
        out = llmcore.inject_image_gen_capabilities(schema, "CAP")
        assert out[0]["function"]["description"] == "A CAP B"
        assert out[1]["function"]["description"] == "{{IMAGE_GEN_CAPABILITIES}}"   # 其它工具不动
