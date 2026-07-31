package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/domain"
)

type fakeSophubAPIService struct {
	status     domain.SophubBindingStatus
	candidates []domain.SOPCandidate
	registry   []domain.SOPRegistryItem
	loaded     []domain.SOPVersion
	boundKey   string
	approvedID string
	rejectedID string
	loadedID   string
	unloadedID string
	importedID string
}

func (service *fakeSophubAPIService) Bind(_ context.Context, key string, _ int64) (domain.SophubBindingStatus, error) {
	service.boundKey = key
	return service.status, nil
}
func (service *fakeSophubAPIService) GetBindingStatus(context.Context) (domain.SophubBindingStatus, error) {
	return service.status, nil
}
func (service *fakeSophubAPIService) Search(context.Context, string, int, int) (domain.SophubSearchResult, error) {
	return domain.SophubSearchResult{}, nil
}
func (service *fakeSophubAPIService) ImportCandidate(_ context.Context, remoteID string) (domain.SOPCandidate, error) {
	service.importedID = remoteID
	return domain.SOPCandidate{ID: "candidate-1", RemoteSOPID: remoteID}, nil
}
func (service *fakeSophubAPIService) ListCandidates(context.Context, domain.SOPCandidateStatus) ([]domain.SOPCandidate, error) {
	return service.candidates, nil
}
func (service *fakeSophubAPIService) ApproveCandidate(_ context.Context, id string, _ int64) (domain.SOPVersion, error) {
	service.approvedID = id
	return domain.SOPVersion{ID: "version-1", EntryID: "entry-1", CandidateID: id, Version: 1, Title: "Report", Content: "# Report\n", ContentDigest: strings.Repeat("a", 64)}, nil
}
func (service *fakeSophubAPIService) RejectCandidate(_ context.Context, id string, _ int64, _ string) error {
	service.rejectedID = id
	return nil
}
func (service *fakeSophubAPIService) ListRegistry(context.Context) ([]domain.SOPRegistryItem, error) {
	return service.registry, nil
}
func (service *fakeSophubAPIService) LoadVersion(_ context.Context, id string, _ int64) (domain.SOPEntry, error) {
	service.loadedID = id
	return domain.SOPEntry{ID: "entry-1", LoadedVersionID: id}, nil
}
func (service *fakeSophubAPIService) Unload(_ context.Context, id string, _ int64) (domain.SOPEntry, error) {
	service.unloadedID = id
	return domain.SOPEntry{ID: id}, nil
}
func (service *fakeSophubAPIService) ListLoaded(context.Context) ([]domain.SOPVersion, error) {
	return service.loaded, nil
}

func TestSophubAdminRoutesAreStrictAndSecretSafe(t *testing.T) {
	service := &fakeSophubAPIService{status: domain.SophubBindingStatus{
		Configured: true, AuthorType: "agent", AgentUID: "agent-1", DisplayName: "platform", UpdatedAt: time.Now().UTC(),
	}}
	server := &Server{sophub: service, devToken: "admin-token", devUserID: 42, mux: http.NewServeMux()}
	server.registerLifecycleRoutes()

	const key = "sophub-secret-sentinel"
	request := httptest.NewRequest(http.MethodPut, "/v1/admin/sophub/binding", bytes.NewBufferString(`{"api_key":"`+key+`"}`))
	request.Header.Set("X-Platform-Dev-Token", "admin-token")
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK || service.boundKey != key || strings.Contains(response.Body.String(), key) || strings.Contains(response.Body.String(), "cipher") {
		t.Fatalf("code=%d body=%s bound=%q", response.Code, response.Body.String(), service.boundKey)
	}

	request = httptest.NewRequest(http.MethodPut, "/v1/admin/sophub/binding", bytes.NewBufferString(`{"api_key":"x","unknown":true}`))
	request.Header.Set("X-Platform-Dev-Token", "admin-token")
	response = httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("unknown field code=%d body=%s", response.Code, response.Body.String())
	}
}

func TestTenantCannotManageSOPAndLoadedListIsReadOnly(t *testing.T) {
	service := &fakeSophubAPIService{loaded: []domain.SOPVersion{{
		ID: "version-1", EntryID: "entry-1", Version: 1, Title: "Report", Description: "Use for reports",
		Content: "# Report\n", ContentDigest: strings.Repeat("a", 64),
	}}}
	server := &Server{sophub: service, devToken: "admin-token", devUserID: 42, mux: http.NewServeMux()}
	server.registerLifecycleRoutes()

	request := httptest.NewRequest(http.MethodPost, "/v1/admin/sop-candidates/candidate-1/approve", nil)
	request.Header.Set("Authorization", "Bearer tenant-token")
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized || service.approvedID != "" {
		t.Fatalf("code=%d approved=%q", response.Code, service.approvedID)
	}

	request = httptest.NewRequest(http.MethodGet, "/v1/sops", nil)
	request = request.WithContext(context.WithValue(request.Context(), ctxUserIDKey, int64(7)))
	response = httptest.NewRecorder()
	server.handleListLoadedSOPs(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", response.Code, response.Body.String())
	}
	var body struct {
		SOPs []map[string]any `json:"sops"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil || len(body.SOPs) != 1 {
		t.Fatalf("body=%s err=%v", response.Body.String(), err)
	}
	if body.SOPs[0]["content"] != "# Report\n" || body.SOPs[0]["digest"] != strings.Repeat("a", 64) {
		t.Fatalf("sop=%+v", body.SOPs[0])
	}
}

func TestSophubAdminReviewAndLoadRoutes(t *testing.T) {
	service := &fakeSophubAPIService{}
	server := &Server{sophub: service, devToken: "admin-token", devUserID: 42, mux: http.NewServeMux()}
	server.registerLifecycleRoutes()

	for _, test := range []struct {
		method string
		path   string
		body   string
		check  func() bool
	}{
		{http.MethodPost, "/v1/admin/sophub/candidates/import", `{"remote_sop_id":"remote-1"}`, func() bool { return service.importedID == "remote-1" }},
		{http.MethodPost, "/v1/admin/sop-candidates/candidate-1/approve", ``, func() bool { return service.approvedID == "candidate-1" }},
		{http.MethodPost, "/v1/admin/sop-versions/version-1/load", ``, func() bool { return service.loadedID == "version-1" }},
		{http.MethodPost, "/v1/admin/sops/entry-1/unload", ``, func() bool { return service.unloadedID == "entry-1" }},
	} {
		request := httptest.NewRequest(test.method, test.path, bytes.NewBufferString(test.body))
		request.Header.Set("X-Platform-Dev-Token", "admin-token")
		response := httptest.NewRecorder()
		server.Handler().ServeHTTP(response, request)
		if response.Code < 200 || response.Code >= 300 || !test.check() {
			t.Fatalf("%s %s code=%d body=%s", test.method, test.path, response.Code, response.Body.String())
		}
	}
}
