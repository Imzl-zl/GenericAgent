import json
from pathlib import Path

import yaml

ROOT = Path(__file__).parents[2]


def test_worker_contract_declares_versioned_service_and_terminal_states():
    text = (ROOT / "contracts/proto/genericagent/worker/v1/worker.proto").read_text(
        encoding="utf-8"
    )
    assert "package genericagent.worker.v1;" in text
    assert "service WorkerService" in text
    assert "rpc BeginCheckpoint" in text
    assert "source_instance_id" in text
    assert "snapshot_checksum" in text
    assert "policy_digest" in text
    # 决策 D1: 凭证热刷新协议已删除, 契约不得重新引入。
    assert "rpc ReloadCredentials" not in text
    assert "credential_generation" not in text
    assert "config_checksum" not in text
    assert "api_key" not in text.lower()
    assert "string delivery_id" not in text
    assert "string result_ref" not in text
    for name in ("TASK_SUCCEEDED", "TASK_CANCELLED", "TASK_INTERRUPTED", "TASK_FAILED"):
        assert name in text


def test_foundation_policy_is_deny_by_default_without_host_tools():
    policy = json.loads(
        (ROOT / "contracts/policy/foundation.v1.json").read_text(encoding="utf-8")
    )
    assert policy["schema_version"] == "genericagent.capability-policy.v1"
    tool_policy = policy["capabilities"]["foundation.v1"]["tool_policies"][
        "foundation.no-host-tools.v1"
    ]
    assert tool_policy["allowed_tools"] == ["update_working_checkpoint"]
    assert not set(tool_policy["allowed_tools"]) & {
        "code_run",
        "file_read",
        "file_write",
        "file_patch",
        "web_scan",
        "web_execute_js",
    }


def test_platform_openapi_exposes_foundation_task_paths():
    text = (ROOT / "contracts/openapi/platform.yaml").read_text(encoding="utf-8")
    for path in (
        "/healthz",
        "/v1/sessions/{session_key}/tasks",
        "/v1/tasks/{task_id}",
        "/v1/tasks/{task_id}/cancel",
        "/v1/tasks/{task_id}/result",
    ):
        assert path in text
    for field in (
        "source_instance_id",
        "snapshot_id",
        "snapshot_checksum",
        "result_ref",
    ):
        assert field in text
    # 审查 D1(去分级): tool_policy_version 已从任务提交契约移除,
    # 策略统一由平台内部默认档位决定, 客户端不再指定。
    assert "tool_policy_version" not in text
    task_required = text.split("TaskEnvelope:", 1)[1].split("properties:", 1)[0]
    for field in (
        "message_id",
        "source_instance_id",
        "prompt",
        "source",
        "persona_snapshot",
    ):
        assert f"- {field}" in task_required
    # 审查 D1(去分级): 客户端不再提交 tool_policy_version。
    assert "- tool_policy_version" not in task_required


def test_sophub_search_route_matches_backend_web_and_openapi():
    openapi = yaml.safe_load(
        (ROOT / "contracts/openapi/platform.yaml").read_text(encoding="utf-8")
    )
    backend = (
        ROOT / "backend-go/internal/api/admin.go"
    ).read_text(encoding="utf-8")
    routes = (
        ("GET", "/v1/admin/sophub/search"),
        ("GET", "/v1/admin/sophub/binding"),
    )
    for method, path in routes:
        assert method.lower() in openapi["paths"][path], f"OpenAPI missing {method} {path}"
        assert f'{method} {path}' in backend, f"backend missing {method} {path}"


def test_platform_openapi_declares_error_response_schema_and_status_enum():
    """Plan Task 5 Step 6: JSON errors with stable code/message/trace_id.

    Plan Task 5 Step 1: status values are the 7 lifecycle states.
    """
    text = (ROOT / "contracts/openapi/platform.yaml").read_text(encoding="utf-8")
    # ErrorResponse component must exist with required code/message/trace_id.
    err_block = text.split("ErrorResponse:", 1)[1].split("TaskEnvelope:", 1)[0]
    for field in ("code", "message", "trace_id"):
        assert f"- {field}" in err_block
    # TaskStatus.status must enumerate the 7 lifecycle states.
    status_block = text.split("TaskStatus:", 1)[1]
    for value in (
        "queued",
        "starting",
        "running",
        "succeeded",
        "failed",
        "cancelled",
        "interrupted",
    ):
        assert value in status_block
    # Source must be constrained to wechat|web (architecture §4.2).
    envelope_block = text.split("TaskEnvelope:", 1)[1].split("TaskStatus:", 1)[0]
    assert "enum: [wechat, web]" in envelope_block
    # At least one path must reference the error response.
    assert '$ref: "#/components/responses/InternalError"' in text


def test_provider_openapi_is_strict_token_proxy_cutover():
    text = (ROOT / "contracts/openapi/platform.yaml").read_text(encoding="utf-8")
    for path in (
        "/v1/admin/llm-providers:",
        "/v1/admin/llm-providers/{provider_id}:",
        "/v1/admin/llm-providers/{provider_id}/default:",
        "/v1/admin/llm-providers/{provider_id}/disable:",
        "/v1/admin/llm-providers/{provider_id}/enable:",
    ):
        assert path in text
    for field in ("session_config", "transport_config", "revision"):
        assert field in text
    for provider_type in ("native_oai", "native_claude"):
        assert provider_type in text
    for removed in (
        "/v1/config/mykey.py",
        "openai_compatible",
        "anthropic_messages",
        "failure_threshold",
        "cooldown_seconds",
        "preferred_model",
        "model_map",
    ):
        assert removed not in text


def test_legacy_plaintext_mykey_scripts_are_removed():
    assert not (ROOT / "test-mykey-generation.sh").exists()
    assert not (ROOT / "test-mykey-generation.ps1").exists()
