from __future__ import annotations

import importlib.util
import os
import threading
import time
import subprocess
import sys
from pathlib import Path
from types import SimpleNamespace

import pytest

SMOKE_PATH = Path(__file__).with_name("foundation_smoke.py")


def _load_smoke():
    spec = importlib.util.spec_from_file_location(
        "foundation_smoke_under_test", SMOKE_PATH
    )
    assert spec is not None and spec.loader is not None
    module = importlib.util.module_from_spec(spec)
    sys.modules[spec.name] = module
    spec.loader.exec_module(module)
    return module


def test_child_environment_uses_allowlist_and_drops_parent_secrets(
    monkeypatch, tmp_path: Path
) -> None:
    smoke = _load_smoke()
    sentinels = {
        "PARENT_PASSWORD": "sentinel-password",
        "PARENT_KEY": "sentinel-key",
        "PARENT_TOKEN": "sentinel-token",
        "PARENT_SECRET": "sentinel-secret",
        "PARENT_CREDENTIAL": "sentinel-credential",
        "HTTP_PROXY": "http://sentinel-proxy.invalid",
        "HTTPS_PROXY": "http://sentinel-proxy.invalid",
        "COOKIE": "sentinel-cookie",
        "AUTHORIZATION": "sentinel-auth",
        "SESSION_ID": "sentinel-session",
    }
    for name, value in sentinels.items():
        monkeypatch.setenv(name, value)
    values = {
        "TEST_DATABASE_URL": "postgresql://test.invalid/db",
        "PLATFORM_DEV_USER_ID": "7",
        "LLM_PROXY_CAPABILITY_SIGNING_KEY": "signing-key-sentinel",
        "LLM_PROXY_ALLOWED_UPSTREAM_CIDRS": "127.0.0.0/8",
        "LLM_PROXY_ALLOW_HTTP_HOSTS": "127.0.0.1:1",
    }
    env = smoke._child_environment(
        values,
        tmp_path / "config",
        tmp_path / "runtime",
        tmp_path,
        tmp_path / "policy.json",
    )

    assert not sentinels.keys() & env.keys()
    assert not set(sentinels.values()) & set(env.values())
    explicit = {
        "DATABASE_URL",
        "PLATFORM_DEV_USER_ID",
        "BOT_TOKEN_KEY",
        "PLATFORM_DEV_USERNAME",
        "PLATFORM_DEV_TOKEN",
        "GA_CONFIG_ROOT",
        "GA_RUNTIME_DIR",
        "GA_LEGACY_ROOT",
        "GA_POLICY_FILE",
        "GA_WORKER_PYTHON",
        "GA_WORKER_SRC",
        "LLM_PROXY_CAPABILITY_SIGNING_KEY",
        "LLM_PROXY_ALLOWED_UPSTREAM_CIDRS",
        "LLM_PROXY_ALLOW_HTTP_HOSTS",
    }
    assert set(env) <= set(smoke.CHILD_ENV_ALLOWLIST) | explicit


@pytest.mark.skipif(os.name != "nt", reason="Windows Job Object contract")
def test_job_fallback_cleans_process_when_sampling_fails() -> None:
    smoke = _load_smoke()
    child = subprocess.Popen(
        [
            sys.executable,
            "-c",
            "import signal,time; signal.signal(signal.SIGBREAK, lambda *_: None); time.sleep(30)",
        ],
        creationflags=subprocess.CREATE_NEW_PROCESS_GROUP,
        stdout=subprocess.DEVNULL,
        stderr=subprocess.DEVNULL,
    )
    job = smoke._WindowsJob()
    try:
        job.assign(child)

        def fail_sampling(_pid: int):
            raise smoke.SmokeError("forced sampling failure")

        result = smoke._stop_process_tree(
            child,
            set(),
            job=job,
            sample_descendants=fail_sampling,
            grace_seconds=0.25,
        )
        assert result.used_fallback is True
        assert result.graceful_worker_shutdown is False
        assert child.poll() is not None
    finally:
        job.close()
        if child.poll() is None:
            child.kill()
            child.wait(timeout=5)


def test_submit_dedupe_does_not_run_rss_sampler_before_fixture_release(
    monkeypatch,
) -> None:
    smoke = _load_smoke()
    sampler_calls = 0

    def submit(_base: str, _session: str, _payload: dict):
        return {"task_id": "task-1"}

    def slow_failing_sampler(_pid: int):
        nonlocal sampler_calls
        sampler_calls += 1
        time.sleep(0.25)
        raise smoke.SmokeError("slow sampler must not gate fixture release")

    monkeypatch.setattr(smoke, "_submit", submit)
    monkeypatch.setattr(smoke, "_sample_descendants", slow_failing_sampler)
    arrived = threading.Event()
    arrived.set()
    fixture = SimpleNamespace(server=SimpleNamespace(request_arrived=arrived))

    started = time.monotonic()
    result = smoke._submit_deduped_success("base", "session", "unique", fixture)
    elapsed = time.monotonic() - started

    assert result["task_id"] == "task-1"
    assert sampler_calls == 0
    assert elapsed < 0.1


def test_post_terminal_worker_sample_rejects_empty_descendants(monkeypatch) -> None:
    smoke = _load_smoke()
    monkeypatch.setattr(smoke, "_sample_descendants", lambda _pid: (set(), 0))
    known_children: set[int] = set()

    with pytest.raises(smoke.SmokeError, match="Worker process was not measurable"):
        smoke._sample_worker_after_terminal(SimpleNamespace(pid=7), known_children)

    assert known_children == set()
