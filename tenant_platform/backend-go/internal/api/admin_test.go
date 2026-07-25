package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/application"
	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/policy"
	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/postgres"
	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/secret"
)

func botServerFixture(t *testing.T) (*Server, *postgres.Store, string) {
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
	dev, err := store.EnsureDevelopmentContext(context.Background(), 42, "bot-dev")
	if err != nil {
		t.Fatal(err)
	}
	svc, err := application.NewTaskService(application.TaskServiceConfig{
		Store: store, Registry: reg, PlatformInstanceID: "bot-inst", ClaimLease: time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	cipher, err := secret.NewStaticKeyCipherFromHex(strings.Repeat("0a", 32))
	if err != nil {
		t.Fatal(err)
	}
	srv, err := NewServer(ServerConfig{
		Service: svc, Registry: reg, Bots: store, Cipher: cipher,
		DevToken: "test-dev-token", DevUserID: dev.UserID, SessionKey: dev.SessionKey,
	})
	if err != nil {
		t.Fatal(err)
	}
	return srv, store, dev.SessionKey
}

func TestAdminCreateBotEncryptsToken(t *testing.T) {
	srv, store, _ := botServerFixture(t)
	body := map[string]any{
		"owner_id": 42,
		"bot_uuid": "11111111-1111-1111-1111-111111111111",
		"token":    "ilink-secret-token",
	}
	raw, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/v1/admin/bots", bytes.NewReader(raw))
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
	if resp["state"] != "active" {
		t.Fatalf("unexpected state: %v", resp["state"])
	}
	botUUID := body["bot_uuid"].(string)
	bot, err := store.GetBotByUUID(context.Background(), botUUID)
	if err != nil {
		t.Fatal(err)
	}
	cipher := srv.cipher
	plain, err := cipher.Decrypt(bot.TokenCiphertext, bot.TokenKeyVersion)
	if err != nil {
		t.Fatal(err)
	}
	if string(plain) != "ilink-secret-token" {
		t.Fatalf("token round-trip failed: %s", string(plain))
	}
}

func TestAdminCreateBotRejectsMissingFields(t *testing.T) {
	srv, _, _ := botServerFixture(t)
	body := map[string]any{
		"owner_id": 42,
		"bot_uuid": "",
		"token":    "secret",
	}
	raw, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/v1/admin/bots", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Platform-Dev-Token", "test-dev-token")
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("want 400 got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestAdminCreateBotRequiresAuth(t *testing.T) {
	srv, _, _ := botServerFixture(t)
	body := map[string]any{
		"owner_id": 42,
		"bot_uuid": "11111111-1111-1111-1111-111111111111",
		"token":    "secret",
	}
	raw, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/v1/admin/bots", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("want 401 got %d", rr.Code)
	}
}
