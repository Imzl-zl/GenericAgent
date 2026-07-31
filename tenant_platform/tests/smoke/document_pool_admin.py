#!/usr/bin/env python3
"""Authenticated control-plane smoke for the secure document platform."""

from __future__ import annotations

import json
import os
import socket
import sys
import urllib.error
import urllib.parse
import urllib.request
from typing import Any

MAX_RESPONSE_BYTES = 256 * 1024
TIMEOUT_SECONDS = 5
ADMIN_TOKEN_ENV = "PLATFORM_DEV_TOKEN"
USER_TOKEN_ENV = "GA_USER_TOKEN"
FORBIDDEN_RESPONSE_KEYS = {
    "api_key",
    "api_key_ciphertext",
    "ciphertext",
    "authorization",
    "access_token",
    "provider_key",
    "bot_token",
    "workspace_id",
    "requester_user_id",
    "payload",
    "slot_path",
    "instance_name",
    "runtime_id",
}


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):  # noqa: ANN001, ANN201
        raise urllib.error.HTTPError(req.full_url, code, "redirect denied", headers, fp)


def _base_url() -> str:
    base = os.environ.get("GA_PLATFORM_BASE_URL", "http://127.0.0.1:8080").rstrip("/")
    parsed = urllib.parse.urlparse(base)
    if parsed.scheme not in {"http", "https"} or parsed.hostname not in {"127.0.0.1", "::1", "localhost"}:
        raise ValueError("GA_PLATFORM_BASE_URL must be loopback http(s)")
    if parsed.username or parsed.password or parsed.query or parsed.fragment:
        raise ValueError("GA_PLATFORM_BASE_URL must not contain credentials, query or fragment")
    return base


def _request(path: str, *, token: str | None = None, body: dict[str, Any] | None = None) -> Any:
    method = "PUT" if body is not None else "GET"
    payload = None if body is None else json.dumps(body, separators=(",", ":")).encode("utf-8")
    request = urllib.request.Request(_base_url() + path, data=payload, method=method)
    request.add_header("Accept", "application/json")
    if payload is not None:
        request.add_header("Content-Type", "application/json")
    if token is not None:
        request.add_header("X-Platform-Dev-Token", token)
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}), NoRedirect())
    with opener.open(request, timeout=TIMEOUT_SECONDS) as response:
        raw = response.read(MAX_RESPONSE_BYTES + 1)
        if len(raw) > MAX_RESPONSE_BYTES:
            raise ValueError(f"response too large for {path}")
        if response.status < 200 or response.status >= 300:
            raise ValueError(f"unexpected HTTP status {response.status} for {path}")
    try:
        return json.loads(raw)
    except json.JSONDecodeError as error:
        raise ValueError(f"invalid JSON from {path}") from error


def _expect_admin_rejected(path: str, *, bearer_token: str | None = None) -> None:
    request = urllib.request.Request(_base_url() + path, method="GET")
    request.add_header("Accept", "application/json")
    if bearer_token is not None:
        request.add_header("Authorization", f"Bearer {bearer_token}")
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}), NoRedirect())
    try:
        with opener.open(request, timeout=TIMEOUT_SECONDS) as response:
            response.read(MAX_RESPONSE_BYTES + 1)
    except urllib.error.HTTPError as error:
        try:
            raw = error.read(MAX_RESPONSE_BYTES + 1)
            if len(raw) > MAX_RESPONSE_BYTES:
                raise ValueError(f"rejection response too large for {path}")
            if error.code not in {401, 403}:
                raise ValueError(f"unexpected rejection status {error.code} for {path}")
            return
        finally:
            error.close()
    raise ValueError(f"unauthorized request unexpectedly succeeded for {path}")


def _assert_no_forbidden_keys(value: Any, path: str = "response") -> None:
    if isinstance(value, dict):
        for key, child in value.items():
            if key.lower() in FORBIDDEN_RESPONSE_KEYS:
                raise ValueError(f"forbidden field {key!r} in {path}")
            _assert_no_forbidden_keys(child, f"{path}.{key}")
    elif isinstance(value, list):
        for index, child in enumerate(value):
            _assert_no_forbidden_keys(child, f"{path}[{index}]")


def _require_int(data: dict[str, Any], key: str, *, minimum: int = 0) -> int:
    value = data.get(key)
    if not isinstance(value, int) or isinstance(value, bool) or value < minimum:
        raise ValueError(f"{key} must be an integer >= {minimum}")
    return value


def _validate_status(status: Any) -> dict[str, Any]:
    if not isinstance(status, dict):
        raise ValueError("document pool status must be an object")
    for key in (
        "jobs_queued",
        "jobs_starting",
        "jobs_running",
        "instances_creating",
        "instances_ready",
        "instances_allocated",
        "instances_running",
        "instances_destroying",
        "instances_lost",
        "commands_pending",
        "commands_executing",
    ):
        _require_int(status, key)
    if not isinstance(status.get("observed_at"), str) or not status["observed_at"]:
        raise ValueError("document pool status observed_at is required")
    _assert_no_forbidden_keys(status, "document_pool_status")
    return status


def _validate_settings(settings: Any) -> dict[str, Any]:
    if not isinstance(settings, dict):
        raise ValueError("document pool settings must be an object")
    for key in (
        "max_active",
        "min_ready",
        "job_idle_ttl_seconds",
        "ready_idle_ttl_seconds",
        "global_queue_limit",
        "per_tenant_queue_limit",
        "per_tenant_active_limit",
        "job_timeout_seconds",
        "command_timeout_seconds",
        "version",
        "deployment_max_active",
    ):
        _require_int(settings, key)
    if not isinstance(settings.get("enabled"), bool):
        raise ValueError("document pool enabled must be boolean")
    if settings["max_active"] > settings["deployment_max_active"]:
        raise ValueError("persisted max_active exceeds deployment hard limit")
    _assert_no_forbidden_keys(settings, "document_pool_settings")
    return settings


def _cas_noop(settings: dict[str, Any], token: str) -> dict[str, Any]:
    mutable_fields = (
        "enabled",
        "max_active",
        "min_ready",
        "job_idle_ttl_seconds",
        "ready_idle_ttl_seconds",
        "global_queue_limit",
        "per_tenant_queue_limit",
        "per_tenant_active_limit",
        "job_timeout_seconds",
        "command_timeout_seconds",
    )
    body = {key: settings[key] for key in mutable_fields}
    body["expected_version"] = settings["version"]
    body["reason"] = "target-host deployment smoke CAS"
    updated = _validate_settings(_request("/v1/admin/settings/document-pool", token=token, body=body))
    if updated["version"] != settings["version"] + 1:
        raise ValueError("document pool CAS did not advance exactly one version")
    for key in mutable_fields:
        if updated[key] != settings[key]:
            raise ValueError(f"document pool CAS changed {key}")
    if updated.get("apply_status", "applied") != "applied":
        raise ValueError("document pool settings were persisted but not applied")
    return updated


def main() -> int:
    admin_token = os.environ.get(ADMIN_TOKEN_ENV, "")
    if not admin_token:
        print(f"document-pool-admin-smoke: FAIL: {ADMIN_TOKEN_ENV} is required", file=sys.stderr)
        return 1
    user_token = os.environ.get(USER_TOKEN_ENV, "")
    if not user_token:
        print(f"document-pool-admin-smoke: FAIL: {USER_TOKEN_ENV} is required", file=sys.stderr)
        return 1
    try:
        health = _request("/healthz")
        if health != {"status": "ok"}:
            raise ValueError("healthz response is not ok")
        _expect_admin_rejected("/v1/admin/document-pool/status")
        _expect_admin_rejected("/v1/admin/document-pool/status", bearer_token=user_token)
        settings = _validate_settings(_request("/v1/admin/settings/document-pool", token=admin_token))
        status = _validate_status(_request("/v1/admin/document-pool/status", token=admin_token))
        for path in ("/v1/admin/sophub/binding", "/v1/admin/sop-candidates", "/v1/admin/sops"):
            _assert_no_forbidden_keys(_request(path, token=admin_token), path)
        if os.environ.get("GA_DOCUMENT_ADMIN_SMOKE_MUTATE") == "1":
            settings = _cas_noop(settings, admin_token)
        request = urllib.request.Request(_base_url() + "/v1/sops", method="GET")
        request.add_header("Accept", "application/json")
        request.add_header("Authorization", f"Bearer {user_token}")
        opener = urllib.request.build_opener(urllib.request.ProxyHandler({}), NoRedirect())
        with opener.open(request, timeout=TIMEOUT_SECONDS) as response:
            raw = response.read(MAX_RESPONSE_BYTES + 1)
        if len(raw) > MAX_RESPONSE_BYTES:
            raise ValueError("tenant SOP response too large")
        _assert_no_forbidden_keys(json.loads(raw), "/v1/sops")
        print(
            "document-pool-admin-smoke: OK: "
            f"settings_version={settings['version']} queued={status['jobs_queued']} "
            f"active={status['jobs_starting'] + status['jobs_running']} ready={status['instances_ready']}"
        )
        return 0
    except (OSError, ValueError, urllib.error.URLError, socket.timeout, json.JSONDecodeError) as error:
        print(f"document-pool-admin-smoke: FAIL: {type(error).__name__}", file=sys.stderr)
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
