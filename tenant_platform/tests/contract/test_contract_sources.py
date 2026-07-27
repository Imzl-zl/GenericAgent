import json
from pathlib import Path

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
    assert "rpc ReloadCredentials" in text
    assert "credential_generation" in text
    assert "config_checksum" in text
    assert "api_key" not in text.lower()
    assert "string delivery_id" not in text
    assert "string result_ref" not in text
    for name in ("TASK_SUCCEEDED", "TASK_CANCELLED", "TASK_INTERRUPTED", "TASK_FAILED"):
        assert name in text


def test_llm_proxy_contract_declares_versioned_service_and_no_api_key():
    """Plan Task 1 Step 1: tests must read BOTH protobuf files.

    Plan Task 1 Step 3: neither protobuf contract may contain a real API key
    field. This guards llm_proxy.proto against regressions that reintroduce a
    key/secret field or rename the versioned service.
    """
    text = (ROOT / "contracts/proto/genericagent/proxy/v1/llm_proxy.proto").read_text(
        encoding="utf-8"
    )
    assert "package genericagent.proxy.v1;" in text
    assert "service LlmProxyService" in text
    assert "rpc Generate(GenerateRequest) returns (stream GenerateEvent)" in text
    assert "capability_token" in text
    assert "session_key" in text
    # Deny any field name that looks like an upstream API key or secret.
    lowered = text.lower()
    for forbidden in (
        "api_key",
        "apikey",
        "api_key_id",
        "secret",
        "secret_key",
        "bearer_token",
    ):
        assert forbidden not in lowered, (
            f"llm_proxy.proto must not declare {forbidden!r}"
        )


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
        "tool_policy_version",
    ):
        assert field in text
    task_required = text.split("TaskEnvelope:", 1)[1].split("properties:", 1)[0]
    for field in (
        "message_id",
        "source_instance_id",
        "prompt",
        "source",
        "persona_snapshot",
        "tool_policy_version",
    ):
        assert f"- {field}" in task_required


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
