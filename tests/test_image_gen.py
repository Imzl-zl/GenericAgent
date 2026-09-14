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
import re

import pytest

import ga
import llmcore

_1PX_PNG = bytes.fromhex(
    "89504e470d0a1a0a0000000d4948445200000000010000000108060000001f15c489"
    "0000000d4944415478da63fcffff3f0300050001ff1aa1e66e0000000049454e44ae426082"
)


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
                            lambda name: self._client_with(monkeypatch, [_sync_response([self.B64])]))
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
                            lambda name: self._client_with(monkeypatch, [_sync_response([self.B64, self.B64])]))
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

        def _raise_unconfigured(name):
            raise ValueError("Config 'image_gen' not in mykey")

        monkeypatch.setattr(ga, "resolve_image_gen", _raise_unconfigured)
        outcome = _drain(h.do_image_gen({"prompt": "a cat"}, None))
        assert outcome.data.startswith("[Error: image_gen 未配置")
        assert "不要重试" in outcome.data
        assert "[FILE:" not in outcome.data

    def test_empty_response_no_marker(self, monkeypatch, tmp_path):
        h = _handler(tmp_path)
        monkeypatch.setattr(ga, "resolve_image_gen",
                            lambda name: self._client_with(monkeypatch, [_sync_response([])]))
        outcome = _drain(h.do_image_gen({"prompt": "a cat"}, None))
        assert "空响应" in outcome.data
        assert "[FILE:" not in outcome.data
        assert not (tmp_path / "outputs").exists()

    def test_api_500_returns_error_text(self, monkeypatch, tmp_path):
        h = _handler(tmp_path)
        resp = _FakeResponse(status_code=500, headers={"retry-after": "0.5"})
        fake = _install_fake_http(monkeypatch, [resp, resp])
        monkeypatch.setattr(ga, "resolve_image_gen",
                            lambda name: llmcore.OpenAIImageGenClient({**self.CFG, "max_retries": 1}))
        outcome = _drain(h.do_image_gen({"prompt": "a cat"}, None))
        assert outcome.data.startswith("[Error: image_gen HTTP 500")
        assert "!!!Error:" not in outcome.data
        assert not (tmp_path / "outputs").exists()

    def test_over_20mib_rejected_before_write(self, monkeypatch, tmp_path):
        h = _handler(tmp_path)
        big = b"x" * (20 * 1024 * 1024 + 1)
        monkeypatch.setattr(ga, "resolve_image_gen",
                            lambda name: self._client_with(monkeypatch, [_sync_response([base64.b64encode(big).decode()])]))
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
                            lambda name: self._client_with(monkeypatch, [_sync_response([self.B64])]))
        outcome = _drain(h.do_image_gen({"prompt": "a cat", "output_format": "jpeg"}, None))
        assert re.match(r"^\[FILE:outputs/image_\d{8}_\d{6}_\d{6}\.png\]$", outcome.data)
        assert list((tmp_path / "outputs").glob("image_*.png"))
        assert not list((tmp_path / "outputs").glob("image_*.jpeg"))

    def test_output_format_kept_when_magic_unknown(self, monkeypatch, tmp_path):
        # 魔数嗅探失败(非标准容器)才回退到请求的扩展名
        h = _handler(tmp_path)
        blob = b"\x00" * 64
        monkeypatch.setattr(ga, "resolve_image_gen",
                            lambda name: self._client_with(
                                monkeypatch, [_sync_response([base64.b64encode(blob).decode()])]))
        outcome = _drain(h.do_image_gen({"prompt": "a cat", "output_format": "jpeg"}, None))
        assert re.match(r"^\[FILE:outputs/image_\d{8}_\d{6}_\d{6}\.jpeg\]$", outcome.data)
        assert list((tmp_path / "outputs").glob("image_*.jpeg"))


# ───────────── 客户端：参数协商自愈（2026-09-13 真实上游实测） ─────────────

class TestClientParamNegotiation:
    """上游按"队列"裁剪参数(实测 new-api 中转 → agnes-image-2.5-flash 的 text
    image queue): 收到 output_format/quality 直接 400 invalid_request，而
    gpt-image-2 恰好接受 output_format。客户端按错误文本协商裁剪后重试，
    而不是按模型名黑名单（方案 §4 原则）。"""

    CFG = {"name": "openai", "apibase": "https://api.openai.com/v1",
           "apikey": "sk-test", "model": "agnes-image-2.5-flash"}
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
        assert second["model"] == "agnes-image-2.5-flash" and second["prompt"] == "a cat"

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
           "apikey": "sk-test", "model": "agnes-image-2.5-flash", "max_retries": 0}
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
        assert second["model"] == "agnes-image-2.5-flash" and second["prompt"] == "a cat"
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
           "apikey": "sk-test", "model": "agnes-image-2.5-flash", "max_retries": 0}
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
