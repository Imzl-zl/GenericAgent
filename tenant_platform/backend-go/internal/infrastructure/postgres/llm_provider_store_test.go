package postgres

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/domain"
)

func TestLLMProviderStoreRoutingRevisionAndKeyRotation(t *testing.T) {
	pool := requireDB(t)
	store, err := NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	zeroRetries := 0
	created, err := store.CreateProvider(ctx, domain.LLMProviderCreate{
		Name:             "primary",
		ProviderType:     domain.ProviderNativeOAI,
		BaseURL:          "https://api.openai.com/v1",
		Model:            "gpt-test",
		APIKeyCiphertext: []byte("cipher-v1"),
		APIKeyKeyVersion: "key-v1",
		SessionConfig:    domain.GASessionConfig{MaxRetries: &zeroRetries},
		TransportConfig:  domain.ProviderTransportConfig{AuthMode: domain.ProviderAuthAuto},
	})
	if err != nil {
		t.Fatal(err)
	}
	if created.Revision != 1 {
		t.Fatalf("initial revision = %d, want 1", created.Revision)
	}

	renamed, err := store.UpdateProvider(ctx, created.ID, domain.LLMProviderUpdate{
		LLMProviderCreate: domain.LLMProviderCreate{
			Name:            "primary-renamed",
			ProviderType:    created.ProviderType,
			BaseURL:         created.BaseURL,
			Model:           created.Model,
			SessionConfig:   created.SessionConfig,
			TransportConfig: created.TransportConfig,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if renamed.Revision != created.Revision {
		t.Fatalf("name-only revision = %d, want %d", renamed.Revision, created.Revision)
	}

	routed, err := store.UpdateProvider(ctx, created.ID, domain.LLMProviderUpdate{
		LLMProviderCreate: domain.LLMProviderCreate{
			Name:            renamed.Name,
			ProviderType:    renamed.ProviderType,
			BaseURL:         renamed.BaseURL,
			Model:           "gpt-next",
			SessionConfig:   renamed.SessionConfig,
			TransportConfig: renamed.TransportConfig,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if routed.Revision != created.Revision+1 {
		t.Fatalf("routing revision = %d, want %d", routed.Revision, created.Revision+1)
	}
	if !bytes.Equal(routed.APIKeyCiphertext, created.APIKeyCiphertext) {
		t.Fatal("routing update changed API key ciphertext")
	}

	rotated, err := store.UpdateProvider(ctx, created.ID, domain.LLMProviderUpdate{
		LLMProviderCreate: domain.LLMProviderCreate{
			Name:             routed.Name,
			ProviderType:     routed.ProviderType,
			BaseURL:          routed.BaseURL,
			Model:            routed.Model,
			APIKeyCiphertext: []byte("cipher-v2"),
			APIKeyKeyVersion: "key-v2",
			SessionConfig:    routed.SessionConfig,
			TransportConfig:  routed.TransportConfig,
		},
		RotateAPIKey: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if rotated.Revision != routed.Revision {
		t.Fatalf("key-only revision = %d, want %d", rotated.Revision, routed.Revision)
	}
	if !bytes.Equal(rotated.APIKeyCiphertext, []byte("cipher-v2")) {
		t.Fatalf("ciphertext = %q", rotated.APIKeyCiphertext)
	}
}

func TestLLMProviderStoreStateTransitions(t *testing.T) {
	pool := requireDB(t)
	store, err := NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	first, err := store.CreateProvider(ctx, providerCreate("state-first"))
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.CreateProvider(ctx, providerCreate("state-second"))
	if err != nil {
		t.Fatal(err)
	}

	disabled, err := store.SetProviderState(ctx, second.ID, domain.ProviderDisabled)
	if err != nil {
		t.Fatal(err)
	}
	if disabled.State != domain.ProviderDisabled || disabled.Revision != second.Revision+1 {
		t.Fatalf("disabled provider = %#v", disabled)
	}
	repeated, err := store.SetProviderState(ctx, second.ID, domain.ProviderDisabled)
	if err != nil {
		t.Fatal(err)
	}
	if repeated.Revision != disabled.Revision {
		t.Fatalf("idempotent disable revision = %d, want %d", repeated.Revision, disabled.Revision)
	}
	if err := store.SetDefaultProvider(ctx, second.ID); err == nil {
		t.Fatal("disabled provider became default")
	}

	active, err := store.SetProviderState(ctx, second.ID, domain.ProviderActive)
	if err != nil {
		t.Fatal(err)
	}
	if active.State != domain.ProviderActive || active.Revision != disabled.Revision+1 {
		t.Fatalf("enabled provider = %#v", active)
	}
	if _, err := store.SetProviderState(ctx, first.ID, domain.ProviderDisabled); err == nil {
		t.Fatal("default provider was disabled")
	}
	unchanged, err := store.GetProvider(ctx, first.ID)
	if err != nil {
		t.Fatal(err)
	}
	if unchanged.State != domain.ProviderActive || unchanged.Revision != first.Revision {
		t.Fatalf("default provider changed after rejected disable: %#v", unchanged)
	}
}

func TestLLMProviderStoreListsActiveDefaultFirst(t *testing.T) {
	pool := requireDB(t)
	store, err := NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	first, err := store.CreateProvider(ctx, providerCreate("first"))
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.CreateProvider(ctx, providerCreate("second"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetDefaultProvider(ctx, second.ID); err != nil {
		t.Fatal(err)
	}

	providers, err := store.ListActiveProviders(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(providers) != 2 {
		t.Fatalf("providers = %d, want 2", len(providers))
	}
	if providers[0].ID != second.ID || providers[1].ID != first.ID {
		t.Fatalf("order = [%d %d], want [%d %d]", providers[0].ID, providers[1].ID, second.ID, first.ID)
	}
}

func TestTransparentProxyMigrationPreservesSupportedConfig(t *testing.T) {
	pool := requireDB(t)
	ctx := context.Background()
	if err := ResetSchema(ctx, pool); err != nil {
		t.Fatal(err)
	}
	for _, name := range migrationFiles() {
		if name == "0024_transparent_llm_proxy.sql" {
			break
		}
		sqlBytes, err := os.ReadFile(filepath.Join(migrationsDir(), name))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, string(sqlBytes)); err != nil {
			t.Fatalf("apply %s: %v", name, err)
		}
	}

	legacyConfig := `{"temperature":0,"max_retries":0,"stream":true,"proxy":"http://proxy.example:8080","verify":false,"connect_timeout":9}`
	if _, err := pool.Exec(ctx, `
		INSERT INTO llm_providers (
			name, provider_type, base_url, model, api_key_ciphertext,
			api_key_key_version, config, is_default
		) VALUES ('legacy', 'native_oai', 'https://api.openai.com/v1', 'gpt-test', $1, 'key-v1', $2::jsonb, TRUE)
	`, []byte("cipher"), legacyConfig); err != nil {
		t.Fatal(err)
	}
	if err := EnsureSchema(ctx, pool, ""); err != nil {
		t.Fatal(err)
	}

	var sessionJSON, transportJSON []byte
	var revision int64
	if err := pool.QueryRow(ctx, `
		SELECT session_config, transport_config, revision
		FROM llm_providers WHERE name = 'legacy'
	`).Scan(&sessionJSON, &transportJSON, &revision); err != nil {
		t.Fatal(err)
	}
	var session map[string]any
	if err := json.Unmarshal(sessionJSON, &session); err != nil {
		t.Fatal(err)
	}
	if session["temperature"] != float64(0) || session["max_retries"] != float64(0) {
		t.Fatalf("session config lost explicit zero: %s", sessionJSON)
	}
	var transport map[string]any
	if err := json.Unmarshal(transportJSON, &transport); err != nil {
		t.Fatal(err)
	}
	if transport["auth_mode"] != "auto" || transport["proxy_url"] != "http://proxy.example:8080" || transport["tls_verify"] != false || transport["connect_timeout_seconds"] != float64(9) {
		t.Fatalf("transport config mismatch: %s", transportJSON)
	}
	if revision != 1 {
		t.Fatalf("revision = %d, want 1", revision)
	}
}

func providerCreate(name string) domain.LLMProviderCreate {
	return domain.LLMProviderCreate{
		Name:             name,
		ProviderType:     domain.ProviderNativeOAI,
		BaseURL:          "https://api.openai.com/v1",
		Model:            "gpt-test",
		APIKeyCiphertext: []byte("cipher"),
		APIKeyKeyVersion: "key-v1",
		TransportConfig:  domain.ProviderTransportConfig{AuthMode: domain.ProviderAuthAuto},
	}
}
