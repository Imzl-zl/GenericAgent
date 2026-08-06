package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/domain"
)

type fakeSophubAPIService struct {
	bindingStatus domain.SophubBindingStatus
	searchResult  domain.SophubSearchResult
	boundKey      string
}

func (service *fakeSophubAPIService) Bind(_ context.Context, key string, _ int64) (domain.SophubBindingStatus, error) {
	service.boundKey = key
	return domain.SophubBindingStatus{Configured: true, DisplayName: "sophub-admin"}, nil
}

func (service *fakeSophubAPIService) GetBindingStatus(context.Context) (domain.SophubBindingStatus, error) {
	return service.bindingStatus, nil
}

func (service *fakeSophubAPIService) Search(context.Context, string, int, int) (domain.SophubSearchResult, error) {
	return service.searchResult, nil
}

func (service *fakeSophubAPIService) FetchRemoteSOP(_ context.Context, remoteID string) (domain.SophubRemoteSOP, error) {
	return domain.SophubRemoteSOP{ID: remoteID, FileType: "markdown", Status: "approved", Content: "x"}, nil
}

func newSophubTestServer(t *testing.T, service *fakeSophubAPIService) *Server {
	t.Helper()
	s := &Server{adminUserID: 1, sophub: service}
	return s
}

func TestSophubBindingStatusRoute(t *testing.T) {
	service := &fakeSophubAPIService{bindingStatus: domain.SophubBindingStatus{
		Configured: true, DisplayName: "admin", AuthorType: "user", AgentUID: "a1",
	}}
	s := newSophubTestServer(t, service)
	req := httptest.NewRequest("GET", "/v1/admin/sophub/binding", nil)
	rec := httptest.NewRecorder()
	s.handleAdminGetSophubBinding(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"configured":true`) {
		t.Fatalf("body=%s", rec.Body.String())
	}
}

func TestSophubBindRouteStoresKeyWithoutEcho(t *testing.T) {
	service := &fakeSophubAPIService{}
	s := newSophubTestServer(t, service)
	req := httptest.NewRequest("PUT", "/v1/admin/sophub/binding", strings.NewReader(`{"api_key":"secret-key"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.handleAdminBindSophub(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	if service.boundKey != "secret-key" {
		t.Fatalf("key not stored: %q", service.boundKey)
	}
	if strings.Contains(rec.Body.String(), "secret-key") {
		t.Fatal("api key echoed in response")
	}
}

func TestSophubSearchRoute(t *testing.T) {
	service := &fakeSophubAPIService{searchResult: domain.SophubSearchResult{
		Items: []domain.SophubRemoteSOP{{ID: "s1", Title: "One", FileType: "markdown"}}, Total: 1,
	}}
	s := newSophubTestServer(t, service)
	req := httptest.NewRequest("GET", "/v1/admin/sophub/search?q=report", nil)
	rec := httptest.NewRecorder()
	s.handleAdminSearchSophub(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"id":"s1"`) {
		t.Fatalf("body=%s", rec.Body.String())
	}
}
