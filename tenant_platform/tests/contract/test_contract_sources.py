import json
from pathlib import Path

ROOT = Path(__file__).parents[2]


def test_worker_contract_declares_versioned_service_and_terminal_states():
    text = (ROOT / "contracts/proto/genericagent/worker/v1/worker.proto").read_text(encoding="utf-8")
    assert "package genericagent.worker.v1;" in text
    assert "service WorkerService" in text
    assert "rpc BeginCheckpoint" in text
    assert "source_instance_id" in text
    assert "snapshot_checksum" in text
    assert "policy_digest" in text
    assert "string delivery_id" not in text
    assert "string result_ref" not in text
    for name in ("TASK_SUCCEEDED", "TASK_CANCELLED", "TASK_INTERRUPTED", "TASK_FAILED"):
        assert name in text


def test_foundation_policy_is_deny_by_default_without_host_tools():
    policy = json.loads((ROOT / "contracts/policy/foundation.v1.json").read_text(encoding="utf-8"))
    assert policy["schema_version"] == "genericagent.capability-policy.v1"
    tool_policy = policy["capabilities"]["foundation.v1"]["tool_policies"]["foundation.no-host-tools.v1"]
    assert tool_policy["allowed_tools"] == ["update_working_checkpoint"]
    assert not set(tool_policy["allowed_tools"]) & {
        "code_run", "file_read", "file_write", "file_patch", "web_scan", "web_execute_js"
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
    for field in ("source_instance_id", "snapshot_id", "snapshot_checksum", "result_ref", "tool_policy_version"):
        assert field in text
    task_required = text.split("TaskEnvelope:", 1)[1].split("properties:", 1)[0]
    for field in ("message_id", "source_instance_id", "prompt", "source", "persona_snapshot", "tool_policy_version"):
        assert f"- {field}" in task_required
