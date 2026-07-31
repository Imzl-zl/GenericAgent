from __future__ import annotations

import json
import os
import subprocess
import sys
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
from typing import Any

import pytest

ROOT = Path(__file__).resolve().parents[3]
SCRIPT = ROOT / "tenant_platform" / "tests" / "smoke" / "document_pool_admin.py"
ADMIN_TOKEN = "admin-smoke-secret"
USER_TOKEN = "user-smoke-secret"


class SmokeHandler(BaseHTTPRequestHandler):
    settings_version = 7
    leak_binding = False
    requests: list[tuple[str, str]] = []

    def log_message(self, format: str, *args: Any) -> None:
        return

    def _json(self, status: int, body: Any) -> None:
        raw = json.dumps(body).encode("utf-8")
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(raw)))
        self.end_headers()
        self.wfile.write(raw)

    @classmethod
    def settings(cls) -> dict[str, Any]:
        return {
            "enabled": True,
            "max_active": 1,
            "min_ready": 0,
            "job_idle_ttl_seconds": 600,
            "ready_idle_ttl_seconds": 300,
            "global_queue_limit": 100,
            "per_tenant_queue_limit": 20,
            "per_tenant_active_limit": 1,
            "job_timeout_seconds": 3600,
            "command_timeout_seconds": 300,
            "version": cls.settings_version,
            "updated_by": 1,
            "updated_at": "2026-07-31T04:00:00Z",
            "reason": "smoke",
            "deployment_max_active": 1,
            "apply_status": "applied",
        }

    def _admin(self) -> bool:
        return self.headers.get("X-Platform-Dev-Token") == ADMIN_TOKEN

    def do_GET(self) -> None:
        type(self).requests.append(("GET", self.path))
        if self.path == "/healthz":
            self._json(200, {"status": "ok"})
            return
        if self.path == "/v1/sops":
            if self.headers.get("Authorization") != f"Bearer {USER_TOKEN}":
                self._json(401, {"code": "UNAUTHORIZED"})
            else:
                self._json(200, {"sops": []})
            return
        if not self._admin():
            self._json(401, {"code": "UNAUTHORIZED"})
            return
        if self.path == "/v1/admin/settings/document-pool":
            self._json(200, type(self).settings())
        elif self.path == "/v1/admin/document-pool/status":
            self._json(200, {
                "jobs_queued": 2, "jobs_starting": 1, "jobs_running": 0,
                "instances_creating": 0, "instances_ready": 1, "instances_allocated": 1,
                "instances_running": 0, "instances_destroying": 0, "instances_lost": 0,
                "commands_pending": 1, "commands_executing": 0,
                "oldest_queued_at": "2026-07-31T03:55:00Z", "observed_at": "2026-07-31T04:00:00Z",
            })
        elif self.path == "/v1/admin/sophub/binding":
            body = {"configured": False, "author_type": "", "agent_uid": "", "display_name": ""}
            if type(self).leak_binding:
                body["api_key"] = "upstream-super-secret"
            self._json(200, body)
        elif self.path == "/v1/admin/sop-candidates":
            self._json(200, {"candidates": []})
        elif self.path == "/v1/admin/sops":
            self._json(200, {"sops": []})
        else:
            self._json(404, {"code": "NOT_FOUND"})

    def do_PUT(self) -> None:
        type(self).requests.append(("PUT", self.path))
        if self.path != "/v1/admin/settings/document-pool" or not self._admin():
            self._json(401, {"code": "UNAUTHORIZED"})
            return
        length = int(self.headers.get("Content-Length", "0"))
        body = json.loads(self.rfile.read(length))
        if body.get("expected_version") != type(self).settings_version:
            self._json(409, {"code": "CONFLICT"})
            return
        type(self).settings_version += 1
        self._json(200, type(self).settings())


@pytest.fixture
def smoke_server():
    SmokeHandler.settings_version = 7
    SmokeHandler.leak_binding = False
    SmokeHandler.requests = []
    server = ThreadingHTTPServer(("127.0.0.1", 0), SmokeHandler)
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    try:
        yield f"http://127.0.0.1:{server.server_port}"
    finally:
        server.shutdown()
        thread.join(timeout=5)
        server.server_close()


def _run(base_url: str, **extra: str) -> subprocess.CompletedProcess[str]:
    env = {
        **os.environ,
        "GA_PLATFORM_BASE_URL": base_url,
        "PLATFORM_DEV_TOKEN": ADMIN_TOKEN,
        "GA_USER_TOKEN": USER_TOKEN,
        **extra,
    }
    return subprocess.run(
        [sys.executable, str(SCRIPT)],
        cwd=ROOT,
        env=env,
        capture_output=True,
        text=True,
        timeout=30,
        check=False,
    )


def test_document_pool_admin_smoke_checks_real_read_surfaces_without_leaking_tokens(smoke_server: str) -> None:
    completed = _run(smoke_server)
    output = completed.stdout + completed.stderr
    assert completed.returncode == 0, output
    assert "settings_version=7 queued=2 active=1 ready=1" in completed.stdout
    assert ADMIN_TOKEN not in output
    assert USER_TOKEN not in output
    assert SmokeHandler.requests.count(("GET", "/v1/admin/document-pool/status")) == 3
    assert ("GET", "/v1/sops") in SmokeHandler.requests


def test_document_pool_admin_smoke_requires_tenant_token(smoke_server: str) -> None:
    completed = _run(smoke_server, GA_USER_TOKEN="")
    assert completed.returncode != 0
    assert "GA_USER_TOKEN is required" in completed.stderr


def test_document_pool_admin_smoke_opt_in_cas_preserves_values(smoke_server: str) -> None:
    completed = _run(smoke_server, GA_DOCUMENT_ADMIN_SMOKE_MUTATE="1")
    assert completed.returncode == 0, completed.stdout + completed.stderr
    assert "settings_version=8" in completed.stdout
    assert ("PUT", "/v1/admin/settings/document-pool") in SmokeHandler.requests


def test_document_pool_admin_smoke_rejects_secret_fields_without_echo(smoke_server: str) -> None:
    SmokeHandler.leak_binding = True
    completed = _run(smoke_server)
    output = completed.stdout + completed.stderr
    assert completed.returncode != 0
    assert "upstream-super-secret" not in output
    assert ADMIN_TOKEN not in output
