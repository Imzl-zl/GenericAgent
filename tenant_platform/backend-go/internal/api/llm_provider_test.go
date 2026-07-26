package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/application"
	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/infrastructure/policy"
	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/infrastructure/postgres"
	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/infrastructure/secret"
)

func llmProviderServerFixture(t *testing.T) (*Server, *postgres.Store, string) {
	t.Helper()
	pool := postgres.OpenTestPool(t)
	store, err := postgres.NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	_, file, _, _ := runtime.Caller(0)
	polPath := filepath.Clean(filepath.Join(filepath.Dir(file), "..", "..", "..", "contracts", "policy", "foundation.v1.json"))
	reg, err := policy.LoadRegistry(polPath)
	if err != nil {
		t.Fatal(err)
	}
	dev, err := store.EnsureDevelopmentContext(context.Background(), 42, "llm-dev")
	if err != nil {
		t.Fatal(err)
	}
	svc, err := application.NewTaskService(application.TaskServiceConfig{
		Store: store, Registry: reg, PlatformInstanceID: "llm-inst", ClaimLease: time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	cipher, err := secret.NewStaticKeyCipherFromHex(strings.Repeat("0a", 32))
	if err != nil {
		t.Fatal(err)
	}
	srv, err := NewServer(ServerConfig{
		Service: svc, Registry: reg, LLMProviders: store, Cipher: cipher,
		DevToken: "test-dev-token", DevUserID: dev.UserID, SessionKey: dev.SessionKey,
	})
	if err != nil {
		t.Fatal(err)
	}
	return srv, store, dev.SessionKey
}

func TestAdminCreateLLMProviderEncryptsKey(t *testing.T) {
	srv, store, _ := llmProviderServerFixture(t)
	body := map[string]any{
		"name":          "openai-default",
		"provider_type": "openai_compatible",
		"base_url":      "https://api.openai.com/v1",
		"model":         "gpt-4o-mini",
		"api_key":       "sk-real-upstream-key",
	}
	raw, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/v1/admin/llm-providers", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Platform-Dev-Token", "test-dev-token")
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp["name"] != "openai-default" {
		t.Fatalf("unexpected name: %v", resp["name"])
	}
	if _, ok := resp["api_key"]; ok {
		t.Fatal("api_key must not be returned in response")
	}

	id := int64(resp["provider_id"].(float64))
	provider, err := store.GetProvider(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	plain, err := srv.cipher.Decrypt(provider.APIKeyCiphertext, atoi(provider.APIKeyKeyVersion))
	if err != nil {
		t.Fatal(err)
	}
	if string(plain) != "sk-real-upstream-key" {
		t.Fatalf("api key round-trip failed: %s", string(plain))
	}
}

func TestAdminCreateLLMProviderRejectsInvalidType(t *testing.T) {
	srv, _, _ := llmProviderServerFixture(t)
	body := map[string]any{
		"name":          "bad",
		"provider_type": "unknown",
		"base_url":      "https://x",
		"model":         "m",
		"api_key":       "k",
	}
	raw, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/v1/admin/llm-providers", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Platform-Dev-Token", "test-dev-token")
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("want 400 got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestAdminLLMProviderLifecycle(t *testing.T) {
	srv, _, _ := llmProviderServerFixture(t)
	devToken := "test-dev-token"

	// Create.
	createBody := map[string]any{
		"name":          "anthropic-default",
		"provider_type": "anthropic_messages",
		"base_url":      "https://api.anthropic.com",
		"model":         "claude-3-5-sonnet",
		"api_key":       "sk-ant",
	}
	raw, _ := json.Marshal(createBody)
	req := httptest.NewRequest(http.MethodPost, "/v1/admin/llm-providers", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Platform-Dev-Token", devToken)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("create status=%d body=%s", rr.Code, rr.Body.String())
	}
	var createResp map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &createResp); err != nil {
		t.Fatal(err)
	}
	id := int64(createResp["provider_id"].(float64))
	if createResp["is_default"] != true {
		t.Fatalf("first provider should be default")
	}

	// List.
	req = httptest.NewRequest(http.MethodGet, "/v1/admin/llm-providers", nil)
	req.Header.Set("X-Platform-Dev-Token", devToken)
	rr = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("list status=%d body=%s", rr.Code, rr.Body.String())
	}
	var listResp map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &listResp); err != nil {
		t.Fatal(err)
	}
	if len(listResp["providers"].([]any)) != 1 {
		t.Fatalf("want 1 provider, got %v", listResp["providers"])
	}

	// Get.
	req = httptest.NewRequest(http.MethodGet, "/v1/admin/llm-providers/"+itoa(id), nil)
	req.Header.Set("X-Platform-Dev-Token", devToken)
	rr = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("get status=%d body=%s", rr.Code, rr.Body.String())
	}

	// Update.
	updateBody := map[string]any{
		"name":          "anthropic-updated",
		"provider_type": "anthropic_messages",
		"base_url":      "https://api.anthropic.com",
		"model":         "claude-3-opus",
		"api_key":       "sk-ant-2",
	}
	raw, _ = json.Marshal(updateBody)
	req = httptest.NewRequest(http.MethodPut, "/v1/admin/llm-providers/"+itoa(id), bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Platform-Dev-Token", devToken)
	rr = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("update status=%d body=%s", rr.Code, rr.Body.String())
	}
	var updateResp map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &updateResp); err != nil {
		t.Fatal(err)
	}
	if updateResp["model"] != "claude-3-opus" {
		t.Fatalf("model not updated: %v", updateResp["model"])
	}

	// Set default (idempotent for single provider).
	req = httptest.NewRequest(http.MethodPost, "/v1/admin/llm-providers/"+itoa(id)+"/default", nil)
	req.Header.Set("X-Platform-Dev-Token", devToken)
	rr = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("set default status=%d body=%s", rr.Code, rr.Body.String())
	}

	// Delete.
	req = httptest.NewRequest(http.MethodDelete, "/v1/admin/llm-providers/"+itoa(id), nil)
	req.Header.Set("X-Platform-Dev-Token", devToken)
	rr = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("delete status=%d body=%s", rr.Code, rr.Body.String())
	}

	// List empty.
	req = httptest.NewRequest(http.MethodGet, "/v1/admin/llm-providers", nil)
	req.Header.Set("X-Platform-Dev-Token", devToken)
	rr = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("list empty status=%d body=%s", rr.Code, rr.Body.String())
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &listResp); err != nil {
		t.Fatal(err)
	}
	if len(listResp["providers"].([]any)) != 0 {
		t.Fatalf("want 0 providers, got %v", listResp["providers"])
	}
}

func TestAdminLLMProviderRequiresAuth(t *testing.T) {
	srv, _, _ := llmProviderServerFixture(t)
	req := httptest.NewRequest(http.MethodGet, "/v1/admin/llm-providers", nil)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("want 401 got %d", rr.Code)
	}
}

func atoi(s string) int {
	v, _ := strconv.Atoi(s)
	return v
}

func itoa(v int64) string {
	return strconv.FormatInt(v, 10)
}
