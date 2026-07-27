package llmproxy

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/domain"
)

const testSigningKey = "test-signing-key-0123456789ab"

func newTestServer(t *testing.T, upstream *httptest.Server) *Server {
	t.Helper()
	cfg := Config{
		Listen:     "127.0.0.1:0",
		SigningKey: []byte(testSigningKey),
		TokenTTL:   time.Hour,
		ProviderSource: &fakeProviderSource{
			provider: testProvider(domain.ProviderNativeOAI, upstream.URL, "gpt-test", testUpstreamKey),
		},
		Cipher: &fakeCipher{wantVersion: 1},
	}
	srv, err := NewServer(cfg)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	return srv
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
	token, _, err := iss.Issue("personal:42", "foundation.no-host-tools.v1", string(domain.ProviderNativeOAI), "gpt-test")
	if err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"gpt","messages":[]}`))
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
	token, _, _ := iss.Issue("personal:1", "p", string(domain.ProviderNativeOAI), "gpt-test")

	for _, path := range []string{"/v1/chat/completions", "/chat/completions"} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{}`))
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

func TestHandlerUpstream500Returns502(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer upstream.Close()
	srv := newTestServer(t, upstream)
	iss, _ := NewIssuer([]byte(testSigningKey), time.Hour)
	token, _, _ := iss.Issue("personal:1", "p", string(domain.ProviderNativeOAI), "gpt-test")

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer "+token)
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", rec.Code)
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
	token, _, _ := iss.Issue("personal:1", "p", string(domain.ProviderNativeOAI), "gpt-test")

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer "+token)
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", rec.Code)
	}
}

func TestHandlerRevocationEndpoint(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{}`))
	}))
	defer upstream.Close()
	srv := newTestServer(t, upstream)
	iss, _ := NewIssuer([]byte(testSigningKey), time.Hour)
	token, claims, _ := iss.Issue("personal:1", "p", string(domain.ProviderNativeOAI), "gpt-test")

	// First call succeeds.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer "+token)
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("pre-revoke status = %d", rec.Code)
	}

	// Revoke.
	rec = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/internal/revoke", bytes.NewReader([]byte(`{"jti":"`+claims.Jti+`"}`))))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("revoke status = %d, want 204", rec.Code)
	}

	// Second call fails.
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{}`))
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
	token, _, _ := iss.Issue("personal:1", "p", string(domain.ProviderNativeOAI), "gpt-test")

	body := `{"model":"gpt-test","messages":[{"role":"user","content":"hello"}]}`
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	srv.Handler().ServeHTTP(rec, req)
	if gotBody != body {
		t.Fatalf("forwarded body = %q, want %q", gotBody, body)
	}
}
