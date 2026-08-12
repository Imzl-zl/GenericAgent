package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
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
				PackageType: "single_file", IsPublic: boolPtr(true), Content: "# SOP\ncontent",
			}, nil
		},
		func(ctx context.Context, token string) (llmproxy.CapabilityClaims, error) {
			if token != "good-token" {
				return llmproxy.CapabilityClaims{}, llmproxy.ErrCapabilityInvalid
			}
			return llmproxy.CapabilityClaims{
				ProviderID: 1,
				Operation:  "sophub",
				RegisteredClaims: jwt.RegisteredClaims{
					Audience: jwt.ClaimStrings{llmproxy.SophubAudience},
				},
			}, nil
		},
		nil, // consume: nil = 跳过计量(现有测试保持语义)
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
				Operation:        "sophub",
				RegisteredClaims: jwt.RegisteredClaims{Audience: jwt.ClaimStrings{llmproxy.SophubAudience}},
			}, nil
		},
		nil,
	)
	req := httptest.NewRequest("GET", "/v1/worker/sophub/install?id=sop-draft", nil)
	req.Header.Set("Authorization", "Bearer t")
	rec := httptest.NewRecorder()
	proxy.ServeInstall(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("code = %d, want 403", rec.Code)
	}
}

func TestWorkerSophubBudgetExhaustedReturns429(t *testing.T) {
	calls := 0
	proxy := NewWorkerSophubProxy(
		func(ctx context.Context, query string, page, pageSize int) (domain.SophubSearchResult, error) {
			return domain.SophubSearchResult{}, nil
		},
		func(ctx context.Context, remoteID string) (domain.SophubRemoteSOP, error) {
			return domain.SophubRemoteSOP{}, nil
		},
		func(ctx context.Context, token string) (llmproxy.CapabilityClaims, error) {
			return llmproxy.CapabilityClaims{
				Operation: "sophub", Budget: `{"max_turns":3}`,
				RegisteredClaims: jwt.RegisteredClaims{Audience: jwt.ClaimStrings{llmproxy.SophubAudience}},
			}, nil
		},
		func(ctx context.Context, jtiHash [32]byte, maxCalls int64) (bool, error) {
			calls++
			return false, nil // 预算耗尽
		},
	)
	req := httptest.NewRequest("GET", "/v1/worker/sophub/search?q=report", nil)
	req.Header.Set("Authorization", "Bearer t")
	rec := httptest.NewRecorder()
	proxy.ServeSearch(rec, req)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("code = %d, want 429", rec.Code)
	}
	if calls != 1 {
		t.Fatalf("consume calls = %d, want 1", calls)
	}
}

func TestWorkerSophubMissingBudgetFailsClosed(t *testing.T) {
	// 审查 F10: 无预算的 token 不允许调用代理(fail-closed)。
	proxy := NewWorkerSophubProxy(
		func(ctx context.Context, query string, page, pageSize int) (domain.SophubSearchResult, error) {
			t.Fatal("search must not be invoked without budget")
			return domain.SophubSearchResult{}, nil
		},
		func(ctx context.Context, remoteID string) (domain.SophubRemoteSOP, error) {
			return domain.SophubRemoteSOP{}, nil
		},
		func(ctx context.Context, token string) (llmproxy.CapabilityClaims, error) {
			return llmproxy.CapabilityClaims{
				Operation: "sophub", Budget: "",
				RegisteredClaims: jwt.RegisteredClaims{Audience: jwt.ClaimStrings{llmproxy.SophubAudience}},
			}, nil
		},
		func(ctx context.Context, jtiHash [32]byte, maxCalls int64) (bool, error) {
			t.Fatal("consume must not be invoked without budget")
			return false, nil
		},
	)
	req := httptest.NewRequest("GET", "/v1/worker/sophub/search?q=report", nil)
	req.Header.Set("Authorization", "Bearer t")
	rec := httptest.NewRecorder()
	proxy.ServeSearch(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("code = %d, want 403", rec.Code)
	}
}

func TestWorkerSophubInstallContentTooLarge(t *testing.T) {
	big := strings.Repeat("x", maxSophubInstallBytes+1)
	proxy := NewWorkerSophubProxy(
		nil,
		func(ctx context.Context, remoteID string) (domain.SophubRemoteSOP, error) {
			return domain.SophubRemoteSOP{
				ID: remoteID, FileType: "markdown", Status: "approved",
				PackageType: "single_file", IsPublic: boolPtr(true), Content: big,
			}, nil
		},
		func(ctx context.Context, token string) (llmproxy.CapabilityClaims, error) {
			return llmproxy.CapabilityClaims{
				Operation: "sophub", Budget: `{"max_turns":3}`,
				RegisteredClaims: jwt.RegisteredClaims{Audience: jwt.ClaimStrings{llmproxy.SophubAudience}},
			}, nil
		},
		nil,
	)
	req := httptest.NewRequest("GET", "/v1/worker/sophub/install?id=sop-big", nil)
	req.Header.Set("Authorization", "Bearer t")
	rec := httptest.NewRecorder()
	proxy.ServeInstall(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("code = %d, want 403", rec.Code)
	}
}

// 审查 R5-C1: 内部 listener 只挂 capability-protected Worker Sophub 路由,
// 不得暴露管理/用户 API; 未认证请求 401, 未注册路由 404。
func TestWorkerSophubInternalHandlerOnlyExposesSophubRoutes(t *testing.T) {
	proxy := newTestSophubProxy()
	h := NewWorkerSophubHandler(proxy)
	if h == nil {
		t.Fatal("handler must be non-nil for configured proxy")
	}
	// Sophub 路由存在且需要 capability 鉴权。
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/worker/sophub/search?q=report", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated search code = %d, want 401", rec.Code)
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/worker/sophub/install?id=sop-1", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated install code = %d, want 401", rec.Code)
	}
	// 管理/用户路由不得存在。
	for _, path := range []string{"/v1/admin/dashboard/stats", "/v1/admin/users", "/v1/register", "/"} {
		rec = httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
		if rec.Code != http.StatusNotFound {
			t.Fatalf("path %s code = %d, want 404 (not exposed on internal listener)", path, rec.Code)
		}
	}
	// proxy 为 nil 时返回 nil(调用方跳过内部 listener)。
	if NewWorkerSophubHandler(nil) != nil {
		t.Fatal("nil proxy must yield nil handler")
	}
}

// TestWorkerSophubClientErrorDoesNotConsumeBudget(Y5 回归): 参数校验失败
// (400)是客户端错误, 不得消费 JTI 预算——预算只在调用即将发起时扣。
// 原 authenticate 校验与计量合一, q/id 为空也烧预算。
func TestWorkerSophubClientErrorDoesNotConsumeBudget(t *testing.T) {
	for _, tc := range []struct {
		name string
		path string
		code string
	}{
		{"search without q", "/v1/worker/sophub/search", "INVALID_QUERY"},
		{"install without id", "/v1/worker/sophub/install", "INVALID_SOP_ID"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			consumeCalls := 0
			proxy := NewWorkerSophubProxy(
				func(ctx context.Context, query string, page, pageSize int) (domain.SophubSearchResult, error) {
					t.Fatal("search must not be invoked on client error")
					return domain.SophubSearchResult{}, nil
				},
				func(ctx context.Context, remoteID string) (domain.SophubRemoteSOP, error) {
					t.Fatal("fetch must not be invoked on client error")
					return domain.SophubRemoteSOP{}, nil
				},
				func(ctx context.Context, token string) (llmproxy.CapabilityClaims, error) {
					return llmproxy.CapabilityClaims{
						Operation: "sophub", Budget: `{"max_turns":3}`,
						RegisteredClaims: jwt.RegisteredClaims{Audience: jwt.ClaimStrings{llmproxy.SophubAudience}},
					}, nil
				},
				func(ctx context.Context, jtiHash [32]byte, maxCalls int64) (bool, error) {
					consumeCalls++
					return true, nil
				},
			)
			req := httptest.NewRequest("GET", tc.path, nil)
			req.Header.Set("Authorization", "Bearer t")
			rec := httptest.NewRecorder()
			NewWorkerSophubHandler(proxy).ServeHTTP(rec, req)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("code = %d, want 400", rec.Code)
			}
			if consumeCalls != 0 {
				t.Fatalf("JTI budget must not be consumed on client error, got %d calls", consumeCalls)
			}
			if body := rec.Body.String(); !strings.Contains(body, tc.code) {
				t.Fatalf("body = %s, want code %s", body, tc.code)
			}
		})
	}
}

func boolPtr(v bool) *bool { return &v }
