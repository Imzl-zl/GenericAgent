from types import SimpleNamespace

import requests

import llmcore


class _FakeResponse:
    status_code = 200
    headers = {}
    text = ""

    def __enter__(self):
        return self

    def __exit__(self, exc_type, exc, tb):
        return False


class _FakeHTTP:
    def __init__(self):
        self.calls = []

    def post(self, *args, **kwargs):
        self.calls.append((args, kwargs))
        return _FakeResponse()


def _drain(gen):
    chunks = []
    try:
        while True:
            chunks.append(next(gen))
    except StopIteration as stop:
        return chunks, stop.value


def test_base_session_builds_pooled_http_session():
    sess = llmcore.BaseSession({
        "apikey": "k",
        "apibase": "https://api.example.test",
        "model": "test-model",
    })

    assert isinstance(sess.http, requests.Session)
    http_adapter = sess.http.get_adapter("https://")
    assert http_adapter._pool_connections == llmcore._HTTP_POOL_CONNECTIONS
    assert http_adapter._pool_maxsize == llmcore._HTTP_POOL_MAXSIZE


def test_stream_with_retry_uses_session_pool_not_module_requests(monkeypatch):
    fake_http = _FakeHTTP()
    sess = SimpleNamespace(
        http=fake_http,
        max_retries=0,
        stream=True,
        connect_timeout=3,
        read_timeout=5,
        proxies=None,
        verify=True,
    )

    def fail_requests_post(*_args, **_kwargs):
        raise AssertionError("requests.post should not be used once session pooling is enabled")

    monkeypatch.setattr(llmcore.requests, "post", fail_requests_post)

    def parse_fn(_resp):
        yield "chunk-1"
        yield "chunk-2"
        return [{"type": "text", "text": "done"}]

    chunks, result = _drain(llmcore._stream_with_retry(
        sess,
        "https://api.example.test/v1/messages",
        {"Authorization": "Bearer x"},
        {"hello": "world"},
        parse_fn,
    ))

    assert chunks == ["chunk-1", "chunk-2"]
    assert result == [{"type": "text", "text": "done"}]
    assert len(fake_http.calls) == 1
    args, kwargs = fake_http.calls[0]
    assert args[0] == "https://api.example.test/v1/messages"
    assert kwargs["timeout"] == (3, 5)
    assert kwargs["stream"] is True
