package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/application"
	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/infrastructure/policy"
	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/infrastructure/postgres"
)

func apiFixture(t *testing.T) (*Server, string) {
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
	dev, err := store.EnsureDevelopmentContext(context.Background(), 9, "api-dev")
	if err != nil {
		t.Fatal(err)
	}
	svc, err := application.NewTaskService(application.TaskServiceConfig{
		Store: store, Registry: reg, PlatformInstanceID: "api-inst", ClaimLease: time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	srv, err := NewServer(ServerConfig{
		Service: svc, Registry: reg, DevToken: "test-dev-token", DevUserID: 9, SessionKey: dev.SessionKey,
	})
	if err != nil {
		t.Fatal(err)
	}
	return srv, dev.SessionKey
}

func TestHealthzAndAuth(t *testing.T) {
	srv, sk := apiFixture(t)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rr.Code != 200 {
		t.Fatalf("health %d", rr.Code)
	}
	body := map[string]any{
		"message_id": "m1", "source_instance_id": "si", "prompt": "hi", "source": "web",
		"persona_snapshot": []string{"p"}, "tool_policy_version": "foundation.no-host-tools.v1",
	}
	raw, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/v1/sessions/"+sk+"/tasks", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	rr = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("want 401 got %d", rr.Code)
	}
}

func TestSubmitRejectsHostToolPolicy(t *testing.T) {
	srv, sk := apiFixture(t)
	body := map[string]any{
		"message_id": "m2", "source_instance_id": "si", "prompt": "hi", "source": "web",
		"persona_snapshot": []string{"p"}, "tool_policy_version": "other.host-tools.v1",
	}
	raw, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/v1/sessions/"+sk+"/tasks", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Platform-Dev-Token", "test-dev-token")
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code == http.StatusAccepted {
		t.Fatal("must reject unknown host tool policy")
	}
	var errBody map[string]any
	_ = json.Unmarshal(rr.Body.Bytes(), &errBody)
	if errBody["code"] == nil {
		t.Fatalf("expected error code: %s", rr.Body.String())
	}
}

func TestSubmit202AndGetTask(t *testing.T) {
	srv, sk := apiFixture(t)
	body := map[string]any{
		"message_id": "m3", "source_instance_id": "si", "prompt": "hello api", "source": "web",
		"persona_snapshot": []string{"p"}, "tool_policy_version": "foundation.no-host-tools.v1",
	}
	raw, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/v1/sessions/"+sk+"/tasks", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Platform-Dev-Token", "test-dev-token")
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusAccepted {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var resp map[string]any
	_ = json.Unmarshal(rr.Body.Bytes(), &resp)
	taskID, _ := resp["task_id"].(string)
	if taskID == "" || resp["status"] != "queued" {
		t.Fatalf("resp=%v", resp)
	}
	req = httptest.NewRequest(http.MethodGet, "/v1/tasks/"+taskID, nil)
	req.Header.Set("X-Platform-Dev-Token", "test-dev-token")
	rr = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("get %d", rr.Code)
	}
}

func TestResultRejectsPathLikeRef(t *testing.T) {
	srv, _ := apiFixture(t)
	req := httptest.NewRequest(http.MethodGet, "/v1/tasks/does-not-exist/result?result_ref=C:%5Csecrets", nil)
	req.Header.Set("X-Platform-Dev-Token", "test-dev-token")
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code == 200 {
		t.Fatal("path-like ref must be rejected")
	}
}
