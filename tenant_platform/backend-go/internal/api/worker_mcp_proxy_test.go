package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/golang-jwt/jwt/v5"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/infrastructure/llmproxy"
)

// newTestMCPProxy 构造带内存 resolver + 固定 token 校验的测试代理。
func newTestMCPProxy() *WorkerMCPProxy {
	return NewWorkerMCPProxy(
		func(ctx context.Context, serverID string) (MCPTarget, bool, error) {
			switch serverID {
			case "exa":
				return MCPTarget{URL: "https://mcp.exa.ai/mcp"}, true, nil
			case "disabled":
				return MCPTarget{}, false, nil
			default:
				return MCPTarget{}, false, nil
			}
		},
		func(ctx context.Context, token string) (llmproxy.CapabilityClaims, error) {
			if token != "good-token" {
				return llmproxy.CapabilityClaims{}, llmproxy.ErrCapabilityInvalid
			}
			return llmproxy.CapabilityClaims{
				Operation: "mcp",
				Budget:    `{"max_turns":100}`,
				RegisteredClaims: jwt.RegisteredClaims{
					ID:       "jti-1",
					Audience: jwt.ClaimStrings{llmproxy.MCPAudience},
				},
			}, nil
		},
		nil, // consume: nil = 跳过计量(现有测试保持语义)
	nil, // quota: nil = 跳过配额(现有测试语义)
	)
}

func TestWorkerMCPRequiresCapability(t *testing.T) {
	proxy := newTestMCPProxy()
	handler := NewWorkerMCPHandler(proxy)
	req := httptest.NewRequest("POST", "/v1/worker/mcp/exa", strings.NewReader(`{"jsonrpc":"2.0"}`))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("code = %d, want 401", rec.Code)
	}
}

func TestWorkerMCPUnknownServer(t *testing.T) {
	proxy := newTestMCPProxy()
	req := httptest.NewRequest("POST", "/v1/worker/mcp/disabled", strings.NewReader(`{"jsonrpc":"2.0"}`))
	req.Header.Set("Authorization", "Bearer good-token")
	rec := httptest.NewRecorder()
	NewWorkerMCPHandler(proxy).ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("code = %d, want 404", rec.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal error body: %v", err)
	}
	if body["code"] != "MCP_SERVER_NOT_FOUND" {
		t.Fatalf("code field = %v, want MCP_SERVER_NOT_FOUND", body["code"])
	}
}

func TestWorkerMCPForwardsJSONRPC(t *testing.T) {
	var gotHeaders http.Header
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeaders = r.Header.Clone()
		if r.Method != http.MethodPost {
			t.Errorf("upstream method = %s, want POST", r.Method)
		}
		if r.URL.Path != "/mcp" {
			t.Errorf("upstream path = %s, want /mcp", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "" {
			t.Errorf("Authorization leaked upstream: %q", r.Header.Get("Authorization"))
		}
		body, _ := io.ReadAll(r.Body)
		if string(body) != `{"jsonrpc":"2.0","method":"initialize"}` {
			t.Errorf("body = %s", body)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("MCP-Protocol-Version", "2024-11-05")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2024-11-05"}}`))
	}))
	defer upstream.Close()

	proxy := NewWorkerMCPProxy(
		func(ctx context.Context, serverID string) (MCPTarget, bool, error) {
			return MCPTarget{URL: upstream.URL + "/mcp"}, true, nil
		},
		func(ctx context.Context, token string) (llmproxy.CapabilityClaims, error) {
			return llmproxy.CapabilityClaims{
				Operation: "mcp",
				Budget:    `{"max_turns":100}`,
				RegisteredClaims: jwt.RegisteredClaims{
					ID:       "jti-1",
					Audience: jwt.ClaimStrings{llmproxy.MCPAudience},
				},
			}, nil
		},
		nil,
	nil, // quota: nil = 跳过配额(现有测试语义)
	)
	req := httptest.NewRequest("POST", "/v1/worker/mcp/exa", strings.NewReader(`{"jsonrpc":"2.0","method":"initialize"}`))
	req.Header.Set("Authorization", "Bearer good-token")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("MCP-Protocol-Version", "2024-11-05")
	req.Header.Set("Mcp-Session-Id", "sess-abc")
	rec := httptest.NewRecorder()
	NewWorkerMCPHandler(proxy).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	want := `{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2024-11-05"}}`
	if rec.Body.String() != want {
		t.Fatalf("body = %s, want %s", rec.Body.String(), want)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q", ct)
	}
	if mcpVer := rec.Header().Get("MCP-Protocol-Version"); mcpVer != "2024-11-05" {
		t.Errorf("MCP-Protocol-Version = %q", mcpVer)
	}
	if auth := gotHeaders.Get("Authorization"); auth != "" {
		t.Errorf("capability leaked upstream: %q", auth)
	}
	if ct := gotHeaders.Get("Content-Type"); ct != "application/json" {
		t.Errorf("upstream Content-Type = %q", ct)
	}
	// 会话头透传(回归: 曾因白名单缺失被剥掉, 导致 gateway 侧第二跳 400)。
	if sid := gotHeaders.Get("Mcp-Session-Id"); sid != "sess-abc" {
		t.Errorf("Mcp-Session-Id = %q, want sess-abc", sid)
	}
}

// TestWorkerMCPForwardsSessionHeader: Mcp-Session-Id 请求头透传 + 响应头
// 回传(Streamable HTTP 会话语义的完整往返)。
func TestWorkerMCPForwardsSessionHeader(t *testing.T) {
	var gotSession string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotSession = r.Header.Get("Mcp-Session-Id")
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Mcp-Session-Id", "new-sess-1")
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"ok":true}}`))
	}))
	defer upstream.Close()

	proxy := NewWorkerMCPProxy(
		func(ctx context.Context, serverID string) (MCPTarget, bool, error) {
			return MCPTarget{URL: upstream.URL}, true, nil
		},
		func(ctx context.Context, token string) (llmproxy.CapabilityClaims, error) {
			return llmproxy.CapabilityClaims{
				Operation: "mcp", Budget: `{"max_turns":100}`,
				RegisteredClaims: jwt.RegisteredClaims{
					ID: "jti-1", Audience: jwt.ClaimStrings{llmproxy.MCPAudience},
				},
			}, nil
		},
		nil,
	nil, // quota: nil = 跳过配额(现有测试语义)
	)
	req := httptest.NewRequest("POST", "/v1/worker/mcp/exa", strings.NewReader(`{"jsonrpc":"2.0"}`))
	req.Header.Set("Authorization", "Bearer good-token")
	req.Header.Set("Mcp-Session-Id", "sess-abc")
	rec := httptest.NewRecorder()
	NewWorkerMCPHandler(proxy).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d", rec.Code)
	}
	if gotSession != "sess-abc" {
		t.Fatalf("upstream Mcp-Session-Id = %q, want sess-abc", gotSession)
	}
	if sid := rec.Header().Get("Mcp-Session-Id"); sid != "new-sess-1" {
		t.Fatalf("response Mcp-Session-Id = %q, want new-sess-1", sid)
	}
}

func TestWorkerMCPStreamsSSE(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		for i := 0; i < 3; i++ {
			_, _ = w.Write([]byte("data: {\"chunk\":1}\n\n"))
			if flusher != nil {
				flusher.Flush()
			}
		}
	}))
	defer upstream.Close()

	proxy := NewWorkerMCPProxy(
		func(ctx context.Context, serverID string) (MCPTarget, bool, error) {
			return MCPTarget{URL: upstream.URL}, true, nil
		},
		func(ctx context.Context, token string) (llmproxy.CapabilityClaims, error) {
			return llmproxy.CapabilityClaims{
				Operation: "mcp", Budget: `{"max_turns":100}`,
				RegisteredClaims: jwt.RegisteredClaims{ID: "jti-1", Audience: jwt.ClaimStrings{llmproxy.MCPAudience}},
			}, nil
		},
		nil,
	nil, // quota: nil = 跳过配额(现有测试语义)
	)
	req := httptest.NewRequest("POST", "/v1/worker/mcp/exa", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer good-token")
	rec := httptest.NewRecorder()
	NewWorkerMCPHandler(proxy).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("Content-Type = %q, want text/event-stream", ct)
	}
	if n := strings.Count(rec.Body.String(), "data:"); n != 3 {
		t.Fatalf("SSE chunks = %d, want 3", n)
	}
}

func TestWorkerMCPBudgetExceeded(t *testing.T) {
	proxy := NewWorkerMCPProxy(
		func(ctx context.Context, serverID string) (MCPTarget, bool, error) {
			return MCPTarget{URL: "https://mcp.example.com/mcp"}, true, nil
		},
		func(ctx context.Context, token string) (llmproxy.CapabilityClaims, error) {
			return llmproxy.CapabilityClaims{
				Operation: "mcp", Budget: `{"max_turns":5}`,
				RegisteredClaims: jwt.RegisteredClaims{ID: "jti-1", Audience: jwt.ClaimStrings{llmproxy.MCPAudience}},
			}, nil
		},
		func(ctx context.Context, jtiHash [32]byte, maxCalls int64) (bool, error) {
			return false, nil // budget exhausted
		},
	nil, // quota: nil = 跳过配额(现有测试语义)
	)
	req := httptest.NewRequest("POST", "/v1/worker/mcp/exa", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer good-token")
	rec := httptest.NewRecorder()
	NewWorkerMCPHandler(proxy).ServeHTTP(rec, req)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("code = %d, want 429", rec.Code)
	}
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body["code"] != "MCP_BUDGET_EXCEEDED" {
		t.Fatalf("code field = %v, want MCP_BUDGET_EXCEEDED", body["code"])
	}
}

// TestWorkerMCPInjectsPlatformHeaders: 平台侧持有的凭据头(headers)注入上游,
// 同时 capability 的 Authorization 绝不外泄; 无任何平台内部头(EPIC D8')。
func TestWorkerMCPInjectsPlatformHeaders(t *testing.T) {
	var gotWorkspace, gotAuth, gotXKey string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotWorkspace = r.Header.Get("X-MCP-Workspace")
		gotAuth = r.Header.Get("Authorization")
		gotXKey = r.Header.Get("x-api-key")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2024-11-05"}}`))
	}))
	defer upstream.Close()

	proxy := NewWorkerMCPProxy(
		func(ctx context.Context, serverID string) (MCPTarget, bool, error) {
			return MCPTarget{
				URL: upstream.URL + "/mcp",
				Headers: map[string]string{
					"Authorization": "Bearer tvly-platform-held-key",
					"x-api-key":     "exa-platform-held-key",
				},
			}, true, nil
		},
		func(ctx context.Context, token string) (llmproxy.CapabilityClaims, error) {
			return llmproxy.CapabilityClaims{
				Operation: "mcp", Budget: `{"max_turns":100}`,
				RegisteredClaims: jwt.RegisteredClaims{
					ID: "jti-1", Subject: "personal:42",
					Audience: jwt.ClaimStrings{llmproxy.MCPAudience},
				},
			}, nil
		},
		nil,
		nil, // quota: nil = 跳过配额
	)
	req := httptest.NewRequest("POST", "/v1/worker/mcp/exa", strings.NewReader(`{"jsonrpc":"2.0"}`))
	req.Header.Set("Authorization", "Bearer good-token")
	rec := httptest.NewRecorder()
	NewWorkerMCPHandler(proxy).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d", rec.Code)
	}
	if gotAuth != "Bearer tvly-platform-held-key" {
		t.Fatalf("injected Authorization = %q, want platform-held key", gotAuth)
	}
	if gotXKey != "exa-platform-held-key" {
		t.Fatalf("injected x-api-key = %q", gotXKey)
	}
	if gotWorkspace != "" {
		t.Fatalf("X-MCP-Workspace leaked: %q", gotWorkspace)
	}
}

// TestWorkerMCPQuotaRejectsExhausted: 用户周期配额耗尽 → 429 MCP_QUOTA_EXCEEDED。
func TestWorkerMCPQuotaRejectsExhausted(t *testing.T) {
	proxy := NewWorkerMCPProxy(
		func(ctx context.Context, serverID string) (MCPTarget, bool, error) {
			return MCPTarget{URL: "https://mcp.example.com/mcp"}, true, nil
		},
		func(ctx context.Context, token string) (llmproxy.CapabilityClaims, error) {
			return llmproxy.CapabilityClaims{
				Operation: "mcp", Budget: `{"max_turns":100}`,
				RegisteredClaims: jwt.RegisteredClaims{
					ID: "jti-1", Subject: "personal:42",
					Audience: jwt.ClaimStrings{llmproxy.MCPAudience},
				},
			}, nil
		},
		nil,
		func(ctx context.Context, sessionKey, serverID string) (bool, error) {
			if sessionKey != "personal:42" || serverID != "exa" {
				t.Fatalf("quota args = (%q, %q)", sessionKey, serverID)
			}
			return false, nil // 耗尽
		},
	)
	req := httptest.NewRequest("POST", "/v1/worker/mcp/exa", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer good-token")
	rec := httptest.NewRecorder()
	NewWorkerMCPHandler(proxy).ServeHTTP(rec, req)

	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("code = %d, want 429", rec.Code)
	}
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body["code"] != "MCP_QUOTA_EXCEEDED" {
		t.Fatalf("code field = %v, want MCP_QUOTA_EXCEEDED", body["code"])
	}
}
