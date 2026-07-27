# GA Transparent LLM Credential Proxy Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace the duplicated plaintext/provider protocol paths with one provider-bound, token-only runtime configuration and a transparent streaming credential proxy while leaving GA Core unchanged.

**Architecture:** Platform stores validated session and transport configuration, signs provider-bound JWTs, and writes structured session-scoped runtime configuration. The Worker manages lifecycle and credential reload only; unmodified GA produces native OpenAI/Anthropic requests. LLM Proxy validates claims, resolves the bound Provider by ID, injects the real credential, and transparently streams the native request and response.

**Tech Stack:** Go 1.22, PostgreSQL/pgx, `github.com/golang-jwt/jwt/v5`, Go `net/http/httputil.ReverseProxy`, protobuf/gRPC, Python 3.10-3.13, React/TypeScript/Vite, pytest, Go testing/httptest.

## Global Constraints

- GA Core files `agentmain.py`, `ga.py`, `llmcore.py`, `agent_loop.py`, `simphtml.py`, `plugins/*`, and `assets/*` MUST NOT be modified.
- Worker runtime, config files, environment, logs, and events MUST never contain a real upstream API key.
- Clean cutover only: remove the plaintext config endpoint, old generator, old provider enums, and old revoke HTTP path; no compatibility aliases or feature flags.
- GA remains the sole owner of payload generation, protocol headers, streaming parsers, retries, mixin failover, history, tools, and Agent execution.
- Proxy MUST NOT translate OpenAI/Anthropic payloads, buffer successful responses, or retry POST requests.
- Supported native paths are `POST /v1/chat/completions`, `POST /v1/responses`, and `POST /v1/messages`.
- JWT validation MUST pin HS256 and validate `typ`, issuer, audience, subject, time claims, JTI, Provider ID, revision, type, and model.
- Provider API key rotation MUST NOT increment routing revision; every other routing/session/transport/state change MUST increment revision.
- Optional numeric configuration MUST preserve explicit zero values; TypeScript production code MUST NOT use `any`.
- Streaming HTTP servers MUST NOT use a finite `WriteTimeout` that truncates SSE.
- Unit tests have a 60-second hard timeout. PostgreSQL integration tests require `TEST_DATABASE_URL` and MUST fail visibly when it is absent.
- Work with the existing dirty workspace; do not revert user changes or the four untracked runtime result artifacts.
- Each task stages and commits only its listed files with `git commit --only`.

## Planned File Structure

### Domain and persistence

- `tenant_platform/backend-go/internal/domain/llm_provider.go`: provider types, session/transport configs, mutation inputs, validation ownership.
- `tenant_platform/infra/postgres/migrations/0024_transparent_llm_proxy.sql`: clean schema migration and persistent revocation table.
- `tenant_platform/backend-go/internal/infrastructure/postgres/llm_provider_store.go`: Provider CRUD by structured inputs and active routing order.
- `tenant_platform/backend-go/internal/infrastructure/postgres/llm_capability_store.go`: hashed JTI revocation persistence.

### Credentials and Worker runtime

- `tenant_platform/backend-go/internal/infrastructure/llmproxy/token.go`: standards-based JWT issuer/validator.
- `tenant_platform/backend-go/internal/application/runtime_config.go`: structured JSON and fixed `mykey.py` loader generation.
- `tenant_platform/backend-go/internal/application/worker_credential.go`: routing snapshot issuance, rotation, and revocation orchestration.
- `tenant_platform/worker-python/src/ga_worker/credential_config.py`: runtime metadata validation and GA reload.
- `tenant_platform/contracts/proto/genericagent/worker/v1/worker.proto`: `ReloadCredentials` unary RPC.
- `tenant_platform/contracts/proto/generate_bindings.py`: reproducible Go/Python binding generation.

### Transparent proxy

- `tenant_platform/backend-go/internal/infrastructure/llmproxy/target.go`: GA-compatible target URL resolution and path policy.
- `tenant_platform/backend-go/internal/infrastructure/llmproxy/headers.go`: protocol header allowlists and credential injection.
- `tenant_platform/backend-go/internal/infrastructure/llmproxy/transport.go`: SSRF-safe cached transports.
- `tenant_platform/backend-go/internal/infrastructure/llmproxy/handler.go`: claim/body/provider validation and ReverseProxy dispatch.
- `tenant_platform/backend-go/internal/infrastructure/llmproxy/server.go`: routes, reverse proxy, error sanitizer, streaming behavior.

### Admin and UI

- `tenant_platform/backend-go/internal/api/llm_provider.go`: nested config API, validation, optional key rotation.
- `tenant_platform/contracts/openapi/platform.yaml`: canonical provider schema.
- `tenant_platform/web/src/api/types.ts`: exact TypeScript config types.
- `tenant_platform/web/src/api/providers.ts`: create/update/default/delete API.
- `tenant_platform/web/src/features/admin/LLMProvidersPage.tsx`: create/edit form with real supported fields.

---

### Task 1: Split Provider Configuration and Migrate Persistence

**Files:**
- Modify: `tenant_platform/backend-go/internal/domain/llm_provider.go`
- Create: `tenant_platform/infra/postgres/migrations/0024_transparent_llm_proxy.sql`
- Modify: `tenant_platform/backend-go/internal/infrastructure/postgres/migrations.go`
- Modify: `tenant_platform/backend-go/internal/infrastructure/postgres/llm_provider_store.go`
- Create: `tenant_platform/backend-go/internal/infrastructure/postgres/llm_provider_store_test.go`
- Modify: `tenant_platform/backend-go/internal/api/http.go`
- Modify: `tenant_platform/backend-go/internal/api/llm_provider.go`
- Modify: `tenant_platform/backend-go/cmd/platform/main.go`

**Interfaces:**
- Produces: `domain.GASessionConfig`, `domain.ProviderTransportConfig`, `domain.LLMProviderCreate`, `domain.LLMProviderUpdate`, `domain.LLMProvider.Revision`.
- Produces: `Store.ListActiveProviders(context.Context) ([]domain.LLMProvider, error)` ordered default-first then ID.
- Produces: `Store.GetProvider(context.Context, int64) (domain.LLMProvider, error)` with both config structs and revision.
- Consumes: existing `secret.TokenCipher` ciphertext/version values unchanged.

- [ ] **Step 1: Add failing domain JSON and validation tests**

Create table-driven tests proving explicit zero survives JSON and invalid protocol combinations fail:

```go
func TestGASessionConfigPreservesExplicitZero(t *testing.T) {
    zeroInt, zeroFloat := 0, 0.0
    input := GASessionConfig{MaxRetries: &zeroInt, Temperature: &zeroFloat}
    raw, err := json.Marshal(input)
    if err != nil { t.Fatal(err) }
    var got GASessionConfig
    if err := json.Unmarshal(raw, &got); err != nil { t.Fatal(err) }
    if got.MaxRetries == nil || *got.MaxRetries != 0 { t.Fatalf("max_retries=%v", got.MaxRetries) }
    if got.Temperature == nil || *got.Temperature != 0 { t.Fatalf("temperature=%v", got.Temperature) }
}

func TestProviderConfigRejectsResponsesForClaude(t *testing.T) {
    mode := "responses"
    cfg := GASessionConfig{APIMode: &mode}
    if err := cfg.Validate(ProviderNativeClaude); err == nil {
        t.Fatal("expected protocol-specific validation error")
    }
}
```

Place pure domain tests in `internal/domain/llm_provider_test.go` if that file is absent.

- [ ] **Step 2: Run the focused tests and observe RED**

Run:

```bash
cd tenant_platform/backend-go
go test ./internal/domain -run 'TestGASessionConfig|TestProviderConfig' -count=1
```

Expected: compile failure because `GASessionConfig` and pointer fields do not exist.

- [ ] **Step 3: Implement the domain types and validators**

Use config objects, not long parameter lists:

```go
type ProviderAuthMode string

const (
    ProviderAuthAuto    ProviderAuthMode = "auto"
    ProviderAuthBearer  ProviderAuthMode = "bearer"
    ProviderAuthXAPIKey ProviderAuthMode = "x_api_key"
)

type GASessionConfig struct {
    ThinkingType        *string  `json:"thinking_type,omitempty"`
    ThinkingBudgetTokens *int    `json:"thinking_budget_tokens,omitempty"`
    ReasoningEffort     *string  `json:"reasoning_effort,omitempty"`
    Temperature         *float64 `json:"temperature,omitempty"`
    MaxTokens           *int     `json:"max_tokens,omitempty"`
    ContextWin          *int     `json:"context_win,omitempty"`
    TrimKeepPrefix      *int     `json:"trim_keep_prefix,omitempty"`
    MaxRetries          *int     `json:"max_retries,omitempty"`
    ReadTimeout         *int     `json:"read_timeout,omitempty"`
    Stream              *bool    `json:"stream,omitempty"`
    APIMode             *string  `json:"api_mode,omitempty"`
    FakeCCSystemPrompt  *bool    `json:"fake_cc_system_prompt,omitempty"`
    UserAgent           *string  `json:"user_agent,omitempty"`
    ServiceTier         *string  `json:"service_tier,omitempty"`
    OmitThinking        *bool    `json:"omit_thinking,omitempty"`
    ExtraSysPrompt      *string  `json:"extra_sys_prompt,omitempty"`
}

type ProviderTransportConfig struct {
    AuthMode                     ProviderAuthMode `json:"auth_mode"`
    ProxyURL                     *string          `json:"proxy_url,omitempty"`
    TLSVerify                    *bool            `json:"tls_verify,omitempty"`
    ConnectTimeoutSeconds        *int             `json:"connect_timeout_seconds,omitempty"`
    ResponseHeaderTimeoutSeconds *int             `json:"response_header_timeout_seconds,omitempty"`
}

type LLMProvider struct {
    ID int64
    Name string
    ProviderType LLMProviderType
    BaseURL string
    Model string
    APIKeyCiphertext []byte
    APIKeyKeyVersion string
    APIKey string
    SessionConfig GASessionConfig
    TransportConfig ProviderTransportConfig
    Revision int64
    IsDefault bool
    State string
    CreatedAt time.Time
    UpdatedAt time.Time
}
```

Implement `Validate` with explicit enum sets, ranges, `thinking_type=enabled` budget requirement, OAI-only `api_mode/service_tier`, Claude-only fake prompt behavior, and a maximum `extra_sys_prompt` byte length constant.

- [ ] **Step 4: Add the clean migration and migration registration**

`0024_transparent_llm_proxy.sql` must:

```sql
DO $$
DECLARE unknown_fields text;
BEGIN
  SELECT string_agg(DISTINCT key, ', ' ORDER BY key)
  INTO unknown_fields
  FROM llm_providers p,
       LATERAL jsonb_object_keys(p.config) AS key
  WHERE key NOT IN (
    'thinking_type','thinking_budget_tokens','reasoning_effort','temperature',
    'max_tokens','context_win','trim_keep_prefix','max_retries','read_timeout',
    'stream','api_mode','fake_cc_system_prompt','user_agent','service_tier',
    'omit_thinking','extra_sys_prompt','extra_sys_prompt_file','proxy','verify',
    'connect_timeout','timeout','top_p'
  );
  IF unknown_fields IS NOT NULL THEN
    RAISE EXCEPTION 'unknown llm provider config fields: %', unknown_fields;
  END IF;
END $$;

DO $$
DECLARE invalid_ids text;
BEGIN
  SELECT string_agg(id::text, ', ' ORDER BY id)
  INTO invalid_ids
  FROM llm_providers
  WHERE jsonb_typeof(config) <> 'object'
     OR COALESCE(config->>'thinking_type', '') NOT IN ('', 'adaptive', 'enabled', 'disabled')
     OR COALESCE(config->>'reasoning_effort', '') NOT IN ('', 'none', 'minimal', 'low', 'medium', 'high', 'xhigh', 'max')
     OR COALESCE(config->>'api_mode', '') NOT IN ('', 'chat_completions', 'responses')
     OR COALESCE(config->>'service_tier', '') NOT IN ('', 'auto', 'default', 'priority', 'flex')
     OR jsonb_path_exists(config, '$.temperature ? (@.type() != "number" || @ < 0 || @ > 2)')
     OR jsonb_path_exists(config, '$.max_retries ? (@.type() != "number" || @ < 0)')
     OR jsonb_path_exists(config, '$.read_timeout ? (@.type() != "number" || @ < 5)')
     OR jsonb_path_exists(config, '$.max_tokens ? (@.type() != "number" || @ <= 0)')
     OR jsonb_path_exists(config, '$.context_win ? (@.type() != "number" || @ <= 0)')
     OR jsonb_path_exists(config, '$.thinking_budget_tokens ? (@.type() != "number" || @ <= 0)')
     OR jsonb_path_exists(config, '$.stream ? (@.type() != "boolean")')
     OR jsonb_path_exists(config, '$.fake_cc_system_prompt ? (@.type() != "boolean")')
     OR jsonb_path_exists(config, '$.verify ? (@.type() != "boolean")')
     OR jsonb_path_exists(config, '$.omit_thinking ? (@.type() != "boolean")')
     OR (config->>'thinking_type' = 'enabled' AND NOT config ? 'thinking_budget_tokens')
     OR (provider_type = 'native_claude' AND config->>'api_mode' = 'responses')
     OR (provider_type = 'native_oai' AND config ? 'fake_cc_system_prompt')
     OR octet_length(COALESCE(config->>'extra_sys_prompt', '')) > 65536;
  IF invalid_ids IS NOT NULL THEN
    RAISE EXCEPTION 'invalid llm provider config for provider ids: %', invalid_ids;
  END IF;
END $$;

ALTER TABLE llm_providers ADD COLUMN session_config JSONB NOT NULL DEFAULT '{}';
ALTER TABLE llm_providers ADD COLUMN transport_config JSONB NOT NULL DEFAULT '{"auth_mode":"auto"}';
ALTER TABLE llm_providers ADD COLUMN revision BIGINT NOT NULL DEFAULT 1 CHECK (revision > 0);

UPDATE llm_providers SET
  session_config = config - ARRAY['proxy','verify','connect_timeout','timeout','top_p','extra_sys_prompt_file'],
  transport_config = jsonb_strip_nulls(jsonb_build_object(
    'auth_mode', 'auto',
    'proxy_url', NULLIF(config->>'proxy',''),
    'tls_verify', config->'verify',
    'connect_timeout_seconds', COALESCE(config->'connect_timeout', config->'timeout')
  ));

ALTER TABLE llm_providers DROP COLUMN config;

CREATE TABLE llm_capability_revocations (
  jti_hash BYTEA PRIMARY KEY CHECK (octet_length(jti_hash) = 32),
  expires_at TIMESTAMPTZ NOT NULL
);
CREATE INDEX llm_capability_revocations_expires_idx ON llm_capability_revocations(expires_at);

CREATE TABLE IF NOT EXISTS migration_0023_llm_provider_ga_config_marker(id BOOLEAN PRIMARY KEY DEFAULT TRUE);
INSERT INTO migration_0023_llm_provider_ga_config_marker(id) VALUES (TRUE) ON CONFLICT DO NOTHING;
CREATE TABLE migration_0024_transparent_llm_proxy_marker(id BOOLEAN PRIMARY KEY DEFAULT TRUE);
INSERT INTO migration_0024_transparent_llm_proxy_marker(id) VALUES (TRUE);
```

Register `0024_transparent_llm_proxy.sql`, both marker tables, and `llm_capability_revocations` in `migrations.go`.

- [ ] **Step 5: Update Store CRUD and revision rules**

Add structured mutations:

```go
type LLMProviderCreate struct {
    Name string
    ProviderType LLMProviderType
    BaseURL string
    Model string
    APIKeyCiphertext []byte
    APIKeyKeyVersion string
    SessionConfig GASessionConfig
    TransportConfig ProviderTransportConfig
}

type LLMProviderUpdate struct {
    LLMProviderCreate
    RotateAPIKey bool
}
```

`UpdateProvider` must compare routing fields inside one transaction. Increment `revision` when any non-key field changes; leave it unchanged for a key-only rotation. `ListActiveProviders` SQL order:

```sql
SELECT id, name, provider_type, base_url, model, api_key_ciphertext,
       api_key_key_version, session_config, transport_config, revision,
       is_default, state, created_at, updated_at
FROM llm_providers
WHERE state = 'active'
ORDER BY is_default DESC, id ASC
```

- [ ] **Step 6: Run domain and PostgreSQL tests**

Run:

```bash
cd tenant_platform/backend-go
go test ./internal/domain -count=1
go test ./internal/infrastructure/postgres -run 'TestLLMProvider' -count=1
```

Expected: PASS. The second command requires `TEST_DATABASE_URL`; absence is a visible prerequisite failure, not a skip.

- [ ] **Step 7: Commit Task 1**

```bash
git add tenant_platform/backend-go/internal/domain/llm_provider.go tenant_platform/backend-go/internal/domain/llm_provider_test.go tenant_platform/infra/postgres/migrations/0024_transparent_llm_proxy.sql tenant_platform/backend-go/internal/infrastructure/postgres/migrations.go tenant_platform/backend-go/internal/infrastructure/postgres/llm_provider_store.go tenant_platform/backend-go/internal/infrastructure/postgres/llm_provider_store_test.go tenant_platform/backend-go/internal/api/http.go tenant_platform/backend-go/internal/api/llm_provider.go tenant_platform/backend-go/cmd/platform/main.go
git commit --only -m "refactor: split llm session and transport configuration" -- tenant_platform/backend-go/internal/domain/llm_provider.go tenant_platform/backend-go/internal/domain/llm_provider_test.go tenant_platform/infra/postgres/migrations/0024_transparent_llm_proxy.sql tenant_platform/backend-go/internal/infrastructure/postgres/migrations.go tenant_platform/backend-go/internal/infrastructure/postgres/llm_provider_store.go tenant_platform/backend-go/internal/infrastructure/postgres/llm_provider_store_test.go tenant_platform/backend-go/internal/api/http.go tenant_platform/backend-go/internal/api/llm_provider.go tenant_platform/backend-go/cmd/platform/main.go
```

### Task 2: Replace Custom Tokens with Provider-Bound JWT and Persistent Revocation

**Files:**
- Modify: `tenant_platform/backend-go/go.mod`
- Modify: `tenant_platform/backend-go/go.sum`
- Rewrite: `tenant_platform/backend-go/internal/infrastructure/llmproxy/token.go`
- Rewrite: `tenant_platform/backend-go/internal/infrastructure/llmproxy/token_test.go`
- Create: `tenant_platform/backend-go/internal/infrastructure/postgres/llm_capability_store.go`
- Create: `tenant_platform/backend-go/internal/infrastructure/postgres/llm_capability_store_test.go`

**Interfaces:**
- Produces: `llmproxy.CapabilitySpec`, `llmproxy.CapabilityClaims`, `Issuer.Issue(CapabilitySpec)`.
- Produces: `Validator.Validate(context.Context, string) (CapabilityClaims, error)`.
- Produces the exact persistence ports:

```go
type CapabilityRevocationSource interface {
    IsCapabilityRevoked(ctx context.Context, jtiHash [32]byte) (bool, error)
}

type CapabilityRevocationStore interface {
    CapabilityRevocationSource
    RevokeCapability(ctx context.Context, jti string, expiresAt time.Time) error
    DeleteExpiredCapabilityRevocations(ctx context.Context, before time.Time) (int64, error)
}
```

- [ ] **Step 1: Write failing JWT behavior tests**

Cover correct claims plus wrong algorithm/type/issuer/audience, expiry, not-before, missing subject, malformed Provider fields, and revocation:

```go
func TestValidatorRejectsProviderClaimMismatchShape(t *testing.T) {
    issuer := newTestIssuer(t)
    token, _, err := issuer.Issue(CapabilitySpec{
        SessionKey: "personal:42", ProviderID: 0, ProviderRevision: 1,
        ProviderType: domain.ProviderNativeOAI, Model: "gpt-test", PolicyVersion: "p1",
    })
    if err == nil || token != "" { t.Fatalf("expected issue validation failure") }
}

func TestValidatorChecksPersistentRevocation(t *testing.T) {
    store := &fakeRevocations{revoked: map[[32]byte]bool{}}
    issuer, validator := newJWTTestPair(t, store)
    token, claims, err := issuer.Issue(validCapabilitySpec())
    if err != nil { t.Fatal(err) }
    store.revoked[HashJTI(claims.ID)] = true
    if _, err := validator.Validate(context.Background(), token); !errors.Is(err, ErrCapabilityRevoked) {
        t.Fatalf("err=%v", err)
    }
}
```

- [ ] **Step 2: Run JWT tests and observe RED**

```bash
cd tenant_platform/backend-go
go test ./internal/infrastructure/llmproxy -run 'TestValidator|TestIssuer' -count=1
```

Expected: compile failures for new JWT interfaces.

- [ ] **Step 3: Add `golang-jwt/jwt/v5` and implement strict JWT validation**

Core types:

```go
const (
    CapabilityIssuer   = "ga-platform"
    CapabilityAudience = "ga-llm-proxy"
    CapabilityType     = "ga-llm-cap+jwt"
    MinSigningKeyLen   = 32
)

type CapabilitySpec struct {
    SessionKey string
    ProviderID int64
    ProviderRevision int64
    ProviderType domain.LLMProviderType
    Model string
    PolicyVersion string
}

type CapabilityClaims struct {
    ProviderID int64 `json:"provider_id"`
    ProviderRevision int64 `json:"provider_revision"`
    ProviderType domain.LLMProviderType `json:"provider_type"`
    Model string `json:"model"`
    PolicyVersion string `json:"policy_version"`
    jwt.RegisteredClaims
}
```

Construct parser with `jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()})`, `WithIssuer`, `WithAudience`, `WithExpirationRequired`, `WithIssuedAt`, and bounded leeway. After parse, require header `typ == CapabilityType`, non-empty subject/JTI/model, positive Provider IDs/revision, and a supported native type. Check persistent revocation after cryptographic and registered-claim validation.

- [ ] **Step 4: Implement PostgreSQL revocation by JTI hash**

```go
func HashJTI(jti string) [32]byte { return sha256.Sum256([]byte(jti)) }

func (s *Store) RevokeCapability(ctx context.Context, jti string, expiresAt time.Time) error {
    digest := llmproxy.HashJTI(jti)
    _, err := s.pool.Exec(ctx, `
        INSERT INTO llm_capability_revocations(jti_hash, expires_at)
        VALUES ($1, $2)
        ON CONFLICT (jti_hash) DO UPDATE SET expires_at = GREATEST(llm_capability_revocations.expires_at, EXCLUDED.expires_at)
    `, digest[:], expiresAt.UTC())
    return err
}
```

`IsCapabilityRevoked` returns `(false, nil)` for no row; cleanup deletes `expires_at <= now()`.

- [ ] **Step 5: Run JWT and store tests**

```bash
cd tenant_platform/backend-go
go test ./internal/infrastructure/llmproxy -run 'TestValidator|TestIssuer' -count=1
go test ./internal/infrastructure/postgres -run 'TestCapabilityRevocation' -count=1
```

Expected: PASS.

- [ ] **Step 6: Commit Task 2**

```bash
git add tenant_platform/backend-go/go.mod tenant_platform/backend-go/go.sum tenant_platform/backend-go/internal/infrastructure/llmproxy/token.go tenant_platform/backend-go/internal/infrastructure/llmproxy/token_test.go tenant_platform/backend-go/internal/infrastructure/postgres/llm_capability_store.go tenant_platform/backend-go/internal/infrastructure/postgres/llm_capability_store_test.go
git commit --only -m "feat: bind llm capabilities with strict jwt claims" -- tenant_platform/backend-go/go.mod tenant_platform/backend-go/go.sum tenant_platform/backend-go/internal/infrastructure/llmproxy/token.go tenant_platform/backend-go/internal/infrastructure/llmproxy/token_test.go tenant_platform/backend-go/internal/infrastructure/postgres/llm_capability_store.go tenant_platform/backend-go/internal/infrastructure/postgres/llm_capability_store_test.go
```

### Task 3: Generate Structured Token-Only GA Runtime Configuration

**Files:**
- Create: `tenant_platform/backend-go/internal/application/runtime_config.go`
- Create: `tenant_platform/backend-go/internal/application/runtime_config_replace_posix.go`
- Create: `tenant_platform/backend-go/internal/application/runtime_config_replace_windows.go`
- Create: `tenant_platform/backend-go/internal/application/runtime_config_test.go`
- Modify: `tenant_platform/backend-go/internal/application/worker_credential.go`
- Modify: `tenant_platform/backend-go/internal/application/worker_credential_test.go`

**Interfaces:**
- Consumes: `domain.LLMProvider`, provider JWTs from Task 2.
- Produces: `RuntimeProviderBinding`, `RuntimeConfigMetadata`, `BuildRuntimeConfig`, `WriteRuntimeConfigAtomic`.
- Produces: stable Provider names `provider-<id>` and variable names containing `native_oai`/`native_claude`.

- [ ] **Step 1: Write failing serialization and GA import contract tests**

Tests must prove:

```go
func TestBuildRuntimeConfigPreservesZeroAndPythonLoadsGA(t *testing.T) {
    zeroInt, zeroFloat, stream := 0, 0.0, true
    provider := domain.LLMProvider{
        ID: 7, ProviderType: domain.ProviderNativeOAI, Model: "gpt-test",
        SessionConfig: domain.GASessionConfig{
            MaxRetries: &zeroInt, Temperature: &zeroFloat, Stream: &stream,
        },
    }
    files, err := BuildRuntimeConfig(RuntimeConfigInput{
        Generation: 3, ProxyBaseURL: "http://127.0.0.1:8081",
        Providers: []RuntimeProviderBinding{{Provider: provider, Token: "capability-token"}},
    })
    if err != nil { t.Fatal(err) }
    if bytes.Contains(files.JSON, []byte("real-upstream")) { t.Fatal("real key leaked") }
    writeFilesAndRunPythonGAImport(t, files, "platform_native_oai_provider_7_config", "NativeOAISession")
}
```

The Python subprocess prepends the temp config directory and repo root to `sys.path`, imports `llmcore`, calls `resolve_session`, and asserts class plus values.

- [ ] **Step 2: Run the focused test and observe RED**

```bash
cd tenant_platform/backend-go
go test ./internal/application -run 'TestBuildRuntimeConfig' -count=1
```

Expected: compile failure because the runtime config API does not exist.

- [ ] **Step 3: Implement canonical JSON plus fixed loader**

```go
const MyKeyLoader = `import json as _json
from pathlib import Path as _Path
_config = _json.loads(_Path(__file__).with_name("mykey.runtime.json").read_text(encoding="utf-8"))
globals().update(_config)
del _config
`

type RuntimeProviderBinding struct {
    Provider domain.LLMProvider
    Token string
}

type RuntimeConfigInput struct {
    Generation uint64
    ProxyBaseURL string
    RoutingSnapshotID string
    Providers []RuntimeProviderBinding
}

type RuntimeConfigMetadata struct {
    CredentialGeneration uint64 `json:"credential_generation"`
    ConfigChecksum string `json:"config_checksum"`
    RoutingSnapshotID string `json:"routing_snapshot_id"`
}

type RuntimeConfigFiles struct {
    JSON []byte
    Loader []byte
    Generation uint64
    Checksum string
    SnapshotID string
}
```

Compute `config_checksum` from canonical JSON with the checksum field empty; set it, marshal final JSON, and make the Worker use the same rule. Include `_platform_runtime`, provider configs, and `mixin_config`. Never serialize API ciphertext or plaintext.

- [ ] **Step 4: Implement atomic writes and permissions**

Write and `Sync` a temp file in the target directory before replacement. POSIX uses `os.Rename` followed by syncing the parent directory; Windows uses `windows.MoveFileEx` with `MOVEFILE_REPLACE_EXISTING|MOVEFILE_WRITE_THROUGH`. Implement these paths in the two build-tagged files listed above. Tests inject a replacement failure and assert the previous complete JSON remains byte-for-byte readable.

- [ ] **Step 5: Run runtime config tests**

```bash
cd tenant_platform/backend-go
go test ./internal/application -run 'TestBuildRuntimeConfig|TestWriteRuntimeConfig' -count=1
```

Expected: PASS, including actual GA import and no real-key assertion.

- [ ] **Step 6: Commit Task 3**

```bash
git add tenant_platform/backend-go/internal/application/runtime_config.go tenant_platform/backend-go/internal/application/runtime_config_replace_posix.go tenant_platform/backend-go/internal/application/runtime_config_replace_windows.go tenant_platform/backend-go/internal/application/runtime_config_test.go tenant_platform/backend-go/internal/application/worker_credential.go tenant_platform/backend-go/internal/application/worker_credential_test.go
git commit --only -m "feat: generate structured token-only ga runtime config" -- tenant_platform/backend-go/internal/application/runtime_config.go tenant_platform/backend-go/internal/application/runtime_config_replace_posix.go tenant_platform/backend-go/internal/application/runtime_config_replace_windows.go tenant_platform/backend-go/internal/application/runtime_config_test.go tenant_platform/backend-go/internal/application/worker_credential.go tenant_platform/backend-go/internal/application/worker_credential_test.go
```

### Task 4: Add Credential Reload Handshake to Worker gRPC

**Files:**
- Modify: `tenant_platform/contracts/proto/genericagent/worker/v1/worker.proto`
- Create: `tenant_platform/contracts/proto/generate_bindings.py`
- Regenerate: `tenant_platform/backend-go/internal/gen/worker/v1/worker.pb.go`
- Regenerate: `tenant_platform/backend-go/internal/gen/worker/v1/worker_grpc.pb.go`
- Regenerate: `tenant_platform/worker-python/src/genericagent/worker/v1/worker_pb2.py`
- Regenerate: `tenant_platform/worker-python/src/genericagent/worker/v1/worker_pb2_grpc.py`
- Modify: `tenant_platform/backend-go/internal/infrastructure/workerclient/client.go`
- Modify: `tenant_platform/backend-go/internal/infrastructure/workerclient/client_test.go`
- Create: `tenant_platform/worker-python/src/ga_worker/credential_config.py`
- Modify: `tenant_platform/worker-python/src/ga_worker/state.py`
- Modify: `tenant_platform/worker-python/src/ga_worker/managed_agent.py`
- Modify: `tenant_platform/worker-python/src/ga_worker/rpc_server.py`
- Modify: `tenant_platform/worker-python/src/ga_worker/session_lifecycle.py`
- Delete: `tenant_platform/worker-python/src/ga_worker/config_fetcher.py`
- Modify: `tenant_platform/worker-python/tests/unit/test_managed_agent.py`
- Modify: `tenant_platform/tests/contract/test_contract_sources.py`
- Modify: `tenant_platform/tests/contract/test_generated_bindings.py`

**Interfaces:**
- Produces RPC: `ReloadCredentials(ReloadCredentialsRequest) returns (ReloadCredentialsResponse)`.
- Produces Go client: `ReloadCredentials(ctx, generation, checksum) (*ReloadCredentialsResponse, error)`.
- Produces Worker adapter: `reload_credentials(request)` with monotonic generation and checksum verification.

- [ ] **Step 1: Add failing contract assertions**

Update contract tests to require:

```python
assert "rpc ReloadCredentials" in text
assert method_names == {
    "StartSession", "ReloadCredentials", "ExecuteTask", "BeginCheckpoint",
    "CancelTask", "Health", "Shutdown",
}
```

- [ ] **Step 2: Run contract tests and observe RED**

```bash
cd tenant_platform
python -m pytest tests/contract/test_contract_sources.py tests/contract/test_generated_bindings.py -q
```

Expected: failure because the RPC and generated bindings are absent.

- [ ] **Step 3: Extend the proto**

```proto
message ReloadCredentialsRequest {
  uint64 credential_generation = 1;
  string config_checksum = 2;
}

message ReloadCredentialsResponse {
  uint64 credential_generation = 1;
  string config_checksum = 2;
}

service WorkerService {
  rpc StartSession(StartSessionRequest) returns (StartSessionResponse);
  rpc ReloadCredentials(ReloadCredentialsRequest) returns (ReloadCredentialsResponse);
  rpc ExecuteTask(ExecuteTaskRequest) returns (stream WorkerEvent);
  rpc BeginCheckpoint(BeginCheckpointRequest) returns (CheckpointReady);
  rpc CancelTask(CancelTaskRequest) returns (CancelTaskResponse);
  rpc Health(HealthRequest) returns (HealthResponse);
  rpc Shutdown(ShutdownRequest) returns (ShutdownResponse);
}
```

- [ ] **Step 4: Add reproducible binding generation**

`generate_bindings.py` must locate repo paths, run `protoc` with the module option for Go, then `python -m grpc_tools.protoc` for Python, and fail on a missing compiler/plugin. Exact underlying commands:

```bash
protoc -I tenant_platform/contracts/proto --go_out=tenant_platform/backend-go --go_opt=module=github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go --go-grpc_out=tenant_platform/backend-go --go-grpc_opt=module=github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go tenant_platform/contracts/proto/genericagent/worker/v1/worker.proto
python -m grpc_tools.protoc -I tenant_platform/contracts/proto --python_out=tenant_platform/worker-python/src --grpc_python_out=tenant_platform/worker-python/src tenant_platform/contracts/proto/genericagent/worker/v1/worker.proto
```

Run the script once and commit generated files.

- [ ] **Step 5: Implement Worker metadata verification and reload**

`credential_config.py` must parse `_platform_runtime`, recompute canonical checksum with an empty checksum field, and return immutable metadata. Adapter behavior:

```python
def reload_credentials(self, request: worker_pb2.ReloadCredentialsRequest):
    with self._lock:
        if self._session is None:
            raise WorkerAdapterError("SESSION_NOT_STARTED", "session not started")
        if self._pending is not None or self._session.active_task_id:
            raise WorkerAdapterError("TASK_ACTIVE", "cannot reload credentials during a task")
        if request.credential_generation <= self._session.credential_generation:
            raise WorkerAdapterError("CREDENTIAL_GENERATION_STALE", "generation must increase")
        metadata = load_runtime_metadata(self.config_root)
        validate_reload_request(metadata, request)
        self._session.agent.load_llm_sessions()
        if not self._session.agent.llmclients:
            raise WorkerAdapterError("CREDENTIAL_CONFIG_EMPTY", "no GA sessions loaded")
        self._session.credential_generation = metadata.generation
        self._session.credential_checksum = metadata.checksum
        return worker_pb2.ReloadCredentialsResponse(
            credential_generation=metadata.generation,
            config_checksum=metadata.checksum,
        )
```

Initial session creation reads local runtime metadata before creating GA. Remove all Platform URL/dev-token fetching.

- [ ] **Step 6: Implement Go client and server method**

Add the typed Worker client method with the existing unary timeout/wrap pattern. Add `WorkerServicer.ReloadCredentials` delegating to the adapter and returning structured gRPC errors.

- [ ] **Step 7: Run Worker and binding tests**

```bash
cd tenant_platform
python -m pytest tests/contract/test_contract_sources.py tests/contract/test_generated_bindings.py -q
cd worker-python
python -m pytest tests/unit/test_managed_agent.py -q
cd ../backend-go
go test ./internal/infrastructure/workerclient -count=1
```

Expected: all PASS.

- [ ] **Step 8: Commit Task 4**

```bash
git add tenant_platform/contracts/proto/genericagent/worker/v1/worker.proto tenant_platform/contracts/proto/generate_bindings.py tenant_platform/backend-go/internal/gen/worker/v1/worker.pb.go tenant_platform/backend-go/internal/gen/worker/v1/worker_grpc.pb.go tenant_platform/worker-python/src/genericagent/worker/v1/worker_pb2.py tenant_platform/worker-python/src/genericagent/worker/v1/worker_pb2_grpc.py tenant_platform/backend-go/internal/infrastructure/workerclient/client.go tenant_platform/backend-go/internal/infrastructure/workerclient/client_test.go tenant_platform/worker-python/src/ga_worker/credential_config.py tenant_platform/worker-python/src/ga_worker/state.py tenant_platform/worker-python/src/ga_worker/managed_agent.py tenant_platform/worker-python/src/ga_worker/rpc_server.py tenant_platform/worker-python/src/ga_worker/session_lifecycle.py tenant_platform/worker-python/tests/unit/test_managed_agent.py tenant_platform/tests/contract/test_contract_sources.py tenant_platform/tests/contract/test_generated_bindings.py
git rm tenant_platform/worker-python/src/ga_worker/config_fetcher.py
git commit --only -m "feat: reload worker capabilities at task boundaries" -- tenant_platform/contracts/proto/genericagent/worker/v1/worker.proto tenant_platform/contracts/proto/generate_bindings.py tenant_platform/backend-go/internal/gen/worker/v1/worker.pb.go tenant_platform/backend-go/internal/gen/worker/v1/worker_grpc.pb.go tenant_platform/worker-python/src/genericagent/worker/v1/worker_pb2.py tenant_platform/worker-python/src/genericagent/worker/v1/worker_pb2_grpc.py tenant_platform/backend-go/internal/infrastructure/workerclient/client.go tenant_platform/backend-go/internal/infrastructure/workerclient/client_test.go tenant_platform/worker-python/src/ga_worker/credential_config.py tenant_platform/worker-python/src/ga_worker/state.py tenant_platform/worker-python/src/ga_worker/managed_agent.py tenant_platform/worker-python/src/ga_worker/rpc_server.py tenant_platform/worker-python/src/ga_worker/session_lifecycle.py tenant_platform/worker-python/src/ga_worker/config_fetcher.py tenant_platform/worker-python/tests/unit/test_managed_agent.py tenant_platform/tests/contract/test_contract_sources.py tenant_platform/tests/contract/test_generated_bindings.py
```

### Task 5: Bind Scheduler Workers to Routing Snapshots and Rotate Tokens

**Files:**
- Modify: `tenant_platform/backend-go/internal/application/scheduler.go`
- Modify: `tenant_platform/backend-go/internal/application/scheduler_worker.go`
- Modify: `tenant_platform/backend-go/internal/application/scheduler_dispatch.go`
- Rewrite: `tenant_platform/backend-go/internal/application/worker_credential.go`
- Modify: `tenant_platform/backend-go/internal/application/worker_credential_test.go`
- Modify: `tenant_platform/backend-go/internal/application/scheduler_test.go`
- Modify: `tenant_platform/backend-go/cmd/platform/main.go`

**Interfaces:**
- Consumes: structured runtime config, JWT issuer, Worker `ReloadCredentials`, and these exact ports:

```go
type LLMProviderSource interface {
    ListActiveProviders(ctx context.Context) ([]domain.LLMProvider, error)
    GetProvider(ctx context.Context, id int64) (domain.LLMProvider, error)
}

type CapabilityStore interface {
    RevokeCapability(ctx context.Context, jti string, expiresAt time.Time) error
}
```

- Produces: immutable `routingSnapshot`, `workerCredentialSet`, refresh/revoke lifecycle.
- Adds Scheduler config: `TokenTTL`, `TokenRefreshSkew`, `MaxTaskWallClock`.

- [ ] **Step 1: Write failing routing and rotation tests with fakes**

Cover default-first mixin, token per Provider, default switch not changing an existing snapshot, Provider revision forcing Worker replacement, key-only rotation preserving revision, refresh acknowledgment ordering, and reload failure retaining old JTI:

```go
func TestRefreshRevokesOldTokensOnlyAfterWorkerAck(t *testing.T) {
    store := newFakeCapabilityStore()
    worker := newFakeWorkerClient()
    worker.reloadErr = errors.New("reload failed")
    entry := seededWorkerEntryWithExpiringCredentials()

    err := sched.refreshWorkerCredentials(context.Background(), entry)
    if err == nil { t.Fatal("expected reload error") }
    if len(store.revoked) != 0 { t.Fatalf("revoked before ack: %v", store.revoked) }
}
```

- [ ] **Step 2: Run scheduler tests and observe RED**

```bash
cd tenant_platform/backend-go
go test ./internal/application -run 'TestRefresh|TestRoutingSnapshot|TestIssueProviderCapabilities' -count=1
```

Expected: compile failures for snapshot/refresh APIs.

- [ ] **Step 3: Implement routing snapshots and credential sets**

```go
type routingProvider struct {
    ID int64
    Revision int64
    ProviderType domain.LLMProviderType
    Model string
    RuntimeName string
    SessionConfig domain.GASessionConfig
}

type routingSnapshot struct {
    ID string
    Providers []routingProvider
}

type workerCredentialSet struct {
    Generation uint64
    Checksum string
    ExpiresAt time.Time
    JTIs []string
    Snapshot routingSnapshot
}
```

Build snapshot from `ListActiveProviders`; reject no default or duplicate defaults. Snapshot ID is SHA-256 of ordered provider ID/revision/type/model/session config. Never include keys.

- [ ] **Step 4: Implement issue, refresh, acknowledgment, and revocation ordering**

Initial flow:

```text
resolve snapshot -> issue one JWT/provider -> write generation 1 config -> start Worker -> StartSession
```

Refresh flow:

```text
check live Provider revisions -> issue new set -> atomic write N+1 -> ReloadCredentials RPC -> verify echo -> persist old JTI revocations -> replace entry credential state
```

Before writing generation `N+1`, retain the exact previous JSON bytes and credential state. Any issue/write/RPC/echo failure atomically restores those bytes, revokes every newly issued JTI, leaves all old JTIs active, and returns an error before task dispatch. Only a successful RPC echo permits old-JTI revocation and entry-state replacement.

- [ ] **Step 5: Enforce bounded task wall clock and startup invariant**

Add `MAX_TASK_WALL_CLOCK_SECONDS` default `2700`, token TTL default `3600s`, refresh skew default `300s`. `NewScheduler` rejects:

```go
if cfg.MaxTaskWallClock <= 0 { return nil, errors.New("MaxTaskWallClock must be positive") }
if cfg.TokenTTL < cfg.MaxTaskWallClock+cfg.TokenRefreshSkew {
    return nil, errors.New("TokenTTL must cover MaxTaskWallClock plus refresh skew")
}
```

Wrap the ExecuteTask stream context with `context.WithTimeout(ctx, MaxTaskWallClock)`. On deadline, cancel Worker and finalize `TASK_DEADLINE_EXCEEDED`; do not report success after the deadline race.

- [ ] **Step 6: Run focused scheduler tests**

```bash
cd tenant_platform/backend-go
go test ./internal/application -run 'TestRefresh|TestRoutingSnapshot|TestIssueProviderCapabilities|TestScheduler.*Deadline' -count=1
```

Expected: PASS.

- [ ] **Step 7: Commit Task 5**

```bash
git add tenant_platform/backend-go/internal/application/scheduler.go tenant_platform/backend-go/internal/application/scheduler_worker.go tenant_platform/backend-go/internal/application/scheduler_dispatch.go tenant_platform/backend-go/internal/application/worker_credential.go tenant_platform/backend-go/internal/application/worker_credential_test.go tenant_platform/backend-go/internal/application/scheduler_test.go tenant_platform/backend-go/cmd/platform/main.go
git commit --only -m "feat: bind workers to provider routing snapshots" -- tenant_platform/backend-go/internal/application/scheduler.go tenant_platform/backend-go/internal/application/scheduler_worker.go tenant_platform/backend-go/internal/application/scheduler_dispatch.go tenant_platform/backend-go/internal/application/worker_credential.go tenant_platform/backend-go/internal/application/worker_credential_test.go tenant_platform/backend-go/internal/application/scheduler_test.go tenant_platform/backend-go/cmd/platform/main.go
```

### Task 6: Implement GA-Compatible Target, Header, and Transport Policy

**Files:**
- Create: `tenant_platform/backend-go/internal/infrastructure/llmproxy/target.go`
- Create: `tenant_platform/backend-go/internal/infrastructure/llmproxy/target_test.go`
- Create: `tenant_platform/backend-go/internal/infrastructure/llmproxy/headers.go`
- Create: `tenant_platform/backend-go/internal/infrastructure/llmproxy/headers_test.go`
- Create: `tenant_platform/backend-go/internal/infrastructure/llmproxy/transport.go`
- Create: `tenant_platform/backend-go/internal/infrastructure/llmproxy/transport_test.go`
- Modify: `tenant_platform/backend-go/internal/infrastructure/llmproxy/config.go`

**Interfaces:**
- Produces: `ResolveUpstreamTarget(provider, inboundURL) (*url.URL, error)`.
- Produces: `SanitizeAndInjectHeaders(out, inbound, provider, realKey)`.
- Produces: `TransportCache.RoundTripper(provider) (http.RoundTripper, error)`.
- Produces deployment network policy parsed from allowed CIDRs/HTTP hosts.

- [x] **Step 1: Write failing URL compatibility tests**

Table cases must mirror GA `auto_make_url()`:

```go
{
    name: "oai base already has v1",
    base: "https://api.openai.com/v1", inbound: "/v1/chat/completions",
    want: "https://api.openai.com/v1/chat/completions",
},
{
    name: "claude exact endpoint",
    base: "https://relay.example/custom/messages$", inbound: "/v1/messages?beta=true",
    want: "https://relay.example/custom/messages?beta=true",
},
```

Also reject type/path mismatch and conflicting query keys.

- [x] **Step 2: Write failing header security tests**

Assert capability Authorization, cookies, forwarding headers, and incoming x-api-key never reach upstream; GA native beta/UA/Stainless headers do; real auth is injected last. Cover Claude auto/Bearer/x-api-key.

- [x] **Step 3: Write failing SSRF and transport-cache tests**

Reject HTTP/public-policy mismatch, loopback, link-local, metadata IP, redirect, and DNS results outside allowlist. Permit `httptest.Server` only when its CIDR/host is explicitly configured. Assert identical transport hashes reuse a pointer and config change replaces it.

- [x] **Step 4: Run policy tests and observe RED**

```bash
cd tenant_platform/backend-go
go test ./internal/infrastructure/llmproxy -run 'TestResolveUpstream|TestHeaders|TestTransport|TestNetworkPolicy' -count=1
```

Expected: compile failures for the new policy APIs.

- [x] **Step 5: Implement target resolution and header policy**

Use parsed `url.URL`, never string concatenation. `$` is stripped only from the configured path terminator. Merge query maps and reject duplicate keys with different values. Header policy uses explicit case-insensitive allowlists; do not copy all headers then subtract.

- [x] **Step 6: Implement cached transports and network enforcement**

Transport settings:

```go
&http.Transport{
    Proxy: proxyFunc,
    DialContext: (&net.Dialer{Timeout: connectTimeout, KeepAlive: 30 * time.Second}).DialContext,
    ForceAttemptHTTP2: true,
    MaxIdleConns: 64,
    MaxIdleConnsPerHost: 32,
    IdleConnTimeout: 90 * time.Second,
    TLSHandshakeTimeout: 10 * time.Second,
    ResponseHeaderTimeout: responseHeaderTimeout,
    TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12, InsecureSkipVerify: !tlsVerify},
}
```

The custom dialer revalidates the resolved IP immediately before connect to close DNS-rebinding gaps. Disable redirects by using raw `RoundTripper`, not `http.Client.Do` redirect behavior.

- [x] **Step 7: Run policy tests**

```bash
cd tenant_platform/backend-go
go test ./internal/infrastructure/llmproxy -run 'TestResolveUpstream|TestHeaders|TestTransport|TestNetworkPolicy' -count=1
```

Expected: PASS.

- [x] **Step 8: Commit Task 6**

```bash
git add tenant_platform/backend-go/internal/infrastructure/llmproxy/target.go tenant_platform/backend-go/internal/infrastructure/llmproxy/target_test.go tenant_platform/backend-go/internal/infrastructure/llmproxy/headers.go tenant_platform/backend-go/internal/infrastructure/llmproxy/headers_test.go tenant_platform/backend-go/internal/infrastructure/llmproxy/transport.go tenant_platform/backend-go/internal/infrastructure/llmproxy/transport_test.go tenant_platform/backend-go/internal/infrastructure/llmproxy/config.go
git commit --only -m "feat: add safe native llm routing policy" -- tenant_platform/backend-go/internal/infrastructure/llmproxy/target.go tenant_platform/backend-go/internal/infrastructure/llmproxy/target_test.go tenant_platform/backend-go/internal/infrastructure/llmproxy/headers.go tenant_platform/backend-go/internal/infrastructure/llmproxy/headers_test.go tenant_platform/backend-go/internal/infrastructure/llmproxy/transport.go tenant_platform/backend-go/internal/infrastructure/llmproxy/transport_test.go tenant_platform/backend-go/internal/infrastructure/llmproxy/config.go
```

### Task 7: Replace Buffered Protocol Forwarder with Transparent ReverseProxy

**Files:**
- Rewrite: `tenant_platform/backend-go/internal/infrastructure/llmproxy/provider.go`
- Rewrite: `tenant_platform/backend-go/internal/infrastructure/llmproxy/handler.go`
- Rewrite: `tenant_platform/backend-go/internal/infrastructure/llmproxy/server.go`
- Delete: `tenant_platform/backend-go/internal/infrastructure/llmproxy/upstream.go`
- Rewrite: `tenant_platform/backend-go/internal/infrastructure/llmproxy/handler_test.go`
- Rewrite: `tenant_platform/backend-go/internal/infrastructure/llmproxy/server_test.go`
- Delete: `tenant_platform/backend-go/internal/infrastructure/llmproxy/upstream_test.go`
- Create: `tenant_platform/backend-go/internal/infrastructure/llmproxy/reverse_proxy_test.go`
- Modify: `tenant_platform/backend-go/cmd/llm-proxy/main.go`
- Modify: `tenant_platform/backend-go/cmd/platform/main.go`
- Verify unchanged: `tenant_platform/backend-go/internal/application/token_revoker.go` remains the persistent scheduler revocation port; no HTTP revoker is retained.

**Interfaces:**
- Consumes: strict JWT validator, `ProviderSource.GetProvider(id)`, revocation store, target/header/transport policies.
- Produces: transparent `http.Handler` for chat, Responses, and messages with SSE.

- [x] **Step 1: Write failing end-to-end proxy tests**

Use `httptest.Server` fixture routes for:

- OAI chat JSON;
- OAI Responses JSON and SSE;
- Claude JSON and SSE;
- Claude Bearer and x-api-key;
- beta/UA/Stainless headers;
- response header allowlist preserves content/stream/request-id metadata and strips `Set-Cookie`, `Server`, `WWW-Authenticate`, and unknown headers;
- first SSE event received before fixture completion;
- client cancel reaching fixture context;
- model mismatch, claim/provider revision mismatch, disabled Provider;
- 429 with `Retry-After` and sanitized body;
- no upstream replay after 500.

SSE assertion pattern:

```go
firstChunk := make(chan struct{})
upstreamDone := make(chan struct{})
// Fixture flushes one event, signals firstChunk, then blocks on upstreamDone.
// Client must read the first event before upstreamDone closes.
```

- [x] **Step 2: Run proxy tests and observe RED**

```bash
cd tenant_platform/backend-go
go test ./internal/infrastructure/llmproxy -run 'TestReverseProxy|TestSSE|TestCapabilityBinding' -count=1
```

Expected: failures because current routes omit Responses, buffer responses, drop headers, and ignore claims.

- [x] **Step 3: Implement request validation context**

```go
type proxyRequestContext struct {
    Claims CapabilityClaims
    Provider domain.LLMProvider
    Target *url.URL
    RealKey string
}
```

Handler sequence is fixed:

```text
method/path -> bearer extraction -> JWT validate -> provider by claims.ProviderID ->
active/revision/type/model checks -> bounded body read -> top-level model check ->
target resolution -> decrypted key -> attach context -> ReverseProxy.ServeHTTP
```

Return stable error codes; never fall back to default Provider.

- [x] **Step 4: Implement one shared ReverseProxy**

`Rewrite` reads `proxyRequestContext`, calls `SetURL`, sets Host, rebuilds allowlisted request headers, and injects auth. A routing `RoundTripper` chooses the cached transport from context. `ModifyResponse` always rebuilds response headers from the exact allowlist `Content-Type`, `Content-Length`, `Content-Encoding`, `Cache-Control`, `Vary`, `Retry-After`, `X-Request-Id`, `Request-Id`, `OpenAI-Request-Id`, and `Anthropic-Request-Id`. For 2xx it does not read the body; for non-2xx it retains status and allowed retry/request metadata while replacing the body with a bounded generic JSON envelope. Set `FlushInterval = -1` for immediate streaming writes.

- [x] **Step 5: Correct HTTP server timeouts**

Set:

```go
ReadHeaderTimeout: 10 * time.Second,
ReadTimeout:       30 * time.Second,
WriteTimeout:      0,
IdleTimeout:       120 * time.Second,
MaxHeaderBytes:    1 << 20,
```

Do not add a whole-response `http.Client.Timeout` or handler `context.WithTimeout` that truncates SSE. Task wall-clock and client cancellation own total lifetime.

- [x] **Step 6: Remove old forwarder and HTTP revocation path**

Delete buffered `Upstream` and ensure `/internal/revoke` and `HTTPTokenRevoker` remain absent. Retain `persistentTokenRevoker`, because Scheduler writes revocations through that persistence port.

- [x] **Step 7: Run all Proxy tests**

```bash
cd tenant_platform/backend-go
go test ./internal/infrastructure/llmproxy -count=1
go test ./cmd/llm-proxy -count=1
```

Expected: PASS.

- [x] **Step 8: Commit Task 7**

```bash
git rm tenant_platform/backend-go/internal/infrastructure/llmproxy/upstream.go tenant_platform/backend-go/internal/infrastructure/llmproxy/upstream_test.go
git add tenant_platform/backend-go/internal/infrastructure/llmproxy tenant_platform/backend-go/cmd/llm-proxy/main.go tenant_platform/backend-go/cmd/platform/main.go
git commit --only -m "refactor: transparently stream native llm traffic" -- tenant_platform/backend-go/internal/infrastructure/llmproxy tenant_platform/backend-go/cmd/llm-proxy/main.go tenant_platform/backend-go/cmd/platform/main.go
```

### Task 8: Clean Cutover Admin API, OpenAPI, and Web Configuration

**Files:**
- Rewrite: `tenant_platform/backend-go/internal/api/llm_provider.go`
- Modify: `tenant_platform/backend-go/internal/api/llm_provider_test.go`
- Modify: `tenant_platform/backend-go/internal/domain/llm_provider.go`
- Modify: `tenant_platform/backend-go/internal/api/admin.go`
- Delete: `tenant_platform/backend-go/internal/api/config_handler.go`
- Delete: `tenant_platform/backend-go/internal/api/mykey_generator.go`
- Modify: `tenant_platform/contracts/openapi/platform.yaml`
- Modify: `tenant_platform/web/src/api/types.ts`
- Modify: `tenant_platform/web/src/api/providers.ts`
- Rewrite: `tenant_platform/web/src/features/admin/LLMProvidersPage.tsx`
- Create: `tenant_platform/web/src/features/admin/LLMProviderForm.tsx`
- Modify: `tenant_platform/web/src/features/admin/AdminPages.css`
- Modify: `tenant_platform/web/src/components/layout/Layout.css`
- Modify: `tenant_platform/web/src/index.css`
- Modify: `tenant_platform/web/src/features/user/UserPages.css`

**Interfaces:**
- API request/response uses `session_config`, `transport_config`, and `revision`.
- Update request `api_key` is optional; omitted/blank preserves the encrypted key.
- Web supports create and edit with exact backend types and no `any`.

- [x] **Step 1: Write failing API validation tests**

Cover native-only enum, explicit zero, invalid ranges/combinations, unknown fields, optional update key, no key in replies, and removed config download endpoint:

```go
func TestAdminUpdateLLMProviderOmittedKeyPreservesCiphertext(t *testing.T) {
    srv, store, _ := llmProviderServerFixture(t)
    create := []byte(`{"name":"p","provider_type":"native_oai","base_url":"https://api.openai.com/v1","model":"gpt-test","api_key":"sk-original","session_config":{},"transport_config":{"auth_mode":"auto"}}`)
    createReq := httptest.NewRequest(http.MethodPost, "/v1/admin/llm-providers", bytes.NewReader(create))
    createReq.Header.Set("X-Platform-Dev-Token", "test-dev-token")
    createRR := httptest.NewRecorder()
    srv.Handler().ServeHTTP(createRR, createReq)
    if createRR.Code != http.StatusCreated { t.Fatalf("create=%d body=%s", createRR.Code, createRR.Body.String()) }
    var created map[string]any
    if err := json.Unmarshal(createRR.Body.Bytes(), &created); err != nil { t.Fatal(err) }
    id := int64(created["provider_id"].(float64))
    before, err := store.GetProvider(context.Background(), id)
    if err != nil { t.Fatal(err) }

    update := []byte(`{"name":"p2","provider_type":"native_oai","base_url":"https://api.openai.com/v1","model":"gpt-test","session_config":{},"transport_config":{"auth_mode":"auto"}}`)
    updateReq := httptest.NewRequest(http.MethodPut, fmt.Sprintf("/v1/admin/llm-providers/%d", id), bytes.NewReader(update))
    updateReq.Header.Set("X-Platform-Dev-Token", "test-dev-token")
    updateRR := httptest.NewRecorder()
    srv.Handler().ServeHTTP(updateRR, updateReq)
    if updateRR.Code != http.StatusOK { t.Fatalf("update=%d body=%s", updateRR.Code, updateRR.Body.String()) }
    after, err := store.GetProvider(context.Background(), id)
    if err != nil { t.Fatal(err) }
    if !bytes.Equal(after.APIKeyCiphertext, before.APIKeyCiphertext) || after.APIKeyKeyVersion != before.APIKeyKeyVersion {
        t.Fatal("omitted api_key rotated stored credentials")
    }
}
func TestProviderConfigEndpointRemoved(t *testing.T) {
    rr := httptest.NewRecorder()
    srv.Handler().ServeHTTP(rr, authenticatedRequest(http.MethodGet, "/v1/config/mykey.py", nil))
    if rr.Code != http.StatusNotFound { t.Fatalf("status=%d", rr.Code) }
}
```

- [x] **Step 2: Run API tests and observe RED**

```bash
cd tenant_platform/backend-go
go test ./internal/api -run 'Test.*LLMProvider|TestProviderConfigEndpointRemoved' -count=1
```

Expected: current old enum tests/config route/nested config behavior fail. Requires `TEST_DATABASE_URL`.

- [x] **Step 3: Implement strict nested API and remove plaintext route**

Request shape:

```go
type providerWriteBody struct {
    Name string `json:"name"`
    ProviderType domain.LLMProviderType `json:"provider_type"`
    BaseURL string `json:"base_url"`
    Model string `json:"model"`
    APIKey *string `json:"api_key,omitempty"`
    SessionConfig domain.GASessionConfig `json:"session_config"`
    TransportConfig domain.ProviderTransportConfig `json:"transport_config"`
}
```

Create requires non-empty API key. Update preserves the existing encrypted key when omitted/blank. Validate URL syntax and both config structs before encryption/store mutation. Remove route registration and both generator files.

- [x] **Step 4: Update OpenAPI as the canonical external contract**

Define `GASessionConfig`, `ProviderTransportConfig`, provider auth enum, nullable/optional numeric fields with exact minima, and `revision`. Remove old enums and `/v1/config/mykey.py`. Update create/update schemas so update API key is optional.

- [x] **Step 5: Write failing Web type/build tests**

First update TypeScript types without page implementation and run:

```bash
cd tenant_platform/web
npm run build
```

Expected: type errors identify every old flat config and missing update API use.

- [x] **Step 6: Implement the Web form without unsupported fields**

Requirements:

- maintain separate `session_config` and `transport_config` state;
- preserve explicit zero using `value={value ?? ''}`;
- remove Top P, duplicate Timeout, and Extra Sys Prompt File;
- expose stream, Responses, auth mode, proxy URL, TLS verify, connect and response-header timeouts;
- add Edit command; blank key means preserve;
- use typed helpers instead of `as any`;
- show backend validation errors unchanged.

- [x] **Step 7: Build and browser-verify the admin workflow**

Run:

```bash
cd tenant_platform/web
npm run build
npm run dev -- --host 127.0.0.1
```

Use the browser tool at desktop and mobile widths. Verify create/edit/default/delete controls, conditional OAI/Claude fields, zero-valued inputs, no overlap, and no console errors. Stop the dev server after verification unless the user needs the URL.

- [x] **Step 8: Run API tests and commit Task 8**

```bash
cd tenant_platform/backend-go
go test ./internal/api -run 'Test.*LLMProvider|TestProviderConfigEndpointRemoved' -count=1
cd ../web
npm run build
```

Then:

```bash
git add tenant_platform/backend-go/internal/api tenant_platform/backend-go/internal/domain/llm_provider.go tenant_platform/contracts/openapi/platform.yaml tenant_platform/web/src/api/types.ts tenant_platform/web/src/api/providers.ts tenant_platform/web/src/features/admin/LLMProvidersPage.tsx tenant_platform/web/src/features/admin/LLMProviderForm.tsx tenant_platform/web/src/features/admin/AdminPages.css tenant_platform/web/src/components/layout/Layout.css tenant_platform/web/src/index.css tenant_platform/web/src/features/user/UserPages.css
git commit --only -m "feat: expose validated native llm provider configuration" -- tenant_platform/backend-go/internal/api tenant_platform/backend-go/internal/domain/llm_provider.go tenant_platform/contracts/openapi/platform.yaml tenant_platform/web/src/api/types.ts tenant_platform/web/src/api/providers.ts tenant_platform/web/src/features/admin/LLMProvidersPage.tsx tenant_platform/web/src/features/admin/LLMProviderForm.tsx tenant_platform/web/src/features/admin/AdminPages.css tenant_platform/web/src/components/layout/Layout.css tenant_platform/web/src/index.css tenant_platform/web/src/features/user/UserPages.css
```

### Task 9: End-to-End GA Contract, Security Cleanup, and Cutover Verification

**Files:**
- Modify: `tenant_platform/worker-python/tests/integration/test_worker_rpc.py`
- Modify: `tenant_platform/tests/integration/test_foundation_flow.py`
- Rewrite: `tenant_platform/tests/security/test_no_real_key_leak.py`
- Modify: `tenant_platform/tests/contract/test_contract_sources.py`
- Modify: `tenant_platform/tests/smoke/foundation_smoke.py`
- Remove: `tenant_platform/test-mykey-generation.sh`
- Remove: `tenant_platform/test-mykey-generation.ps1`
- Update affected existing docs only: `tenant_platform/QUICKSTART.md`, `tenant_platform/docs/LLM_PROVIDER_ARCHITECTURE.md`, `tenant_platform/docs/IMPLEMENTATION_SUMMARY.md`

**Interfaces:**
- Exercises the complete Scheduler -> runtime JSON -> Worker -> actual GA -> transparent Proxy -> fixture Provider path.
- Verifies no real key reaches Worker artifacts, environment, logs, or events.

- [ ] **Step 1: Add failing actual-GA integration scenarios**

Extend fixtures to support native chat, Responses, Claude x-api-key/Bearer, and SSE. Add tests for:

```text
chat JSON success
responses streaming success
claude beta headers and SSE
mixin primary failure -> fallback success
token refresh generation handshake
expired/revoked/wrong-model/wrong-revision rejection
provider default switch affects new Worker only
key rotation keeps same Provider session working
```

Every Worker test snapshots GA legacy files before/after and asserts unchanged.

- [ ] **Step 2: Run focused integrations and observe RED**

```bash
cd tenant_platform/worker-python
python -m pytest tests/integration/test_worker_rpc.py -q
cd ../
python -m pytest tests/integration/test_foundation_flow.py -q
```

Expected: new scenarios fail until every prior task is integrated and correctly assembled.

- [ ] **Step 3: Replace source-string security tests with behavior tests**

Security tests must inspect generated runtime directories and captured logs/events, not Go source strings. Assertions include:

```python
for path in config_root.rglob("*"):
    if path.is_file():
        assert real_upstream_key.encode() not in path.read_bytes()
assert real_upstream_key not in worker_output
assert capability_token not in proxy_logs
```

Retain one contract assertion that `/v1/config/mykey.py` is absent from OpenAPI; do not assert internal source ordering.

- [ ] **Step 4: Remove obsolete plaintext scripts and update affected docs**

Delete the two config download scripts. Update existing docs to describe JSON loader, JWT claims, transparent streaming, network allowlists, required task/token lifetime invariant, and clean cutover. Remove every command that downloads or prints a plaintext `mykey.py`.

- [ ] **Step 5: Run complete verification**

Prerequisites: export a valid `TEST_DATABASE_URL` and start no conflicting Platform/Proxy processes.

```bash
cd tenant_platform/backend-go
go test ./... -count=1
cd ../worker-python
python -m pytest tests -q
cd ../
python -m pytest tests/contract tests/security tests/integration tests/smoke -q
cd web
npm run build
```

Then run the real smoke path and observe an actual streamed chunk before terminal completion. Expected: all commands exit 0; no skipped security/integration contracts caused by missing prerequisites.

- [ ] **Step 6: Run diagnostics and formatters**

```bash
cd tenant_platform/backend-go
gofmt -w cmd internal
go vet ./...
cd ../worker-python
python -m compileall -q src
cd ../web
npm run lint
```

Expected: exit 0. Re-run the complete verification commands after formatter changes.

- [ ] **Step 7: Request independent code review**

Use the `requesting-code-review` skill and a reviewer subagent. Review against every acceptance criterion in `docs/superpowers/specs/2026-07-27-ga-transparent-llm-credential-proxy-design.md`, with special attention to secret flow, SSRF, JWT validation, POST replay, streaming, task deadline races, and GA Core immutability. Resolve all High/Medium findings before completion.

- [ ] **Step 8: Commit final integration and cleanup**

```bash
git add tenant_platform/worker-python/tests/integration/test_worker_rpc.py tenant_platform/tests/integration/test_foundation_flow.py tenant_platform/tests/security/test_no_real_key_leak.py tenant_platform/tests/contract/test_contract_sources.py tenant_platform/tests/smoke/foundation_smoke.py tenant_platform/QUICKSTART.md tenant_platform/docs/LLM_PROVIDER_ARCHITECTURE.md tenant_platform/docs/IMPLEMENTATION_SUMMARY.md
git rm tenant_platform/test-mykey-generation.sh tenant_platform/test-mykey-generation.ps1
git commit --only -m "test: verify transparent llm credential cutover" -- tenant_platform/worker-python/tests/integration/test_worker_rpc.py tenant_platform/tests/integration/test_foundation_flow.py tenant_platform/tests/security/test_no_real_key_leak.py tenant_platform/tests/contract/test_contract_sources.py tenant_platform/tests/smoke/foundation_smoke.py tenant_platform/test-mykey-generation.sh tenant_platform/test-mykey-generation.ps1 tenant_platform/QUICKSTART.md tenant_platform/docs/LLM_PROVIDER_ARCHITECTURE.md tenant_platform/docs/IMPLEMENTATION_SUMMARY.md
```

## Completion Evidence

Before declaring completion, record:

- migration applied successfully to an existing 0023 schema and a fresh database;
- Go full-suite test count and zero failures;
- Python Worker/platform full-suite test count and zero failures;
- Web build/lint success;
- browser verification viewports and observed admin workflows;
- streamed first-chunk timing proving no response buffering;
- captured Worker config/env/log scan proving no real key;
- reviewer findings and resolutions;
- final `git status --short` with the user's unrelated runtime artifacts untouched.
