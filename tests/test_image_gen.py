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
        pass


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

    def test_sync_payload_gpt_image_no_response_format(self, monkeypatch):
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
        assert body["output_format"] == "png"
        # 二轮审查 I-3: gpt-image 恒返回 b64_json, 不发 response_format(会 400)
        assert "response_format" not in body

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

    def test_output_format_jpeg_extension(self, monkeypatch, tmp_path):
        h = _handler(tmp_path)
        monkeypatch.setattr(ga, "resolve_image_gen",
                            lambda name: self._client_with(monkeypatch, [_sync_response([self.B64])]))
        outcome = _drain(h.do_image_gen({"prompt": "a cat", "output_format": "jpeg"}, None))
        assert re.match(r"^\[FILE:outputs/image_\d{8}_\d{6}_\d{6}\.jpeg\]$", outcome.data)
        assert list((tmp_path / "outputs").glob("image_*.jpeg"))
