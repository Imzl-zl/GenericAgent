package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/domain"
)

func apiDocumentPoolSettings() domain.DocumentPoolSettings {
	return domain.DocumentPoolSettings{
		Enabled: true, MaxActive: 2, MinReady: 1,
		JobIdleTTLSeconds: 600, ReadyIdleTTLSeconds: 300,
		GlobalQueueLimit: 100, PerTenantQueueLimit: 20, PerTenantActiveLimit: 1,
		JobTimeoutSeconds: 3600, CommandTimeoutSeconds: 300,
		Version: 3, UpdatedBy: 7, UpdatedAt: time.Date(2026, 7, 29, 11, 0, 0, 0, time.UTC), Reason: "initial",
	}
}

func updateDocumentPoolBody(settings domain.DocumentPoolSettings, expectedVersion int64, reason string) updateDocumentPoolSettingsBody {
	return updateDocumentPoolSettingsBody{
		Enabled: settings.Enabled, MaxActive: settings.MaxActive, MinReady: settings.MinReady,
		JobIdleTTLSeconds: settings.JobIdleTTLSeconds, ReadyIdleTTLSeconds: settings.ReadyIdleTTLSeconds,
		GlobalQueueLimit: settings.GlobalQueueLimit, PerTenantQueueLimit: settings.PerTenantQueueLimit,
		PerTenantActiveLimit: settings.PerTenantActiveLimit, JobTimeoutSeconds: settings.JobTimeoutSeconds,
		CommandTimeoutSeconds: settings.CommandTimeoutSeconds, ExpectedVersion: expectedVersion, Reason: reason,
	}
}

func newDocumentPoolSettingsServer(t *testing.T, store *fakeRuntimeSettingsStore, runtime DocumentPoolSettingsRuntime) *Server {
	t.Helper()
	if runtime == nil {
		runtime = &fakeDocumentPoolRuntime{store: store}
	}
	srv, err := NewServer(ServerConfig{
		Service: dashboardFakeTaskService{}, Registry: dashboardFakeRegistry{},
		RuntimeSettings: store, DocumentPoolSettingsRuntime: runtime,
		DocumentPoolDeploymentMaxActive: 4,
		DevToken:                        "test-dev-token", DevUserID: 9,
	})
	if err != nil {
		t.Fatal(err)
	}
	return srv
}

func TestDocumentPoolSettingsServerRejectsInvalidRuntimeConfiguration(t *testing.T) {
	store := &fakeRuntimeSettingsStore{documentPool: apiDocumentPoolSettings()}
	for name, config := range map[string]ServerConfig{
		"nonpositive deployment limit": {
			Service: dashboardFakeTaskService{}, Registry: dashboardFakeRegistry{}, RuntimeSettings: store,
			DocumentPoolSettingsRuntime: &fakeDocumentPoolRuntime{store: store}, DocumentPoolDeploymentMaxActive: -1,
			DevToken: "test-dev-token", DevUserID: 9,
		},
		"missing runtime": {
			Service: dashboardFakeTaskService{}, Registry: dashboardFakeRegistry{}, RuntimeSettings: store,
			DocumentPoolDeploymentMaxActive: 4, DevToken: "test-dev-token", DevUserID: 9,
		},
		"missing store": {
			Service: dashboardFakeTaskService{}, Registry: dashboardFakeRegistry{},
			DocumentPoolSettingsRuntime: &fakeDocumentPoolRuntime{store: store}, DocumentPoolDeploymentMaxActive: 4,
			DevToken: "test-dev-token", DevUserID: 9,
		},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := NewServer(config); err == nil {
				t.Fatal("expected invalid document pool runtime configuration to fail")
			}
		})
	}
}

func TestServerWithoutDocumentPoolRuntimeDoesNotRegisterDocumentPoolRoutes(t *testing.T) {
	srv, err := NewServer(ServerConfig{
		Service: dashboardFakeTaskService{}, Registry: dashboardFakeRegistry{},
		RuntimeSettings: &fakeRuntimeSettingsStore{windowMS: 2500, maxTurns: 80},
		DevToken:        "test-dev-token", DevUserID: 9,
	})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/v1/admin/settings/document-pool", nil)
	req.Header.Set("X-Platform-Dev-Token", "test-dev-token")
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
}

func documentPoolRequest(t *testing.T, srv *Server, method string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var payload *bytes.Reader
	if body == nil {
		payload = bytes.NewReader(nil)
	} else {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		payload = bytes.NewReader(raw)
	}
	req := httptest.NewRequest(method, "/v1/admin/settings/document-pool", payload)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Platform-Dev-Token", "test-dev-token")
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	return rr
}

func TestAdminDocumentPoolSettingsGetReturnsAggregateAndDeploymentLimit(t *testing.T) {
	store := &fakeRuntimeSettingsStore{documentPool: apiDocumentPoolSettings()}
	rr := documentPoolRequest(t, newDocumentPoolSettingsServer(t, store, nil), http.MethodGet, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var got struct {
		domain.DocumentPoolSettings
		DeploymentMaxActive int `json:"deployment_max_active"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.MaxActive != 2 || got.Version != 3 || got.Reason != "initial" || got.DeploymentMaxActive != 4 {
		t.Fatalf("reply=%+v", got)
	}
}

func TestAdminDocumentPoolSettingsPutUsesCASAndAppliesAfterStorage(t *testing.T) {
	store := &fakeRuntimeSettingsStore{documentPool: apiDocumentPoolSettings()}
	runtime := &fakeDocumentPoolRuntime{store: store}
	input := apiDocumentPoolSettings()
	input.MaxActive = 3
	input.MinReady = 2
	input.PerTenantActiveLimit = 2
	body := updateDocumentPoolBody(input, 3, "scale for batch")
	rr := documentPoolRequest(t, newDocumentPoolSettingsServer(t, store, runtime), http.MethodPut, body)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	if store.documentPool.Version != 4 || store.documentPool.UpdatedBy != 9 || store.documentPool.Reason != "scale for batch" {
		t.Fatalf("stored=%+v", store.documentPool)
	}
	if runtime.applied.Version != 4 || runtime.applied.MaxActive != 3 {
		t.Fatalf("applied=%+v", runtime.applied)
	}
}

func TestAdminDocumentPoolSettingsPutRejectsInvalidAndHardMax(t *testing.T) {
	for name, maxActive := range map[string]int{"invalid combination": 0, "hard max": 5} {
		t.Run(name, func(t *testing.T) {
			store := &fakeRuntimeSettingsStore{documentPool: apiDocumentPoolSettings()}
			input := apiDocumentPoolSettings()
			input.MaxActive = maxActive
			rr := documentPoolRequest(t, newDocumentPoolSettingsServer(t, store, nil), http.MethodPut,
				updateDocumentPoolBody(input, 3, "change"))
			if rr.Code != http.StatusBadRequest {
				t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
			}
			if store.documentPool.Version != 3 {
				t.Fatalf("invalid request persisted: %+v", store.documentPool)
			}
		})
	}
}

func TestAdminDocumentPoolSettingsPutValidatesReasonByCharacters(t *testing.T) {
	store := &fakeRuntimeSettingsStore{documentPool: apiDocumentPoolSettings()}
	input := apiDocumentPoolSettings()
	valid := documentPoolRequest(t, newDocumentPoolSettingsServer(t, store, nil), http.MethodPut,
		updateDocumentPoolBody(input, 3, strings.Repeat("文", 500)))
	if valid.Code != http.StatusOK {
		t.Fatalf("500-character reason status=%d body=%s", valid.Code, valid.Body.String())
	}

	tooLong := documentPoolRequest(t, newDocumentPoolSettingsServer(t, store, nil), http.MethodPut,
		updateDocumentPoolBody(input, 4, strings.Repeat("文", 501)))
	if tooLong.Code != http.StatusBadRequest {
		t.Fatalf("501-character reason status=%d body=%s", tooLong.Code, tooLong.Body.String())
	}
}

func TestAdminDocumentPoolSettingsPutReturnsConflict(t *testing.T) {
	store := &fakeRuntimeSettingsStore{documentPool: apiDocumentPoolSettings(), updateErr: domain.ErrDocumentPoolSettingsConflict}
	input := apiDocumentPoolSettings()
	rr := documentPoolRequest(t, newDocumentPoolSettingsServer(t, store, nil), http.MethodPut,
		updateDocumentPoolBody(input, 2, "stale"))
	if rr.Code != http.StatusConflict {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
}

func TestAdminDocumentPoolSettingsApplyFailureReturnsPersistedVersionPendingRetry(t *testing.T) {
	store := &fakeRuntimeSettingsStore{documentPool: apiDocumentPoolSettings()}
	runtime := documentPoolRuntimeFunc(func(context.Context, domain.DocumentPoolSettings) error { return errors.New("manager unavailable") })
	input := apiDocumentPoolSettings()
	rr := documentPoolRequest(t, newDocumentPoolSettingsServer(t, store, runtime), http.MethodPut,
		updateDocumentPoolBody(input, 3, "apply"))
	if rr.Code != http.StatusAccepted {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	if store.documentPool.Version != 4 {
		t.Fatalf("settings were not persisted before apply: %+v", store.documentPool)
	}
	var reply documentPoolSettingsReply
	if err := json.Unmarshal(rr.Body.Bytes(), &reply); err != nil {
		t.Fatal(err)
	}
	if reply.Version != 4 || reply.ApplyStatus != documentPoolApplyPendingRetry {
		t.Fatalf("reply=%+v", reply)
	}
}

func TestAdminDocumentPoolSettingsApplySurvivesRequestCancellationAfterPersist(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	store := &fakeRuntimeSettingsStore{
		documentPool: apiDocumentPoolSettings(),
		afterUpdate:  cancel,
	}
	runtime := documentPoolRuntimeFunc(func(ctx context.Context, _ domain.DocumentPoolSettings) error {
		return ctx.Err()
	})
	input := apiDocumentPoolSettings()
	body, err := json.Marshal(updateDocumentPoolBody(input, 3, "apply after persistence"))
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPut, "/v1/admin/settings/document-pool", bytes.NewReader(body)).WithContext(ctx)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Platform-Dev-Token", "test-dev-token")
	rr := httptest.NewRecorder()
	newDocumentPoolSettingsServer(t, store, runtime).Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
}

type documentPoolRuntimeFunc func(context.Context, domain.DocumentPoolSettings) error

func (f documentPoolRuntimeFunc) ApplyDocumentPoolSettings(ctx context.Context, settings domain.DocumentPoolSettings) error {
	return f(ctx, settings)
}
