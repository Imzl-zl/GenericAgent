package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/golang-jwt/jwt/v5"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/domain"
	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/infrastructure/llmproxy"
)

func newTestSophubProxy() *WorkerSophubProxy {
	return NewWorkerSophubProxy(
		func(ctx context.Context, query string, page, pageSize int) (domain.SophubSearchResult, error) {
			return domain.SophubSearchResult{
				Items: []domain.SophubRemoteSOP{{ID: "sop-1", Title: "SOP One", FileType: "markdown"}},
				Total: 1, Page: 1,
			}, nil
		},
		func(ctx context.Context, remoteID string) (domain.SophubRemoteSOP, error) {
			return domain.SophubRemoteSOP{
				ID: remoteID, Title: "SOP One", FileType: "markdown", Status: "approved",
				Content: "# SOP\ncontent",
			}, nil
		},
		func(ctx context.Context, token string) (llmproxy.CapabilityClaims, error) {
			if token != "good-token" {
				return llmproxy.CapabilityClaims{}, llmproxy.ErrCapabilityInvalid
			}
			return llmproxy.CapabilityClaims{
				ProviderID: 1,
				RegisteredClaims: jwt.RegisteredClaims{
					Audience: jwt.ClaimStrings{llmproxy.SophubAudience},
				},
			}, nil
		},
	)
}

func TestWorkerSophubSearchRequiresCapability(t *testing.T) {
	proxy := newTestSophubProxy()
	req := httptest.NewRequest("GET", "/v1/worker/sophub/search?q=report", nil)
	rec := httptest.NewRecorder()
	proxy.ServeSearch(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("code = %d, want 401", rec.Code)
	}
}

func TestWorkerSophubSearchWithCapability(t *testing.T) {
	proxy := newTestSophubProxy()
	req := httptest.NewRequest("GET", "/v1/worker/sophub/search?q=report", nil)
	req.Header.Set("Authorization", "Bearer good-token")
	rec := httptest.NewRecorder()
	proxy.ServeSearch(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d body=%s", rec.Code, rec.Body.String())
	}
	var result domain.SophubSearchResult
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Items) != 1 || result.Items[0].ID != "sop-1" {
		t.Fatalf("result=%+v", result)
	}
}

func TestWorkerSophubInstallReturnsApprovedMarkdownOnly(t *testing.T) {
	proxy := newTestSophubProxy()
	req := httptest.NewRequest("GET", "/v1/worker/sophub/install?id=sop-1", nil)
	req.Header.Set("Authorization", "Bearer good-token")
	rec := httptest.NewRecorder()
	proxy.ServeInstall(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d body=%s", rec.Code, rec.Body.String())
	}
	var payload map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload["content"] != "# SOP\ncontent" {
		t.Fatalf("payload=%v", payload)
	}
}

func TestWorkerSophubInstallRejectsNonApproved(t *testing.T) {
	proxy := NewWorkerSophubProxy(
		func(ctx context.Context, query string, page, pageSize int) (domain.SophubSearchResult, error) {
			return domain.SophubSearchResult{}, nil
		},
		func(ctx context.Context, remoteID string) (domain.SophubRemoteSOP, error) {
			return domain.SophubRemoteSOP{ID: remoteID, FileType: "markdown", Status: "draft", Content: "x"}, nil
		},
		func(ctx context.Context, token string) (llmproxy.CapabilityClaims, error) {
			return llmproxy.CapabilityClaims{
				RegisteredClaims: jwt.RegisteredClaims{Audience: jwt.ClaimStrings{llmproxy.SophubAudience}},
			}, nil
		},
	)
	req := httptest.NewRequest("GET", "/v1/worker/sophub/install?id=sop-draft", nil)
	req.Header.Set("Authorization", "Bearer t")
	rec := httptest.NewRecorder()
	proxy.ServeInstall(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("code = %d, want 403", rec.Code)
	}
}
