package llmproxy

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/domain"
)

const testSigningKey = "test-signing-key-at-least-32-bytes"

func newTestServer(t *testing.T, upstream *httptest.Server) *Server {
	t.Helper()
	return newTestServerWithUsage(t, upstream, &fakeUsageCounter{maxCalls: 1000})
}

func newTestServerWithUsage(t *testing.T, upstream *httptest.Server, usage *fakeUsageCounter) *Server {
	t.Helper()
	cfg := Config{
		Listen:               "127.0.0.1:0",
		SigningKey:           []byte(testSigningKey),
		TokenTTL:             time.Hour,
		AllowedUpstreamCIDRs: []string{"127.0.0.0/8", "::1/128"},
		AllowedHTTPHosts:     []string{upstream.Listener.Addr().String()},
		ProviderSource: &fakeProviderSource{
			provider: testProvider(domain.ProviderNativeOAI, upstream.URL, "gpt-test", testUpstreamKey),
		},
		Cipher:       &fakeCipher{wantVersion: 1},
		Revocations:  &fakeRevocationSource{revoked: make(map[[32]byte]bool)},
		UsageCounter: usage,
	}
	srv, err := NewServer(cfg)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	return srv
}

// fakeUsageCounter 记录计量调用, 按 maxCalls 放行; err 非 nil 时模拟后端故障。
type fakeUsageCounter struct {
	maxCalls int64
	err      error
	calls    map[[32]byte]int64
}

func (f *fakeUsageCounter) ConsumeCapabilityCall(_ context.Context, jtiHash [32]byte, maxCalls int64) (bool, error) {
	if f.calls == nil {
		f.calls = make(map[[32]byte]int64)
	}
	if f.err != nil {
		return false, f.err
	}
	f.calls[jtiHash]++
	return f.calls[jtiHash] <= maxCalls && f.calls[jtiHash] <= f.maxCalls, nil
}

func handlerCapabilitySpec(sessionKey string) CapabilitySpec {
	return CapabilitySpec{
		SessionKey: sessionKey, ProviderID: 1, ProviderRevision: 1,
		ProviderType: domain.ProviderNativeOAI, Model: "gpt-test", PolicyVersion: "p",
		TaskID: "task-1", RunnerGeneration: 1,
		Operation: "llm.chat", Budget: `{"max_turns":8,"max_output_bytes":262144}`,
	}
}

func TestHandlerChatCompletionsValidToken(t *testing.T) {
	upstreamBody := `{"id":"cc","choices":[{"message":{"role":"assistant","content":"hi"}}]}`
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+testUpstreamKey {
			t.Errorf("upstream auth = %q", r.Header.Get("Authorization"))
		}
		_, _ = w.Write([]byte(upstreamBody))
	}))
	defer upstream.Close()

	srv := newTestServer(t, upstream)
	iss, err := NewIssuer([]byte(testSigningKey), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	token, _, err := iss.Issue(handlerCapabilitySpec("personal:42"))
	if err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"gpt-test","messages":[]}`))
	req.Header.Set("Authorization", "Bearer "+token)
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if rec.Body.String() != upstreamBody {
		t.Fatalf("body = %q", rec.Body.String())
	}
}

func TestHandlerAliasChatCompletionsPath(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{}`))
	}))
	defer upstream.Close()
	srv := newTestServer(t, upstream)
	iss, _ := NewIssuer([]byte(testSigningKey), time.Hour)
	token, _, _ := iss.Issue(handlerCapabilitySpec("personal:1"))

	for _, path := range []string{"/v1/chat/completions", "/chat/completions"} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{"model":"gpt-test"}`))
		req.Header.Set("Authorization", "Bearer "+token)
		srv.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("path %s status = %d", path, rec.Code)
		}
	}
}

func TestHandlerRejectsMissingToken(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer upstream.Close()
	srv := newTestServer(t, upstream)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{}`)))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

func TestHandlerRejectsInvalidToken(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer upstream.Close()
	srv := newTestServer(t, upstream)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer not-a-real-token")
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

func TestHandlerUpstream500PreservesStatus(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer upstream.Close()
	srv := newTestServer(t, upstream)
	iss, _ := NewIssuer([]byte(testSigningKey), time.Hour)
	token, _, _ := iss.Issue(handlerCapabilitySpec("personal:1"))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"gpt-test"}`))
	req.Header.Set("Authorization", "Bearer "+token)
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "UPSTREAM_ERROR") {
		t.Fatalf("body = %q, want UPSTREAM_ERROR", rec.Body.String())
	}
}

func TestHandlerUpstream429Returns429(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "rate", http.StatusTooManyRequests)
	}))
	defer upstream.Close()
	srv := newTestServer(t, upstream)
	iss, _ := NewIssuer([]byte(testSigningKey), time.Hour)
	token, _, _ := iss.Issue(handlerCapabilitySpec("personal:1"))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"gpt-test"}`))
	req.Header.Set("Authorization", "Bearer "+token)
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", rec.Code)
	}
}

func TestHandlerRejectsPersistentlyRevokedCapability(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{}`))
	}))
	defer upstream.Close()
	srv := newTestServer(t, upstream)
	iss, _ := NewIssuer([]byte(testSigningKey), time.Hour)
	token, claims, _ := iss.Issue(handlerCapabilitySpec("personal:1"))

	// First call succeeds.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"gpt-test"}`))
	req.Header.Set("Authorization", "Bearer "+token)
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("pre-revoke status = %d", rec.Code)
	}

	revocations := srv.Config().Revocations.(*fakeRevocationSource)
	revocations.revoked[HashJTI(claims.ID)] = true

	// Second call fails.
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"gpt-test"}`))
	req.Header.Set("Authorization", "Bearer "+token)
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("post-revoke status = %d, want 401", rec.Code)
	}
}

func TestHandlerRejectsNonPost(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer upstream.Close()
	srv := newTestServer(t, upstream)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/chat/completions", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", rec.Code)
	}
}

func TestHandlerForwardsBodyUnmodified(t *testing.T) {
	var gotBody string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer upstream.Close()
	srv := newTestServer(t, upstream)
	iss, _ := NewIssuer([]byte(testSigningKey), time.Hour)
	token, _, _ := iss.Issue(handlerCapabilitySpec("personal:1"))

	body := `{"model":"gpt-test","messages":[{"role":"user","content":"hello"}]}`
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	srv.Handler().ServeHTTP(rec, req)
	if gotBody != body {
		t.Fatalf("forwarded body = %q, want %q", gotBody, body)
	}
}

func TestResetRequestBodyDisablesAutomaticPOSTReplay(t *testing.T) {
	request, err := http.NewRequest(http.MethodPost, "http://proxy.invalid/v1/responses", strings.NewReader("original"))
	if err != nil {
		t.Fatal(err)
	}
	if request.GetBody == nil {
		t.Fatal("fixture request is not rewindable")
	}
	resetRequestBody(request, []byte("validated"))
	if request.GetBody != nil {
		t.Fatal("validated POST remains rewindable for automatic transport replay")
	}
	body, err := io.ReadAll(request.Body)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "validated" || request.ContentLength != int64(len("validated")) {
		t.Fatalf("body=%q content_length=%d", body, request.ContentLength)
	}
}

func issueHandlerToken(t *testing.T, spec CapabilitySpec) string {
	t.Helper()
	iss, err := NewIssuer([]byte(testSigningKey), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	token, _, err := iss.Issue(spec)
	if err != nil {
		t.Fatal(err)
	}
	return token
}

func doChatRequest(t *testing.T, srv *Server, token string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"gpt-test","messages":[]}`))
	req.Header.Set("Authorization", "Bearer "+token)
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

// TestHandlerBudgetExceededRejects429: 同一 JTI 调用超过 max_turns 后,
// llm-proxy 必须拒绝(429), 防止 Runner 绕过 Worker 限制直接刷 Proxy
// (审查 R4-I9)。
func TestHandlerBudgetExceededRejects429(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{}`))
	}))
	defer upstream.Close()

	usage := &fakeUsageCounter{maxCalls: 1000}
	srv := newTestServerWithUsage(t, upstream, usage)
	token := issueHandlerToken(t, handlerCapabilitySpec("personal:42"))

	for i := 0; i < 8; i++ {
		if rec := doChatRequest(t, srv, token); rec.Code != http.StatusOK {
			t.Fatalf("call %d: status = %d, want 200", i+1, rec.Code)
		}
	}
	rec := doChatRequest(t, srv, token)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("9th call status = %d, want 429", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "CAPABILITY_BUDGET_EXCEEDED") {
		t.Fatalf("9th call body = %s", rec.Body.String())
	}
}

// TestHandlerBudgetMissingRejectedFailClosed: 预算缺失/无 max_turns 的
// token 必须拒绝(fail-closed), 不允许无界转发。
func TestHandlerBudgetMissingRejectedFailClosed(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{}`))
	}))
	defer upstream.Close()

	srv := newTestServer(t, upstream)
	spec := handlerCapabilitySpec("personal:42")
	spec.Budget = `{"max_output_bytes":262144}`
	token := issueHandlerToken(t, spec)

	rec := doChatRequest(t, srv, token)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "CAPABILITY_BUDGET_INVALID") {
		t.Fatalf("body = %s", rec.Body.String())
	}
}

// TestHandlerBudgetCounterUnavailable503: 计量后端故障时拒绝(503),
// 不得降级为无计量转发。
func TestHandlerBudgetCounterUnavailable503(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{}`))
	}))
	defer upstream.Close()

	srv := newTestServerWithUsage(t, upstream, &fakeUsageCounter{err: context.DeadlineExceeded})
	token := issueHandlerToken(t, handlerCapabilitySpec("personal:42"))

	rec := doChatRequest(t, srv, token)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
}

// TestParseBudgetMaxTurns 覆盖预算解析边界。
func TestParseBudgetMaxTurns(t *testing.T) {
	cases := []struct {
		budget  string
		want    int64
		wantOK  bool
	}{
		{`{"max_turns":8}`, 8, true},
		{`{"max_turns":8,"max_output_bytes":1024}`, 8, true},
		{`{"max_output_bytes":1024}`, 0, false},
		{`{"max_turns":0}`, 0, false},
		{`{"max_turns":-1}`, 0, false},
		{``, 0, false},
		{`not-json`, 0, false},
	}
	for _, c := range cases {
		got, ok := ParseBudgetMaxTurns(c.budget)
		if got != c.want || ok != c.wantOK {
			t.Errorf("ParseBudgetMaxTurns(%q) = (%d,%v), want (%d,%v)", c.budget, got, ok, c.want, c.wantOK)
		}
	}
}

// ─────────────────── Phase B 托管形态: 生图路由(2026-08-14) ───────────────────

func imageCapabilitySpec(sessionKey string) CapabilitySpec {
	spec := handlerCapabilitySpec(sessionKey)
	spec.Operation = OperationImage
	return spec
}

func doImageRequest(t *testing.T, srv *Server, token, body string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/images/generations", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

// TestHandlerImageGenerationsValidToken: llm.image 能力 token 打生图路由 →
// 转发到上游 images/generations, 鉴权头注入正确。
func TestHandlerImageGenerationsValidToken(t *testing.T) {
	upstreamBody := `{"data":[{"b64_json":"aGVsbG8="}]}`
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/images/generations" {
			t.Errorf("upstream path = %q, want /v1/images/generations", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer "+testUpstreamKey {
			t.Errorf("upstream auth = %q", r.Header.Get("Authorization"))
		}
		_, _ = w.Write([]byte(upstreamBody))
	}))
	defer upstream.Close()

	srv := newTestServer(t, upstream)
	token := issueHandlerToken(t, imageCapabilitySpec("personal:42"))
	rec := doImageRequest(t, srv, token, `{"model":"gpt-test","prompt":"a cat","n":1,"size":"1024x1024"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if rec.Body.String() != upstreamBody {
		t.Fatalf("body = %q", rec.Body.String())
	}
}

// TestHandlerImageGenerationsRejectsChatToken: llm.chat 能力 token 打生图路由
// → 401 CAPABILITY_INVALID(操作维度不匹配, deny-by-default)。
func TestHandlerImageGenerationsRejectsChatToken(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("upstream must not be reached for chat token on image route")
	}))
	defer upstream.Close()

	srv := newTestServer(t, upstream)
	token := issueHandlerToken(t, handlerCapabilitySpec("personal:1"))
	rec := doImageRequest(t, srv, token, `{"model":"gpt-test","prompt":"x"}`)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

// TestHandlerChatRejectsImageToken: llm.image 能力 token 打 chat 路由 → 401。
func TestHandlerChatRejectsImageToken(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("upstream must not be reached for image token on chat route")
	}))
	defer upstream.Close()

	srv := newTestServer(t, upstream)
	token := issueHandlerToken(t, imageCapabilitySpec("personal:1"))
	rec := doChatRequest(t, srv, token)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

// TestHandlerImageGenerationsAliasPath: 无 /v1 前缀别名也路由到生图。
func TestHandlerImageGenerationsAliasPath(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":[]}`))
	}))
	defer upstream.Close()

	srv := newTestServer(t, upstream)
	token := issueHandlerToken(t, imageCapabilitySpec("personal:1"))
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/images/generations", strings.NewReader(`{"model":"gpt-test","prompt":"x"}`))
	req.Header.Set("Authorization", "Bearer "+token)
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
}
