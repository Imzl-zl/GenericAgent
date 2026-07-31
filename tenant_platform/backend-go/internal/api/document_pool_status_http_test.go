package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/domain"
)

type fakeDocumentPoolStatusStore struct {
	status domain.DocumentPoolStatus
	err    error
}

func (f fakeDocumentPoolStatusStore) GetDocumentPoolStatus(context.Context) (domain.DocumentPoolStatus, error) {
	return f.status, f.err
}

func TestDocumentPoolStatusEndpointIsAdminOnlyAndAggregate(t *testing.T) {
	oldest := time.Date(2026, 7, 31, 1, 2, 3, 0, time.UTC)
	observed := oldest.Add(time.Hour)
	srv, err := NewServer(ServerConfig{
		Service: dashboardFakeTaskService{}, Registry: dashboardFakeRegistry{},
		DocumentPoolStatus: fakeDocumentPoolStatusStore{status: domain.DocumentPoolStatus{
			JobsQueued: 2, JobsStarting: 1, JobsRunning: 1,
			InstancesCreating: 1, InstancesReady: 2, InstancesAllocated: 1,
			InstancesRunning: 1, InstancesDestroying: 1, InstancesLost: 0,
			CommandsPending: 3, CommandsExecuting: 1,
			OldestQueuedAt: &oldest, ObservedAt: observed,
		}},
		DevToken: "status-admin-token", DevUserID: 9,
	})
	if err != nil {
		t.Fatal(err)
	}

	unauthorized := httptest.NewRecorder()
	srv.Handler().ServeHTTP(unauthorized, httptest.NewRequest(http.MethodGet, "/v1/admin/document-pool/status", nil))
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized status=%d body=%s", unauthorized.Code, unauthorized.Body.String())
	}

	request := httptest.NewRequest(http.MethodGet, "/v1/admin/document-pool/status", nil)
	request.Header.Set("X-Platform-Dev-Token", "status-admin-token")
	response := httptest.NewRecorder()
	srv.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"jobs_queued", "instances_ready", "commands_executing", "oldest_queued_at", "observed_at"} {
		if _, ok := body[field]; !ok {
			t.Fatalf("missing aggregate field %s: %v", field, body)
		}
	}
	lowered := strings.ToLower(response.Body.String())
	for _, forbidden := range []string{"job_id", "workspace_id", "requester", "payload", "slot_path", "instance_name", "runtime_id", "artifact"} {
		if strings.Contains(lowered, forbidden) {
			t.Fatalf("response leaks %s: %s", forbidden, response.Body.String())
		}
	}
}
