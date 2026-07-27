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
		"provider_type": "native_oai",
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
		"provider_type": "native_claude",
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
		"provider_type": "native_claude",
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

func TestAdminUpdateLLMProviderOmittedOrBlankKeyPreservesCiphertext(t *testing.T) {
	for _, keyMode := range []string{"omitted", "blank"} {
		t.Run(keyMode, func(t *testing.T) {
			srv, store, _ := llmProviderServerFixture(t)
			created := performProviderWrite(t, srv, http.MethodPost, "/v1/admin/llm-providers", providerWritePayload("preserve-"+keyMode))
			if created.Code != http.StatusCreated {
				t.Fatalf("create=%d body=%s", created.Code, created.Body.String())
			}
			var reply map[string]any
			if err := json.Unmarshal(created.Body.Bytes(), &reply); err != nil {
				t.Fatal(err)
			}
			id := int64(reply["provider_id"].(float64))
			before, err := store.GetProvider(context.Background(), id)
			if err != nil {
				t.Fatal(err)
			}
			update := providerWritePayload("preserve-" + keyMode + "-updated")
			delete(update, "api_key")
			if keyMode == "blank" {
				update["api_key"] = "   "
			}
			updated := performProviderWrite(t, srv, http.MethodPut, "/v1/admin/llm-providers/"+itoa(id), update)
			if updated.Code != http.StatusOK {
				t.Fatalf("update=%d body=%s", updated.Code, updated.Body.String())
			}
			after, err := store.GetProvider(context.Background(), id)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(after.APIKeyCiphertext, before.APIKeyCiphertext) || after.APIKeyKeyVersion != before.APIKeyKeyVersion {
				t.Fatal("omitted or blank api_key rotated stored credentials")
			}
		})
	}
}

func TestAdminCreateLLMProviderValidatesNestedConfiguration(t *testing.T) {
	srv, _, _ := llmProviderServerFixture(t)
	tests := []struct {
		name   string
		mutate func(map[string]any)
	}{
		{name: "relative base URL", mutate: func(body map[string]any) { body["base_url"] = "not-an-absolute-url" }},
		{name: "thinking budget required", mutate: func(body map[string]any) {
			body["session_config"] = map[string]any{"thinking_type": "enabled"}
		}},
		{name: "Claude rejects OAI mode", mutate: func(body map[string]any) {
			body["provider_type"] = "native_claude"
			body["session_config"] = map[string]any{"api_mode": "responses"}
		}},
		{name: "transport timeout positive", mutate: func(body map[string]any) {
			body["transport_config"] = map[string]any{"auth_mode": "auto", "connect_timeout_seconds": 0}
		}},
		{name: "proxy URL has credentials and path", mutate: func(body map[string]any) {
			body["transport_config"] = map[string]any{
				"auth_mode": "auto", "proxy_url": "http://user:pass@proxy.example/internal?token=x",
			}
		}},
		{name: "unknown nested field", mutate: func(body map[string]any) {
			body["session_config"] = map[string]any{"not_a_ga_field": true}
		}},
	}
	for index, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			body := providerWritePayload("invalid-" + strconv.Itoa(index))
			test.mutate(body)
			response := performProviderWrite(t, srv, http.MethodPost, "/v1/admin/llm-providers", body)
			if response.Code != http.StatusBadRequest {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}
		})
	}
}

func TestAdminCreateLLMProviderPreservesExplicitZeroAndOmitsSecrets(t *testing.T) {
	srv, _, _ := llmProviderServerFixture(t)
	body := providerWritePayload("explicit-zero")
	body["session_config"] = map[string]any{
		"temperature": 0, "max_retries": 0, "trim_keep_prefix": 0, "stream": false,
	}
	body["transport_config"] = map[string]any{"auth_mode": "auto", "tls_verify": false}
	response := performProviderWrite(t, srv, http.MethodPost, "/v1/admin/llm-providers", body)
	if response.Code != http.StatusCreated {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	var reply map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &reply); err != nil {
		t.Fatal(err)
	}
	session := reply["session_config"].(map[string]any)
	transport := reply["transport_config"].(map[string]any)
	if session["temperature"] != float64(0) || session["max_retries"] != float64(0) || session["stream"] != false {
		t.Fatalf("explicit session values lost: %v", session)
	}
	if transport["tls_verify"] != false || reply["revision"].(float64) <= 0 {
		t.Fatalf("explicit transport/revision values lost: transport=%v revision=%v", transport, reply["revision"])
	}
	for _, field := range []string{"api_key", "api_key_ciphertext", "api_key_key_version"} {
		if _, present := reply[field]; present {
			t.Fatalf("secret field %q returned: %v", field, reply[field])
		}
	}
}

func TestProviderConfigEndpointRemoved(t *testing.T) {
	srv, _, _ := llmProviderServerFixture(t)
	request := httptest.NewRequest(http.MethodGet, "/v1/config/mykey.py", nil)
	request.Header.Set("X-Platform-Dev-Token", "test-dev-token")
	response := httptest.NewRecorder()
	srv.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusNotFound {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
}

func providerWritePayload(name string) map[string]any {
	return map[string]any{
		"name": name, "provider_type": "native_oai", "base_url": "https://api.openai.com/v1",
		"model": "gpt-test", "api_key": "sk-original", "session_config": map[string]any{},
		"transport_config": map[string]any{"auth_mode": "auto"},
	}
}

func performProviderWrite(
	t *testing.T,
	srv *Server,
	method string,
	path string,
	body map[string]any,
) *httptest.ResponseRecorder {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(method, path, bytes.NewReader(raw))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Platform-Dev-Token", "test-dev-token")
	response := httptest.NewRecorder()
	srv.Handler().ServeHTTP(response, request)
	return response
}

func atoi(s string) int {
	v, _ := strconv.Atoi(s)
	return v
}

func itoa(v int64) string {
	return strconv.FormatInt(v, 10)
}
