# Multi-Tenant Platform Foundation Implementation Plan

> **Status (updated 2026-07-24):** Foundation slice IMPLEMENTED. All 6 tasks / 37 steps complete and verified by Go + Python test suites and live API smoke testing (single session, 3-way personal-session concurrency, minimal team workspace, non-member rejection). Checkbox state below has been reconciled with the codebase.
>
> **Session 2026-07-24 additions (beyond original plan scope):**
> - Multi-session concurrency: scheduler refactored from single-worker to per-`session_key` worker pool; verified 3 personal sessions run in parallel with isolated worker processes and no cross-talk.
> - Minimal team workspace: `0002_team_tables.sql` adds `teams` + `team_members`; `team_store.go` provides idempotent `EnsureTeamContext` and `authorizeSubmitter`; `--dev-team` flag bootstraps a dev team. Verified owner + member submit success and non-member rejection.
> - Three blocking bugs fixed: (1) `mykey.py` `mixin_config` with empty `llm_nos` crashed worker start with `'dict' object has no attribute 'backend'`; (2) `EnsureTeamContext` was not idempotent across restarts (`teams_owner_name_uq` violation); (3) Python `Path.resolve()` normalized Windows drive-letter case, causing `staging ref mismatch` at checkpoint commit.
> - Stale `genericagent_worker/` empty package removed; `.gitignore` extended for `tenant_platform/runtime/runtime_data/`.
>
> **Session 2026-07-25 — Slice 2a (LLM Proxy + capability_token):**
> - Closed the #1 security gap: real upstream LLM key removed from the Worker path. `cmd/llm-proxy` holds the real key (host-env injected); the scheduler issues a short-lived, HMAC-SHA256 signed, session-bound `capability_token` and writes a token-only `mykey.py` (Proxy URL + token, no real key) into `GA_CONFIG_ROOT`.
> - Legacy `writeFixtureMyKey` / `startOAIFixture` / user-provided-`mykey.py` branch REMOVED from `scheduler.go` (security red line: always Proxy path).
> - Token revocation via in-memory denylist (`/internal/revoke` endpoint); per-session issuance, revoked on Worker cleanup.
> - `cmd/platform` starts an in-process LLM Proxy in dev-loopback (or accepts `--llm-proxy-addr` for external deployment).
> - Smoke + integration tests updated to use the Proxy path end-to-end; 10-test security audit suite added (`tenant_platform/tests/security/test_no_real_key_leak.py`).
>
> **Session 2026-07-25 — Slice 2b (Worker Containerization, rootless Podman):**
> - Closed the #2 P0 security gap: Worker execution moved from host subprocess to rootless Podman container per active session.
> - Introduced `WorkerRuntime` interface (`internal/worker/runtime.go`) with `LoopbackWorkerRuntime` (dev/Windows) and `ManagerWorkerRuntime` (podman) implementations.
> - Added `genericagent.worker.manager.v1` gRPC contract and generated Go bindings.
> - Implemented `cmd/worker-manager` and `internal/workermanager` (server + Podman runtime) to own container allocate/release/list lifecycle.
> - Added `tenant_platform/worker-python/Dockerfile` + `.dockerignore` based on `python:3.11-slim`.
> - Wired `--worker-runtime=loopback|podman` and `--worker-manager-addr` into `cmd/platform`; podman mode uses session-scoped config dirs.
> - Go unit tests pass for manager client, Podman runtime (fake executor), and platform runtime switch. Real Podman end-to-end remains Linux-only and is deferred to deployment verification.
>
> **Session 2026-07-25 — Slice 3b (IM Gateway / iLink real access):**
> - Added encrypted bot registration admin API (`POST /v1/admin/bots`) with AES-256-GCM token storage.
> - Implemented production `ILinkAdapter` (`internal/transport/ilink.go`) that resolves bot tokens, decrypts them, and calls the iLink send-message HTTP API.
> - Implemented `DeliveryService` (`internal/application/delivery_service.go`) that polls the `task_deliveries` outbox, retries with exponential backoff, and dead-letters expired rows.
> - Wired iLink transport, cipher, bot store, and delivery service into `cmd/platform` via `--ilink-base-url`, `--bot-token-key`, and `BOT_TOKEN_KEY`/`ILINK_BASE_URL` env vars.
> - Added unit tests for delivery retry/dead-letter/ack logic.
>
> **Windows/Linux compatibility note:** GA Core (`agentmain.py`) remains cross-platform and runs on Windows unchanged. The Go platform layer defaults to `--worker-runtime=loopback`, which starts a local Python Worker subprocess and works on Windows. Only the optional `--worker-runtime=podman` path (container isolation via rootless Podman) is Linux-only. iLink delivery works on any platform that can reach the iLink base URL.

> **Session 2026-07-25 — Slice 3c (iLink media + Windows loopback E2E + bug fixes):**
> - Media file send/receive fully wired through WeChat (iLink CDN), NOT through Web UI. Inbound: `wxbot_media.download_media` → `media_paths` in webhook body → router appends to prompt. Outbound: `poller.Client.SendMessage(msg_type=image/video/file)` → `WxBotClient.send_image/send_video/send_file` → iLink CDN upload. `file_item` (iLink type 4) supports any file format (Word/Excel/PDF/ZIP) per official protocol — no extension whitelist at protocol level.
> - Windows loopback mode (`--worker-runtime=loopback`) verified end-to-end: mock iLink → Bot Poller → Platform → Loopback Worker subprocess → LLM Proxy → deepseek-v4-flash → reply back to mock iLink. No container required on Windows.
> - Two bugs fixed during verification: (1) `bot_transport_store.go` `last_error_code` NULL scan crash on bot restore — changed to `*string` deref; (2) `LoopbackConfig` missing `PolicyFile` field caused worker subprocess to exit (missing `GA_POLICY_FILE` env var) — added field and wired `boot.PolicyFile` through.
> - Web UI is management-only (registration/approval/binding/persona/provider config); chat happens in WeChat. Other IM platforms (QQ/Feishu/DingTalk/WeCom/Telegram) can reuse GA's existing `frontends/*.py` via the same Poller pattern. See [iLink binding flow SPEC](../specs/2026-07-25-ilink-official-binding-flow.md) §7.5–§7.6.
>
> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Build the first real vertical slice from a Go platform task API through PostgreSQL and a versioned RPC to the existing Python GenericAgent, without modifying legacy runtime files.

**Architecture:** The new code lives under `tenant_platform/`. Go owns the platform task store and PostgreSQL transactions. A separate Python process wraps the existing `GenericAgent` behind gRPC; the foundation slice uses a real loopback worker process, while rootless Podman and the production worker-manager are deferred to the next plan. The legacy root modules and existing `frontends/` remain untouched.

**Tech Stack:** Go 1.22+, Python `>=3.10,<3.14`, `google.golang.org/grpc`, `google.golang.org/protobuf`, Python `grpcio`/`grpcio-tools`, PostgreSQL via `pgx/v5`, standard-library `net/http`, pytest, and a local deterministic OpenAI-compatible HTTP fixture for the Worker smoke test.

## Global Constraints

- Do not modify `agentmain.py`, `ga.py`, `llmcore.py`, `agent_loop.py`, `mykey.py`, or any existing file under `frontends/` in this plan.
- Do not move or rename legacy files and do not add compatibility aliases to the legacy runtime.
- The Worker adapter must build an isolated runtime overlay under `GA_RUNTIME_DIR/<session>` for all tenant-writable paths; static legacy assets remain read-only. Tests must prove no writes reach `GA_LEGACY_ROOT`.
- PostgreSQL is the only task fact source. Do not add RabbitMQ, Redis, Celery, or an in-memory-only queue.
- No real API keys may appear in source, fixtures, snapshots, logs, or test output. The smoke test uses a temporary ignored `mykey.py` and a local HTTP fixture.
- Every new behavior gets a failing test first. Unit tests must finish within 60 seconds; integration tests must fail clearly when `TEST_DATABASE_URL` is absent rather than silently switching to SQLite or an in-memory substitute.
- Generated protobuf code is never edited by hand; edit `tenant_platform/contracts/proto/genericagent/**/v1/*.proto` and regenerate.
- The shared `tenant_platform/contracts/policy/foundation.v1.json` is the only foundation capability registry source; Go and Python load it, compare its SHA-256 digest, and fail startup on a missing, malformed, or mismatched policy file.
- Foundation endpoints bind to loopback and use a development token. They are not a production authentication implementation.
- `--dev-loopback` requires an explicit `PLATFORM_DEV_USER_ID`, development token, and database URL; startup transactionally upserts only that approved development user and its `personal:<user_id>` workspace. Normal startup must reject this bootstrap path. The local coordinator's file actions are a development-only exception; production file ownership remains with worker-manager/Workspace Store.
- `cmd/platform` generates a cryptographically random `platform_instance_id` once per process start, injects it into the scheduler, stores it in `tasks.claim_owner`, and never derives it from a tenant/session value.
- A successful task must produce a bounded checkpoint bundle and a terminal event. A cancellation must produce a distinct terminal status and must not rerun the task.

---

### Task 1: Create the contract and package boundaries

**Files:**
- Create: `tenant_platform/contracts/proto/genericagent/worker/v1/worker.proto`
- Create: `tenant_platform/contracts/proto/genericagent/proxy/v1/llm_proxy.proto`
- Create: `tenant_platform/contracts/openapi/platform.yaml`
- Create: `tenant_platform/contracts/policy/foundation.v1.json`
- Create: `tenant_platform/backend-go/go.mod`
- Create: `tenant_platform/worker-python/pyproject.toml`
- Create: `tenant_platform/tests/contract/test_contract_sources.py`

**Interfaces:**
- Produces the versioned `genericagent.worker.v1.WorkerService` contract consumed by Tasks 2–6.
- Produces the initial platform HTTP contract and shared foundation capability policy consumed by Tasks 2–6 and the later React plan.

- [x] **Step 1: Write the contract-source tests**

Create `tenant_platform/tests/contract/test_contract_sources.py` with tests that read both protobuf files, the OpenAPI file, and the shared policy manifest and assert:

```python
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
```

- [x] **Step 2: Run the contract-source tests and verify the red state**

Run:

```bash
python -m pytest tenant_platform/tests/contract/test_contract_sources.py -q
```

Expected: FAIL because the contract files do not exist yet. Do not create empty files just to make this test pass.

- [x] **Step 3: Write the Worker protobuf contract**

`worker.proto` must define the following service and message shape:

```protobuf
syntax = "proto3";

package genericagent.worker.v1;
option go_package = "github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/gen/worker/v1;workerv1";

import "google/protobuf/timestamp.proto";

enum TerminalStatus {
  TERMINAL_STATUS_UNSPECIFIED = 0;
  TASK_SUCCEEDED = 1;
  TASK_FAILED = 2;
  TASK_CANCELLED = 3;
  TASK_INTERRUPTED = 4;
}

message RuntimePolicy {
  uint32 max_turns = 1;
  uint64 max_history_bytes = 2;
  uint64 max_working_bytes = 3;
  uint64 max_output_bytes = 4;
  uint32 task_timeout_seconds = 5;
  string capability_version = 6;
  string policy_digest = 7;
}

message StartSessionRequest {
  string session_key = 1;
  string snapshot_ref = 2;
  RuntimePolicy runtime_policy = 3;
  string snapshot_id = 4;
  string snapshot_checksum = 5;
}

message StartSessionResponse {
  string session_key = 1;
  string worker_instance_id = 2;
}

message TaskEnvelope {
  string task_id = 1;
  string session_key = 2;
  int64 requester_user_id = 3;
  string source = 4;
  string source_instance_id = 5;
  string message_id = 6;
  string prompt = 7;
  repeated string persona_snapshot = 8;
  string tool_policy_version = 9;
  google.protobuf.Timestamp created_at = 10;
}

message ExecuteTaskRequest { TaskEnvelope task = 1; }

message Chunk {
  string task_id = 1;
  string text = 2;
  uint32 turn = 3;
}

message ToolProgress {
  string task_id = 1;
  string text = 2;
  uint32 turn = 3;
}

message BeginCheckpointRequest {
  string task_id = 1;
  string checkpoint_token = 2;
  string staging_ref = 3;
  uint64 max_bundle_bytes = 4;
}

message CheckpointReady {
  string task_id = 1;
  string checkpoint_token = 2;
  string staging_ref = 3;
  string checksum = 4;
  string result_digest = 5;
}

message ErrorEnvelope {
  string code = 1;
  string user_message = 2;
  string trace_id = 3;
}

message Terminal {
  string task_id = 1;
  TerminalStatus status = 2;
  string result_digest = 3;
  string user_message = 4;
  ErrorEnvelope error = 5;
}

message WorkerEvent {
  oneof payload {
    Chunk chunk = 1;
    ToolProgress tool_progress = 2;
    Terminal terminal = 3;
  }
}

message CancelTaskRequest { string task_id = 1; }
message CancelTaskResponse { bool accepted = 1; }
message HealthRequest {}
message HealthResponse {
  string worker_instance_id = 1;
  string session_key = 2;
  bool ready = 3;
}
message ShutdownRequest { string reason = 1; }
message ShutdownResponse { bool accepted = 1; }

service WorkerService {
  rpc StartSession(StartSessionRequest) returns (StartSessionResponse);
  rpc ExecuteTask(ExecuteTaskRequest) returns (stream WorkerEvent);
  rpc BeginCheckpoint(BeginCheckpointRequest) returns (CheckpointReady);
  rpc CancelTask(CancelTaskRequest) returns (CancelTaskResponse);
  rpc Health(HealthRequest) returns (HealthResponse);
  rpc Shutdown(ShutdownRequest) returns (ShutdownResponse);
}
```

`llm_proxy.proto` must use package `genericagent.proxy.v1`, Go package `github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/gen/proxy/v1;proxyv1`, and define `LlmProxyService.Generate(GenerateRequest) returns (stream GenerateEvent)`. `GenerateRequest` contains `session_key`, `capability_token`, `model`, and repeated `{role, content}` messages. `GenerateEvent` is a oneof of `{text}`, `{input_tokens, output_tokens}`, and `ErrorEnvelope`. Neither protobuf contract may contain a real API key field.

- [x] **Step 4: Write the initial OpenAPI and capability-policy contracts**

`platform.yaml` must define OpenAPI 3.1 paths and schemas for:

- `GET /healthz` → `{ "status": "ok" }`.
- `POST /v1/sessions/{session_key}/tasks` → accepts required `TaskEnvelope` fields `message_id`, `source_instance_id`, `prompt`, `source`, `persona_snapshot`, and `tool_policy_version`; returns `202` with `task_id` and `status`.
- `GET /v1/tasks/{task_id}` → returns `task_id`, `session_key`, `status`, `snapshot_id`, `snapshot_checksum`, `result_ref`, `result_digest`, `terminal_error`.
- `GET /v1/tasks/{task_id}/result` → reads the bounded committed result payload by opaque `result_ref` and returns its matching digest; it never accepts a host path.
- `POST /v1/tasks/{task_id}/cancel` → returns `accepted` and the current task status.

Document `X-Platform-Dev-Token` as required for this foundation-only loopback API. Write `tenant_platform/contracts/policy/foundation.v1.json` as the single immutable registry source:

```json
{
  "schema_version": "genericagent.capability-policy.v1",
  "capabilities": {
    "foundation.v1": {
      "tool_policies": {
        "foundation.no-host-tools.v1": {
          "allowed_tools": ["update_working_checkpoint"]
        }
      }
    }
  }
}
```

The foundation policy is deny-by-default and deliberately enables no Shell/Python, file, browser, desktop, arbitrary-network, or container-management tool while the loopback Worker has no container boundary. `tool_policy_version` must resolve under `RuntimePolicy.capability_version="foundation.v1"`; both processes compute the manifest SHA-256 and the Worker rejects a different `policy_digest`. Do not expose a browser chat endpoint in this plan; the P0 user chat path remains BotTransportAdapter.

- [x] **Step 5: Add isolated project metadata**

Create `backend-go/go.mod` with module path `github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go`, Go `1.22`, and dependencies only after the generated client is used. Create `worker-python/pyproject.toml` with Python `>=3.10,<3.14`, runtime dependencies `grpcio` and `protobuf`, test dependencies `pytest` and `grpcio-tools`, a `src` package-discovery configuration, pytest `pythonpath = ["src"]`, and a test extra installable with `python -m pip install -e "tenant_platform/worker-python[test]"`. Do not add these dependencies to the root `pyproject.toml` yet.

- [x] **Step 6: Run the contract-source tests and commit the boundary**

Run:

```bash
python -m pip install -e "tenant_platform/worker-python[test]"
python -m pytest tenant_platform/tests/contract/test_contract_sources.py -q
```

Expected: PASS. Commit only the new `tenant_platform/contracts`, `tenant_platform/backend-go/go.mod`, `tenant_platform/worker-python/pyproject.toml`, and contract test files.

---

### Task 2: Generate and verify Go/Python RPC bindings

**Files:**
- Create: `tenant_platform/worker-python/src/genericagent/worker/v1/` generated files
- Create: `tenant_platform/worker-python/src/genericagent/proxy/v1/` generated files
- Create: `tenant_platform/backend-go/internal/gen/worker/v1/` generated files
- Create: `tenant_platform/backend-go/internal/gen/proxy/v1/` generated files
- Create: `tenant_platform/tests/contract/test_generated_bindings.py`

**Interfaces:**
- Produces Python `WorkerServiceServicer` and Go `WorkerServiceClient` bindings used by Tasks 3–5.
- The `.proto` files remain the only hand-edited source.

- [x] **Step 1: Write the binding compatibility test**

The test must add `tenant_platform/worker-python/src` to its import path, import both `genericagent.worker.v1.worker_pb2` and `genericagent.worker.v1.worker_pb2_grpc`, and assert that the generated `WorkerService` descriptor exposes `StartSession`, `ExecuteTask`, `BeginCheckpoint`, `CancelTask`, `Health`, and `Shutdown`. It must assert that `WorkerServiceServicer` and `WorkerServiceStub` exist and that the generated descriptor package is `genericagent.worker.v1`.

- [x] **Step 2: Run the test to verify the red state**

Run:

```bash
python -m pytest tenant_platform/tests/contract/test_generated_bindings.py -q
```

Expected: FAIL with an import error because generated bindings do not exist.

- [x] **Step 3: Generate Python bindings**

From the repository root, run:

```bash
python -m grpc_tools.protoc \
  -I tenant_platform/contracts/proto \
  --python_out=tenant_platform/worker-python/src \
  --grpc_python_out=tenant_platform/worker-python/src \
  tenant_platform/contracts/proto/genericagent/worker/v1/worker.proto \
  tenant_platform/contracts/proto/genericagent/proxy/v1/llm_proxy.proto
```

Add package `__init__.py` files required for Python imports. Keep generated files out of manual edits.

- [x] **Step 4: Generate Go bindings**

Verify `protoc` is installed before generation; a missing compiler or generator plugin is a visible prerequisite failure, never replaced by checked-in hand-written bindings. Install the pinned generator tools in the developer environment and run:

```bash
protoc --version
go install google.golang.org/protobuf/cmd/protoc-gen-go@v1.34.2
go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@v1.5.1
protoc -I tenant_platform/contracts/proto \
  --go_out=module=github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go:tenant_platform/backend-go \
  --go-grpc_out=module=github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go:tenant_platform/backend-go \
  tenant_platform/contracts/proto/genericagent/worker/v1/worker.proto \
  tenant_platform/contracts/proto/genericagent/proxy/v1/llm_proxy.proto
```

- [x] **Step 5: Run both binding checks**

Run:

```bash
python -m pytest tenant_platform/tests/contract/test_generated_bindings.py -q
cd tenant_platform/backend-go
go test ./...
```

Expected: both commands exit 0. Commit the `.proto` sources, generator instructions if captured as a script, and generated bindings together so a clean checkout is buildable.

---

### Task 3: Implement the Python legacy-runtime Worker adapter

**Files:**
- Create: `tenant_platform/worker-python/src/ga_worker/legacy_import.py`
- Create: `tenant_platform/worker-python/src/ga_worker/managed_agent.py`
- Create: `tenant_platform/worker-python/src/ga_worker/checkpoint.py`
- Create: `tenant_platform/worker-python/src/ga_worker/rpc_server.py`
- Create: `tenant_platform/worker-python/src/ga_worker/entrypoint.py`
- Create: `tenant_platform/worker-python/tests/unit/test_managed_agent.py`
- Create: `tenant_platform/worker-python/tests/integration/test_worker_rpc.py`
- Create: `tenant_platform/worker-python/src/ga_worker/runtime_overlay.py`
- Create: `tenant_platform/worker-python/src/ga_worker/limits.py`

**Interfaces:**

`managed_agent.py` must expose:

```python
class ManagedAgentAdapter:
    def start_session(self, request: StartSessionRequest) -> StartSessionResponse: ...
    def execute_task(self, request: ExecuteTaskRequest) -> Iterator[WorkerEvent]: ...
    def begin_checkpoint(self, request: BeginCheckpointRequest) -> CheckpointReady: ...
    def cancel_task(self, task_id: str) -> CancelTaskResponse: ...
    def health(self) -> HealthResponse: ...
    def shutdown(self, reason: str) -> ShutdownResponse: ...
```

The adapter accepts explicit `config_root: Path`, `legacy_root: Path`, and `runtime_root: Path` values plus an injectable `agent_factory` for unit tests. `runtime_overlay.py` is created before importing legacy modules. It materializes immutable runtime copies of `agentmain.py`, `ga.py`, `llmcore.py`, `agent_loop.py`, and `simphtml.py`; `plugins/__init__.py` and `plugins/hooks.py` only; and the exact foundation asset set `assets/tools_schema.json`, `assets/tools_schema_cn.json`, `assets/sys_prompt.txt`, `assets/sys_prompt_en.txt`, `assets/global_mem_insight_template.txt`, `assets/global_mem_insight_template_en.txt`, `assets/insight_fixed_structure.txt`, `assets/insight_fixed_structure_en.txt`, and `assets/code_run_header.py` into `GA_RUNTIME_DIR/<session-id>/legacy-overlay`. No repository `memory/**` or unapproved plugin is copied: the overlay creates per-session writable `memory/global_mem.txt` and `memory/global_mem_insight.txt` from the selected template, plus writable `temp/` and `temp/model_responses/`; any future memory seed or plugin must be named in the immutable capability registry before it is copied. It records a SHA-256 manifest against `GA_LEGACY_ROOT`, marks code and static assets read-only, and keeps all writes inside the resolved session directory. The import path contains the validated config root and overlay, never `GA_LEGACY_ROOT`; config root may contain only `mykey.py`/approved config names and cannot shadow legacy modules. Thus each module's `script_dir` resolves to the isolated overlay while source remains untouched.

`<session-id>` is a platform-issued opaque identifier or a fixed encoding of a validated session identity; it is never a raw path fragment from `session_key`. Resolve the final directory, require it to remain beneath `runtime_root`, and reject separators, traversal, symlink escapes, and collisions before creating the overlay.

`limits.py` must expose immutable policy types loaded once at process start:

```python
@dataclass(frozen=True)
class ToolPolicy:
    version: str
    allowed_tools: frozenset[str]


class CapabilityRegistry:
    digest: str

    @classmethod
    def load(cls, path: Path) -> "CapabilityRegistry": ...

    def resolve(self, capability_version: str, tool_policy_version: str) -> ToolPolicy: ...
```

`load` parses only `genericagent.capability-policy.v1`, hashes the exact file bytes as `sha256:<lowercase-hex>`, rejects duplicate/empty names and empty allowlists, and never supplies a built-in fallback. `resolve` rejects unknown and cross-capability versions.

- [x] **Step 1: Write unit tests for session state and cancellation**

Use a test-only scripted Agent implementing only `put_task`, `abort`, `run`, `is_running`, and a queue-compatible output stream. Cover:

- `start_session` creates exactly one Agent runner.
- A second `start_session` for the same Worker returns the existing session rather than creating a second runtime.
- A repeated `start_session` is idempotent only when `session_key`, snapshot ID/checksum/ref, runtime limits, capability version, and policy digest all match; any conflicting immutable input returns a visible `SESSION_ALREADY_STARTED` error without replacing the active state.
- `execute_task` maps `next` messages to `Chunk` events and `done` to exactly one `TASK_SUCCEEDED` terminal event.
- `cancel_task` distinguishes adapter-pending from started work: pending prompts never enter the legacy queue; started work calls `abort` once; both paths emit one cancelled/interrupted terminal without rerun.
- A second task while one task is running is rejected with a bounded `TASK_ALREADY_RUNNING` error.
- `begin_checkpoint` accepts only the active session's completed task and matching opaque token/staging ref; its `CheckpointReady` response never enters the display event stream.
- `start_session` with a committed snapshot restores backend history, bounded `agent_history`, and seed working state only after schema/checksum validation; an incompatible or mismatched bundle is a visible startup error, and restored display history is never replayed to the live stream.
- A new handler receives the restored working seed before the task runner starts; the task's full working state is captured again at the quiet checkpoint boundary.
- `persona_snapshot` applies only to the current task and is cleared/restored before the next queued task; no session-level persona mutation is used.
- The overlay manifest contains only the five legacy modules, `plugins/__init__.py`, `plugins/hooks.py`, and the exact allowlisted assets; the unit test imports `ga` so the eager `simphtml` dependency is exercised, writes a sentinel in `GA_LEGACY_ROOT`, and asserts it is unchanged after import, task execution, and checkpoint.
- A cancel arriving before runner start emits one cancelled terminal without executing the queued prompt; a cancel during execution invokes `abort` once and maps the terminal result without rerun.
- Runtime policy tests cover max turns, task deadline, history/working/output byte caps, unknown or capability-mismatched `tool_policy_version`, a `policy_digest` mismatch, and disallowed tool names; the checked-in foundation policy resolves to only `update_working_checkpoint`, while test-only injected registries exercise quota wrappers without enabling host tools in the smoke path.

- [x] **Step 2: Run the unit tests and verify the red state**

Run:

```bash
cd tenant_platform/worker-python
python -m pytest tests/unit/test_managed_agent.py -q
```

Expected: FAIL because the adapter modules and methods do not exist.

- [x] **Step 3: Implement the legacy import boundary**

`legacy_import.py` must validate that `legacy_root / "agentmain.py"` exists and then ask `runtime_overlay.py` to materialize `GA_RUNTIME_DIR/<session-id>/legacy-overlay` before importing anything. Symlinks/read-only links are allowed only when their resolved targets are immutable manifest entries; otherwise copy and verify SHA-256 before import. Never include `mykey.py` or legacy `temp/`/`memory/` in the overlay source. Reject reserved module names in `GA_CONFIG_ROOT`, any preloaded legacy module, any imported legacy module absent from the overlay manifest, a missing root, or a current-working-directory fallback. `GA_LEGACY_ROOT` is removed from `sys.path` after materialization. `start_session` then resolves a non-empty `snapshot_ref` only under the permitted runtime/session root, verifies `snapshot_id`, `snapshot_checksum`, and `schema_version`, and loads backend history plus seed working/display state before accepting tasks.

- [x] **Step 4: Implement the adapter state machine**

Use a `threading.Lock`, one session state, an adapter-owned pending task gate, `active_task_id`, `started_event`, `cancel_requested`, a runner thread, and a bounded display buffer. Reserve a task in the adapter before calling legacy `put_task`; a pre-start cancel marks the pending item cancelled and the dispatcher skips it without enqueueing the prompt, then emits exactly one `TASK_CANCELLED`. A started cancel calls legacy `abort` once and waits for the normal terminal boundary. A task deadline requests cancellation and reports a visible timeout; `abort()` is not described as an immediate hard kill for a blocked upstream call.

Before each task, validate `session_key`, `tool_policy_version`, persona limits, and the runtime policy against the injected `CapabilityRegistry`. Require `RuntimePolicy.policy_digest` to equal the registry digest. Set task-scoped `agent.extra_sys_prompts` and a filtered `agentmain.TOOLS_SCHEMA`, and wrap `GenericAgentHandler.dispatch` with the same deny-by-default allowlist so a fabricated tool call cannot bypass schema filtering. Wrap the handler factory so the new handler receives restored working and isolated cwd before the runner starts. Wrap the legacy runner to enforce `max_turns`; count UTF-8 bytes through handler print/tool-output and display paths; on `max_output_bytes`, signal `code_stop_signal`, terminate the active code subprocess through the existing stop path, and emit a visible quota error immediately. Start a task deadline monitor and restore every task-scoped setting in `finally`. The checked-in `foundation.no-host-tools.v1` policy permits only `update_working_checkpoint`; the loopback path exposes no code, file, browser, desktop, network, or container-management tool.

Before dispatching the first restored task, clear the legacy `agent.handler` carryover reference so `GenericAgent.run()` cannot merge stale `key_info` into the new handler. The wrapped handler factory injects a deep copy of the complete snapshot `working` after construction; task completion replaces the adapter seed with the resulting complete working map rather than merging selected keys.

`RuntimePolicy.capability_version` selects a capability in the immutable registry. Each task's `tool_policy_version` must resolve inside that capability, and the request's `policy_digest` must match the exact loaded manifest; unknown versions, cross-capability versions, digest mismatches, or empty allowlists fail before enqueue. History/working limits apply both during snapshot restore and checkpoint encoding, not only after a task finishes.

Map legacy payloads as follows:

```text
{next: text, turn: n} -> WorkerEvent.chunk and bounded display_history
{done: text, turn: n} after normal completion -> Terminal(TASK_SUCCEEDED)
{done: text, turn: n} after cancel/timeout -> Terminal(TASK_CANCELLED/TASK_INTERRUPTED)
exception or policy limit -> Terminal(TASK_FAILED) with bounded user_message and error code
```

The legacy queue has no structured tool-progress signal in this slice. Textual tool markers remain bounded `Chunk` data; the adapter must not fabricate `ToolProgress`. Add the structured mapping as a follow-up contract task and test that no checkpoint or duplicate terminal event enters the display stream. The Worker does not create delivery IDs or immutable result refs; the platform creates them only after checkpoint commit.

- [x] **Step 5: Implement bounded checkpoint bundle writing**

`checkpoint.py` must capture a quiescent task boundary as a JSON bundle with `schema_version="genericagent.snapshot.v1"`, `task_id`, `session_key`, bounded `backend_history`, bounded `working`, bounded `display_history`, and `result={"content_type":"text/plain; charset=utf-8","body":"final user-visible text"}`. `result_digest` is `sha256:<lowercase-hex>` over the exact UTF-8 bytes of `result.body`, not over JSON or the full bundle; Worker, coordinator, and result API must verify that same byte sequence. `begin_checkpoint` validates the opaque token, active completed task, schema limits, and that `staging_ref` resolves under `GA_RUNTIME_DIR`; it writes a token-scoped temporary file, flushes and `os.fsync`s it, atomically renames it to the ready staging ref, fsyncs the staging directory, and returns `CheckpointReady` with token, ref, bundle checksum, and result digest. `start_session` accepts only the same schema version and supplied snapshot checksum. A configured byte limit must fail visibly before writing an oversized bundle.

The bundle also stores bounded `agent_history` from `agent.history`. Restore writes it back before the first task; `max_history_bytes` applies to the combined serialized `backend_history` and `agent_history`.

- [x] **Step 6: Implement the gRPC servicer and entrypoint**

`rpc_server.py` must use generated bindings and delegate every Worker RPC, including `BeginCheckpoint`, to `ManagedAgentAdapter`. `entrypoint.py` must require `GA_CONFIG_ROOT`, `GA_LEGACY_ROOT`, `GA_RUNTIME_DIR`, `GA_POLICY_FILE`, and a Unix/TCP listen address; missing values or an invalid policy registry are startup errors. It loads the registry before importing the legacy runtime, then injects the same immutable object into every adapter operation. It must not read PostgreSQL or call Podman. Graceful shutdown first requests cooperative cancellation, waits the configured grace period, and reports a visible timeout if a blocked call remains; Worker Manager owns eventual destruction in the production follow-up.

- [x] **Step 7: Run the unit tests to verify the green state**

Run:

```bash
cd tenant_platform/worker-python
python -m pytest tests/unit/test_managed_agent.py -q
```

Expected: PASS with coverage for success, cancellation before runner start, cancellation during execution, duplicate task rejection, snapshot restore/persona isolation, runtime overlay writes, policy limits, checkpoint isolation, and failure transitions.

- [x] **Step 8: Add the real GenericAgent integration smoke test**

Create a temporary `GA_CONFIG_ROOT` containing a test-only `mykey.py` whose `native_oai_config` points to a local deterministic OpenAI-compatible HTTP server with `stream=False`. Start that server inside the test process; it must return one valid non-stream chat completion and assert the bearer token is the test token. Launch the Worker entrypoint as a subprocess with that directory as `GA_CONFIG_ROOT`, the repository root as `GA_LEGACY_ROOT`, a temporary `GA_RUNTIME_DIR`, and the checked-in policy manifest as `GA_POLICY_FILE`. Drive `Health`, `StartSession` with an empty and a committed snapshot, `ExecuteTask` with two different personas and `tool_policy_version="foundation.no-host-tools.v1"`, pre-start and mid-run `CancelTask`, `BeginCheckpoint`, and graceful `Shutdown` through gRPC. Assert restored history/working, schema/checksum/policy-digest rejection, no writes under the legacy root, policy-limit errors, exactly one terminal display event per task, no checkpoint/tool-progress fabrication, a checksum-valid ready bundle, and no real key in captured output.

Run:

```bash
cd tenant_platform/worker-python
python -m pytest tests/integration/test_worker_rpc.py -q
```

Expected: PASS. If the local fixture cannot start or the required test dependencies are missing, fail explicitly; do not skip or use a real upstream model.

---

### Task 4: Implement the Go Worker client and loopback harness

**Files:**
- Create: `tenant_platform/backend-go/internal/workerclient/client.go`
- Create: `tenant_platform/backend-go/internal/workerclient/events.go`
- Create: `tenant_platform/backend-go/internal/workerclient/client_test.go`
- Create: `tenant_platform/backend-go/internal/workerclient/testserver_test.go`
- Create: `tenant_platform/backend-go/cmd/worker-loopback/main.go`

**Interfaces:**

```go
type WorkerClient interface {
    StartSession(ctx context.Context, req *workerv1.StartSessionRequest) (*workerv1.StartSessionResponse, error)
    ExecuteTask(ctx context.Context, req *workerv1.ExecuteTaskRequest) (<-chan WorkerEvent, <-chan error)
    BeginCheckpoint(ctx context.Context, req *workerv1.BeginCheckpointRequest) (*workerv1.CheckpointReady, error)
    CancelTask(ctx context.Context, taskID string) error
    Health(ctx context.Context) (*workerv1.HealthResponse, error)
    Shutdown(ctx context.Context, reason string) error
}
```

`WorkerEvent` must be a typed Go wrapper that distinguishes chunk, terminal, and transport error without exposing generated oneof handling to the scheduler. `CheckpointReady` is returned only by the separate unary `BeginCheckpoint` method.

- [x] **Step 1: Write Go client tests against an in-process gRPC server**

Cover streaming order, context cancellation, malformed terminal events, transport disconnect, and token-preserving `BeginCheckpoint`. Assert that a terminal event closes the event channel exactly once and no checkpoint payload can appear on that channel.

- [x] **Step 2: Run the Go tests and verify the red state**

Run:

```bash
cd tenant_platform/backend-go
go test ./internal/workerclient -run TestWorkerClient -v
```

Expected: FAIL because the client and typed event wrapper do not exist.

- [x] **Step 3: Implement the client and event conversion**

Use one gRPC connection per Worker process, bounded receive contexts, explicit conversion errors for unknown display oneof values, and a bounded unary checkpoint deadline. Do not put retry loops in the client; retry policy belongs to the manager/scheduler layer.

- [x] **Step 4: Implement the loopback command**

`cmd/worker-loopback` starts a local Python Worker subprocess only for development smoke tests, waits for `Health.ready`, executes one task using `capability_version="foundation.v1"`, `tool_policy_version="foundation.no-host-tools.v1"`, and the loaded policy digest, requests `BeginCheckpoint` with a temporary token-scoped staging ref, prints a JSON summary with task ID/status/checksum, then sends `Shutdown`. It must require `GA_CONFIG_ROOT`, `GA_LEGACY_ROOT`, `GA_RUNTIME_DIR`, and `GA_POLICY_FILE`, and bind to loopback only.

- [x] **Step 5: Run the tests and loopback smoke**

Run:

```bash
cd tenant_platform/backend-go
go test ./...
go run ./cmd/worker-loopback
```

Expected: all Go tests pass and the smoke command prints one succeeded terminal status plus a checkpoint checksum. This command is a development-only harness, not a production Worker process.

---

### Task 5: Add the minimal Go platform task store and PostgreSQL vertical path

**Files:**
- Create: `tenant_platform/infra/postgres/migrations/0001_foundation.sql`
- Create: `tenant_platform/backend-go/internal/domain/task.go`
- Create: `tenant_platform/backend-go/internal/postgres/store.go`
- Create: `tenant_platform/backend-go/internal/postgres/migrations.go`
- Create: `tenant_platform/backend-go/internal/checkpoint/coordinator.go`
- Create: `tenant_platform/backend-go/internal/checkpoint/local.go`
- Create: `tenant_platform/backend-go/internal/checkpoint/local_test.go`
- Create: `tenant_platform/backend-go/internal/application/task_service.go`
- Create: `tenant_platform/backend-go/internal/api/http.go`
- Create: `tenant_platform/backend-go/cmd/platform/main.go`
- Create: `tenant_platform/backend-go/internal/postgres/store_test.go`
- Create: `tenant_platform/backend-go/internal/application/task_service_test.go`
- Create: `tenant_platform/tests/integration/test_foundation_flow.py`
- Create: `tenant_platform/backend-go/internal/application/dev_bootstrap.go`
- Create: `tenant_platform/backend-go/internal/application/dev_bootstrap_test.go`
- Create: `tenant_platform/backend-go/internal/application/instance_id.go`
- Create: `tenant_platform/backend-go/internal/application/instance_id_test.go`
- Create: `tenant_platform/backend-go/internal/domain/delivery.go`
- Create: `tenant_platform/backend-go/internal/application/scheduler.go`
- Create: `tenant_platform/backend-go/internal/application/scheduler_test.go`
- Create: `tenant_platform/backend-go/internal/policy/registry.go`
- Create: `tenant_platform/backend-go/internal/policy/registry_test.go`

**Interfaces:**

```go
type SubmitTaskCommand struct {
    SessionKey        string
    RequesterUserID   int64
    Source            string
    SourceInstanceID  string
    MessageID         string
    Prompt            string
    PersonaSnapshot   []string
    ToolPolicyVersion string
}

type TaskStatus string

const (
    TaskQueued      TaskStatus = "queued"
    TaskStarting    TaskStatus = "starting"
    TaskRunning     TaskStatus = "running"
    TaskSucceeded   TaskStatus = "succeeded"
    TaskFailed      TaskStatus = "failed"
    TaskCancelled   TaskStatus = "cancelled"
    TaskInterrupted TaskStatus = "interrupted"
)

type Task struct {
    ID                    string
    SessionKey            string
    WorkspaceID           string
    RequesterID           int64
    Source                string
    SourceInstanceID      string
    MessageID             string
    MessageIdempotencyKey string
    Prompt                string
    PersonaSnapshot       []string
    ToolPolicyVersion     string
    ClaimOwner            string
    ClaimLeaseUntil       time.Time
    SessionSequence       int64
    Status                TaskStatus
    SnapshotID            string
    SnapshotChecksum      string
    ResultRef             string
    ResultDigest          string
}

type ResultPayload struct {
    Ref    string
    Digest string
    Body   []byte
}

type DeliveryType string

const (
    DeliveryTaskComplete    DeliveryType = "task_complete"
    DeliveryTaskFailed      DeliveryType = "task_failed"
    DeliveryTaskCancelled   DeliveryType = "task_cancelled"
    DeliveryTaskInterrupted DeliveryType = "task_interrupted"
)

func StableDeliveryID(taskID string, deliveryType DeliveryType) string {
    return taskID + ":" + string(deliveryType)
}

type TaskService interface {
    SubmitTask(ctx context.Context, cmd SubmitTaskCommand) (Task, error)
    GetTask(ctx context.Context, taskID string) (Task, error)
    CancelTask(ctx context.Context, taskID string, requesterUserID int64) (Task, error)
    ClaimNextTask(ctx context.Context, sessionKey, platformInstanceID string) (Task, bool, error)
    RecoverAfterRestart(ctx context.Context, platformInstanceID string) error
    ReadResult(ctx context.Context, taskID string) (ResultPayload, error)
}

type Scheduler interface {
    Run(ctx context.Context) error
    KickSession(ctx context.Context, sessionKey string) error
    Recover(ctx context.Context, platformInstanceID string) error
}
```

`instance_id.go` must expose `NewPlatformInstanceID() (string, error)`, using `crypto/rand` to create a lowercase RFC 4122 UUID and returning an error if system randomness is unavailable. The exported function delegates to an unexported reader-injected helper so tests can exercise entropy failure. `cmd/platform` calls it exactly once before opening PostgreSQL and has no empty-ID fallback.

`SchedulerConfig` carries `PlatformInstanceID string` and `ClaimLease time.Duration`; the scheduler rejects an empty ID or non-positive lease. Every claim, heartbeat, recovery scan, and Worker dispatch uses the same ID for that process lifetime.

`internal/policy` must expose the same immutable registry to API validation and scheduler RPC construction:

```go
type ToolPolicy struct {
    Version      string
    AllowedTools []string
}

type Registry interface {
    Digest() string
    Resolve(capabilityVersion, toolPolicyVersion string) (ToolPolicy, error)
}

func LoadRegistry(path string) (Registry, error)
```

`LoadRegistry` parses only `genericagent.capability-policy.v1`, computes `sha256:<lowercase-hex>` over the exact file bytes, rejects duplicate/empty names and empty allowlists, and has no compiled fallback. `cmd/platform` loads it once from explicit `--policy-file`; API and scheduler receive the same instance.

`SubmitTask` maps the validated carrier `MessageID` to the stored `message_idempotency_key`; callers cannot provide a second competing dedupe value. `SourceInstanceID` is the concrete bot/binding identifier, so dedupe is scoped to one transport instance rather than the coarse `wechat|web` source. `StableDeliveryID` is platform-owned and uses `task_id:task_complete`, `task_id:task_failed`, `task_id:task_cancelled`, or `task_id:task_interrupted`; the Worker never receives or creates it.

```go
type CheckpointPrepareRequest struct {
    TaskID         string
    WorkspaceID    string
    SessionKey     string
    MaxBundleBytes uint64
}

type CheckpointLease struct {
    SnapshotID     string
    Token          string
    StagingRef     string
    MaxBundleBytes uint64
}

type ReadyCheckpoint struct {
    TaskID          string
    SnapshotID      string
    CheckpointToken string
    StagingRef      string
    Checksum        string
    ResultDigest    string
}

type CommittedCheckpoint struct {
    SnapshotID   string
    FileRef      string
    Checksum     string
    ResultRef    string
    ResultDigest string
}

type CheckpointCoordinator interface {
    Prepare(ctx context.Context, request CheckpointPrepareRequest) (CheckpointLease, error)
    Commit(ctx context.Context, ready ReadyCheckpoint) (CommittedCheckpoint, error)
    ReadResult(ctx context.Context, ref string, expectedDigest string) (ResultPayload, error)
}
```

`LocalCheckpointCoordinator` is wired only when `cmd/platform` starts with explicit `--dev-loopback`. `Prepare` creates the `workspace_snapshots(state=writing)` row and generation-bearing opaque token, then returns a token-scoped ref under the configured development runtime root. `Commit` rejects mismatched/expired tokens, verifies staging ref, checksum, schema, and size, performs the atomic immutable rename/fsync, and returns an opaque `result_ref` for the bounded `result` member inside the committed bundle. `ReadResult` resolves only that opaque ref, reads the result bytes, and verifies `expectedDigest`. The normal platform startup path must refuse this coordinator; production lease renewal and `CommitCheckpoint` manager RPC remain in the worker-manager follow-up plan.

- [x] **Step 1: Write migration and transaction tests first**

The Go store tests and Python end-to-end test must require `TEST_DATABASE_URL`; absence is a visible prerequisite failure.

Create `tenant_platform/tests/integration/test_foundation_flow.py` with real PostgreSQL and subprocess scenarios: submit a normal task and poll it to `succeeded`, submit a second task and cancel it before completion, restart the platform with a pre-existing queued row plus expired prior-owner work, and preserve an unexpired row owned by a different live instance. Every scenario uses `TEST_DATABASE_URL`, explicit `--dev-loopback`, and a positive test claim lease. Cover:

- development bootstrap creates an approved user and personal workspace only under `--dev-loopback`, and normal startup rejects it. The minimal `users` row has `id BIGINT PRIMARY KEY`, unique non-empty `username`, `status` constrained to `approved|pending|blocked`, nullable unique `bootstrap_marker`, `created_at`, and nullable `approved_at`; the minimal `workspaces` row has `id UUID PRIMARY KEY`, unique non-empty `session_key`, `owner_user_id BIGINT REFERENCES users(id)`, `kind='personal'`, nullable `team_id UUID`, nullable `volume_id`, nullable unique `bootstrap_marker`, nullable `current_snapshot_id`, and `created_at`; foundation constraints require a personal workspace to have no team, require `bootstrap_marker='dev-loopback'` when `volume_id` is null, and allow a null volume only for that loopback marker.
- `NewPlatformInstanceID` produces lowercase RFC 4122 UUIDs, two process starts receive different IDs, an injected failing randomness reader returns an error, and startup never opens PostgreSQL with an empty ID.
- policy registry tests load the checked-in manifest, assert the exact digest is sent in `RuntimePolicy`, reject unknown/cross-capability policies and malformed registries, and prove HTTP submit cannot select a host-tool policy.
- `tasks` unique `(source, source_instance_id, message_idempotency_key)`; the same message ID under two different source instances creates two tasks.
- duplicate submit with a different prompt/requester returns the original task and leaves every business column unchanged.
- `ClaimNextTask` round-trips the durable `prompt`, `persona_snapshot`, and `tool_policy_version` fields into the Worker RPC after a process restart; it never reconstructs them from scheduler memory.
- two queued tasks for one session are claimed in `session_sequence` order; a concurrent claim cannot take the same row or violate the one-running-task index.
- restart uses a new `platform_instance_id`; expired `starting/running` rows whose `claim_owner` belongs to a prior instance become `interrupted` with one delivery, current-instance rows are untouched, and rows owned by a different live instance with an unexpired lease remain untouched until that lease expires. Queued rows remain claimable. The test pre-seeds both an expired prior-owner row and an unexpired foreign-owner row.
- cancelling a still-queued row and a claimed-but-not-dispatched `starting` row performs zero Worker RPCs, writes one `task_cancelled` delivery, and leaves no claimable terminal row; a dispatch race is covered separately.
- `task_deliveries` unique `(task_id, delivery_type)`, stable IDs, bounded payload refs/digests, and terminal type checks.
- `workspace_snapshots` state values, schema/generation/checksum, writing lease, current pointer updates, and token mismatch rejection.
- successful terminal state is not committed until `Prepare`, Worker `BeginCheckpoint`, and coordinator `Commit` all succeed.
- `GET /v1/tasks/{task_id}/result` returns bounded bytes whose digest matches `result_digest`; arbitrary host paths are rejected.
- terminal transition, snapshot pointer, result ref, and pending delivery are written in one PostgreSQL transaction.
- `task_events` records transition/chunk sequence, byte count, and digest only; it never persists full prompt or chunk text.

- [x] **Step 2: Run the integration tests to verify the red state**

Run:

```bash
export TEST_DATABASE_URL=postgresql://genericagent_test:genericagent_test@127.0.0.1:5432/genericagent_test?sslmode=disable
python -m pytest tenant_platform/tests/integration/test_foundation_flow.py -q
cd tenant_platform/backend-go
go test ./internal/policy ./internal/postgres ./internal/application ./internal/api -v
```

Expected: FAIL because the migration, store, and services do not exist. If `TEST_DATABASE_URL` is absent, the test must exit with a clear prerequisite error rather than selecting SQLite.

- [x] **Step 3: Write the foundation migration**

Create `0001_foundation.sql` with PostgreSQL tables and constraints for `users`, `workspaces`, `tasks`, `task_events`, `task_deliveries`, and `workspace_snapshots`. Define `users` with `id BIGINT PRIMARY KEY`, unique non-empty `username`, `status TEXT NOT NULL CHECK (status IN ('approved', 'pending', 'blocked'))`, nullable unique `bootstrap_marker` with an allowed value of `dev-loopback`, `created_at`, and nullable `approved_at`. Define `workspaces` with `id UUID PRIMARY KEY`, unique non-empty `session_key`, `owner_user_id BIGINT NOT NULL REFERENCES users(id)`, `kind TEXT NOT NULL CHECK (kind = 'personal')` for this foundation slice, nullable `team_id UUID`, nullable `volume_id`, nullable unique `bootstrap_marker` with an allowed value of `dev-loopback`, nullable `current_snapshot_id`, and `created_at`; add checks that personal rows have no team and that `volume_id IS NULL` is allowed only when `bootstrap_marker='dev-loopback'`. `tasks` stores `workspace_id`, `session_sequence`, requester, idempotency fields, `prompt`, `persona_snapshot`, `tool_policy_version`, validated prompt/persona byte counts, `claim_owner`, `claimed_at`, `claim_lease_until`, `worker_instance_id`, `cancel_requested_at`, `worker_dispatch_started_at`, and lifecycle timestamps; `starting/running` rows require a non-empty `claim_owner` and non-null `claim_lease_until`, while queued rows have no claim owner or lease. `workspace_snapshots` references workspace/task and carries `schema_version`, `state` in `writing/committed/quarantined`, lease owner/until, generation, checksum, opaque file/result refs, and bounded result metadata; `task_deliveries` carries the stable delivery ID, one of the four delivery types, payload ref/digest or bounded error payload, and retry/ack state. Include:

```sql
CREATE UNIQUE INDEX tasks_message_dedupe
ON tasks (source, source_instance_id, message_idempotency_key);

CREATE UNIQUE INDEX tasks_session_order
ON tasks (session_key, session_sequence);

CREATE UNIQUE INDEX task_deliveries_task_type
ON task_deliveries (task_id, delivery_type);

CREATE UNIQUE INDEX task_deliveries_delivery_id
ON task_deliveries (delivery_id);

CREATE UNIQUE INDEX one_running_task_per_session
ON tasks (session_key)
WHERE status IN ('starting', 'running');
```

Use `CHECK` constraints for task/delivery/snapshot states, delivery types, bounded byte sizes, non-empty stable IDs, and claim ownership/lease invariants; use UTC timestamps and foreign keys, adding the nullable `workspaces.current_snapshot_id` foreign key after `workspace_snapshots` exists. Validate prompt, persona, policy-version, terminal-error, and claim-lease future-time limits in application code before insert/update; database checks enforce non-null ownership/lease fields but do not use volatile `now()` checks. `EnsureDevelopmentContext` is the only foundation bootstrap mutation: inside `--dev-loopback` it inserts the approved development user/workspace or updates only a row already marked `bootstrap_marker='dev-loopback'`; if the requested ID exists with another marker or a non-approved status, it fails visibly without promotion. Normal startup rejects this bootstrap path. Do not store result payloads or terminal errors beyond configured byte limits, and never put prompt or chunk text in `task_events`.

- [x] **Step 4: Implement the PostgreSQL store**

Use `pgxpool.Pool`. Every mutating method accepts a context and runs in an explicit transaction. `EnsureDevelopmentContext` is the only foundation bootstrap mutation and is gated by the command flag. `SubmitTask` locks the workspace/session row to allocate the next `session_sequence`, validates and stores the complete prompt/persona/policy envelope, inserts `queued` with `ON CONFLICT (source, source_instance_id, message_idempotency_key) DO NOTHING RETURNING`, and, when no row is returned, selects the existing unique-key row `FOR UPDATE` and returns it unchanged. `ClaimNextTask` receives the current `platform_instance_id`, locks the oldest queued row with `FOR UPDATE SKIP LOCKED` only when the session has no `starting/running` row, then advances it to `starting` with `claim_owner=platform_instance_id` and `claim_lease_until=now()+configured_lease`; it returns all durable envelope fields with `worker_instance_id` still empty. After the scheduler starts or reuses the Worker, a transaction records its actual `worker_instance_id` and `worker_dispatch_started_at` before issuing the Worker RPC, then advances to `running` only after Worker session/task acceptance. The scheduler heartbeats the lease while dispatching/running. `RecoverAfterRestart(platform_instance_id)` locks only `starting/running` rows with a different owner and expired lease, transactionally converts them to `interrupted`, creates the stable `task_interrupted` delivery, and leaves current-owner, unexpired foreign-owner, and queued rows untouched. `PrepareCheckpoint` creates writing metadata and a generation token without advancing the current pointer. After `CheckpointCoordinator.Commit` returns an immutable ref, `CompleteTask` updates snapshot state, current pointer, task result fields, and the success `task_complete` delivery payload (`result_ref`, `result_digest`) atomically. `ReadResult` resolves only an opaque ref and verifies its digest.

- [x] **Step 5: Implement the task service and loopback Worker integration**

The service persists every request before scheduling. `cmd/platform` generates one `platform_instance_id`, validates a positive claim lease, constructs `SchedulerConfig`, and calls recovery before accepting HTTP traffic. A single P0 scheduler loop implements `Run`, periodically scans PostgreSQL for queued and newly expired prior-owner work, heartbeats current claims, and is also kicked after submit, terminal completion, cancellation, and recovery; PostgreSQL remains the fact source, while notifications are only wakeups. It claims one session FIFO row, starts/reuses the real Worker with the same explicit `GA_POLICY_FILE`, records bounded display event metadata, resolves the task's durable `tool_policy_version` under `capability_version="foundation.v1"`, and passes `snapshot_ref`, `snapshot_id`, `snapshot_checksum`, `RuntimePolicy` including the registry digest, persona, and tool policy into the RPC. On a successful terminal signal it performs `Prepare → Worker.BeginCheckpoint → Commit`, verifies the terminal/result digest, stores the opaque result ref, and only then commits `succeeded` plus `task_complete` delivery. Failure/cancel/interrupted paths skip success checkpointing and persist their corresponding stable delivery type and bounded error payload in the terminal transaction. After every terminal transition it kicks the next queued row. Unit tests use an in-process gRPC test service and a real temporary-filesystem coordinator; the foundation smoke uses the real Python Worker subprocess and `LocalCheckpointCoordinator` under explicit `--dev-loopback`. No production code path may make platform read an arbitrary Worker path.

`CancelTask` first locks the task row and makes a durable decision. A `queued` task becomes `cancelled` and gets its stable bounded `task_cancelled` delivery without contacting Worker. A `starting` task with no `worker_dispatch_started_at` is also committed as `cancelled` with zero Worker RPCs; the scheduler's dispatch transaction must re-check the row and stop. Once `worker_dispatch_started_at` is set, or for `running`, the transaction records `cancel_requested_at` exactly once; the scheduler then issues an idempotent Worker cancel RPC once, and the Worker pre-start gate maps a not-yet-running task to `TASK_CANCELLED` while started work maps to `TASK_INTERRUPTED`. The platform commits the matching terminal state and stable delivery without checkpointing. Unique delivery IDs and the locked state transition make retries and dispatch races side-effect free; cancelled tasks never re-enter the claim path, and every cancellation kicks the next queued session task.

- [x] **Step 6: Implement the loopback-only HTTP API**

Use Go `net/http` with these handlers:

```text
GET  /healthz
POST /v1/sessions/{session_key}/tasks
GET  /v1/tasks/{task_id}
GET  /v1/tasks/{task_id}/result
POST /v1/tasks/{task_id}/cancel
```

Require `cmd/platform --policy-file <path> --claim-lease <positive-duration>` and fail startup if the policy manifest is missing/malformed, the lease is non-positive, or platform instance ID generation fails. Require `X-Platform-Dev-Token`, bind to `127.0.0.1`, derive `requester_user_id` from the bootstrapped development context rather than request JSON, require and validate `source`, `source_instance_id`, `message_id`, prompt limits, `persona_snapshot`, and `tool_policy_version` against the loaded `foundation.v1` capability, and return JSON errors with stable `code`, `message`, and `trace_id`. `POST` returns `202` for a durable `queued` task; `GET /result` reads through `TaskService.ReadResult`, returns only the bounded committed payload with its digest, and rejects path-like refs. Do not add production user authentication in this foundation plan.

- [x] **Step 7: Run the red-green test cycle**

Run:

```bash
python -m pytest tenant_platform/tests/integration/test_foundation_flow.py -q
cd tenant_platform/backend-go
go test ./internal/policy ./internal/postgres ./internal/application ./internal/api -v
```

Expected: both commands pass against PostgreSQL; the end-to-end scenario proves durable queued FIFO, claim/recovery, real Worker execution, `Prepare → BeginCheckpoint → Commit`, digest-checked result readback, one terminal delivery, and no task rerun after restart.

---

### Task 6: Verify legacy non-regression and hand off the next plan

**Files:**
- Create: `tenant_platform/tests/integration/test_legacy_imports.py`
- Create: `tenant_platform/tests/smoke/foundation_smoke.py`
- Review only: `agentmain.py`, `ga.py`, `llmcore.py`, `frontends/`

**Interfaces:**
- Confirms the new boundary does not change legacy imports or desktop/IM startup contracts.

- [x] **Step 1: Write the import regression test**

The test must import `agentmain`, `ga`, `llmcore`, `frontends.chatapp_common`, and `frontends.ga_contract_probe` in a clean subprocess and assert exit code 0. It must not import any real key value into test output.

- [x] **Step 2: Run the regression test**

Run:

```bash
python -m pytest tenant_platform/tests/integration/test_legacy_imports.py -q
```

Expected: PASS. Any failure is a boundary regression and must be fixed without moving or rewriting the legacy files.

- [x] **Step 3: Implement the foundation smoke script**

`foundation_smoke.py` must require `TEST_DATABASE_URL`, `PLATFORM_DEV_USER_ID`, `GA_LEGACY_ROOT`, and `GA_POLICY_FILE`; create a temporary `GA_CONFIG_ROOT`/`GA_RUNTIME_DIR` and test-only `mykey.py`; and start a deterministic local OpenAI-compatible fixture that validates the test bearer token. It starts the loopback-only Go platform with `--dev-loopback --policy-file "$GA_POLICY_FILE" --claim-lease 5s`, passes `X-Platform-Dev-Token`, waits for `/healthz`, and submits a task to `personal:<PLATFORM_DEV_USER_ID>` with a unique source instance/message ID, explicit `persona_snapshot`, and `tool_policy_version="foundation.no-host-tools.v1"`. It resubmits the same key and asserts the same task ID, polls until `succeeded`, reads `GET /v1/tasks/{task_id}/result`, and verifies returned bytes against `result_digest`. It then submits a second task, cancels it, and asserts a cancellation terminal with no checkpoint/result ref. It must terminate every child process in a `finally` block and print only task IDs, statuses, result/checkpoint digests, and elapsed milliseconds.

- [x] **Step 4: Run the complete foundation verification**

Run:

```bash
python -m pytest tenant_platform/tests -q
cd tenant_platform/worker-python
python -m pytest tests -q
cd ../backend-go
go test ./...
```

Expected: all tests pass; no test silently skips due to missing PostgreSQL or local fixture configuration.

- [x] **Step 5: Run the foundation smoke command**

Run:

```bash
python tenant_platform/tests/smoke/foundation_smoke.py
```

The command must exercise health, submit, the internal Worker display stream, separate `BeginCheckpoint`, terminal persistence/read-back, cancellation, and shutdown, then print a bounded JSON summary without prompt contents or credentials.

- [x] **Step 6: Stop and review before Podman/Web work**

Record the observed Worker RSS, checkpoint size/time, task latency, and failure behavior. Use those measurements to write separate follow-up plans for:

- `tenant_platform` rootless Podman worker-manager and host capacity.
- LLM Proxy capability/security hardening.
- Go iLink BotTransportAdapter and QR-based binding (iLink official `confirmed` flow, no `/activate`; see [iLink binding SPEC](../specs/2026-07-25-ilink-official-binding-flow.md)).
- React Web P0 registration/approval/binding/status UI.
- Structured `ToolProgress` mapping once the legacy runtime exposes a non-text progress signal; foundation must not infer it from chunk text.

Do not begin GA Go/Rust replacement from this plan. That decision requires a separate benchmark and compatibility plan.

## Plan self-review checklist

- Contracts are defined before either language implementation.
- The Worker owns no PostgreSQL access and the platform owns all task/snapshot/delivery transactions.
- Slice 2 loopback is explicitly development-only until the LLM Proxy path exists.
- P0 Web has no chat entry; iLink remains the user message transport.
- Existing root modules and `frontends/` are not modified in the foundation slice.
- Checkpoint readiness is a separate unary RPC; it never enters the Worker display stream or competes with the single stream consumer.
- PostgreSQL-backed FIFO claim/recovery, side-effect-free duplicate submit, stable delivery IDs, and digest-checked result readback are covered before Podman work.
- Every new behavior has a failing test, a focused command, and a smoke/acceptance condition; Task 6's pre-existing import contract is verified as a regression instead of being introduced red.
- Podman, iLink, React, and GA rewrite are separate follow-up scopes rather than unverified implementations in this plan.
