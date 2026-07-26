package llmproxy

import (
	"bytes"
	"context"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/domain"
)

const testUpstreamKey = "real-upstream-key-do-not-leak"

func newTestUpstream(t *testing.T, srv *httptest.Server) *Upstream {
	t.Helper()
	return NewUpstream(srv.Client())
}

func TestUpstreamForwardsWithRealKey(t *testing.T) {
	var gotAuth, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"cc","choices":[{"message":{"role":"assistant","content":"hi"}}]}`))
	}))
	defer srv.Close()

	u := newTestUpstream(t, srv)
	p := testProvider(domain.ProviderOpenAICompatible, srv.URL, "gpt-test", testUpstreamKey)
	resp, err := u.Forward(context.Background(), p, UpstreamRequest{Body: []byte(`{"model":"gpt-test","messages":[]}`)})
	if err != nil {
		t.Fatalf("Forward: %v", err)
	}
	if gotAuth != "Bearer "+testUpstreamKey {
		t.Fatalf("upstream auth = %q, want Bearer <key>", gotAuth)
	}
	if gotBody != `{"model":"gpt-test","messages":[]}` {
		t.Fatalf("upstream body = %q", gotBody)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
}

func TestUpstreamConverts429ToError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"rate limited"}`, http.StatusTooManyRequests)
	}))
	defer srv.Close()

	u := newTestUpstream(t, srv)
	p := testProvider(domain.ProviderOpenAICompatible, srv.URL, "gpt-test", testUpstreamKey)
	_, err := u.Forward(context.Background(), p, UpstreamRequest{Body: []byte(`{}`)})
	if err == nil {
		t.Fatal("expected UpstreamError for 429")
	}
	ue, ok := err.(*UpstreamError)
	if !ok {
		t.Fatalf("expected *UpstreamError, got %T", err)
	}
	if ue.Code != http.StatusTooManyRequests {
		t.Fatalf("code = %d", ue.Code)
	}
}

func TestUpstreamConverts500ToError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `internal`, http.StatusInternalServerError)
	}))
	defer srv.Close()

	u := newTestUpstream(t, srv)
	p := testProvider(domain.ProviderOpenAICompatible, srv.URL, "gpt-test", testUpstreamKey)
	_, err := u.Forward(context.Background(), p, UpstreamRequest{Body: []byte(`{}`)})
	if err == nil {
		t.Fatal("expected UpstreamError for 500")
	}
	if _, ok := err.(*UpstreamError); !ok {
		t.Fatalf("expected *UpstreamError, got %T", err)
	}
}

func TestUpstreamPassesThrough200(t *testing.T) {
	body := `{"id":"x","choices":[]}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	u := newTestUpstream(t, srv)
	p := testProvider(domain.ProviderOpenAICompatible, srv.URL, "gpt-test", testUpstreamKey)
	resp, err := u.Forward(context.Background(), p, UpstreamRequest{Body: []byte(`{}`)})
	if err != nil {
		t.Fatalf("Forward: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if string(resp.Body) != body {
		t.Fatalf("body = %q", resp.Body)
	}
}

func TestUpstreamNeverLogsKey(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `boom`, http.StatusBadGateway)
	}))
	defer srv.Close()

	var logBuf bytes.Buffer
	u := newTestUpstream(t, srv)
	u.SetLogger(log.New(&logBuf, "", 0))

	p := testProvider(domain.ProviderOpenAICompatible, srv.URL, "gpt-test", testUpstreamKey)
	_, err := u.Forward(context.Background(), p, UpstreamRequest{Body: []byte(`{}`)})
	if err == nil {
		t.Fatal("expected error")
	}
	if strings.Contains(logBuf.String(), testUpstreamKey) {
		t.Fatalf("upstream key leaked into log: %s", logBuf.String())
	}
}

func TestUpstreamContextCancel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer srv.Close()

	u := newTestUpstream(t, srv)
	p := testProvider(domain.ProviderOpenAICompatible, srv.URL, "gpt-test", testUpstreamKey)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := u.Forward(ctx, p, UpstreamRequest{Body: []byte(`{}`)}); err == nil {
		t.Fatal("expected context cancellation error")
	}
}

func TestUpstreamAnthropicUsesXApiKey(t *testing.T) {
	var gotKey, gotVersion string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKey = r.Header.Get("x-api-key")
		gotVersion = r.Header.Get("anthropic-version")
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	u := newTestUpstream(t, srv)
	p := testProvider(domain.ProviderAnthropicMessages, srv.URL, "claude-test", testUpstreamKey)
	_, err := u.Forward(context.Background(), p, UpstreamRequest{Body: []byte(`{}`)})
	if err != nil {
		t.Fatalf("Forward: %v", err)
	}
	if gotKey != testUpstreamKey {
		t.Fatalf("x-api-key = %q, want %q", gotKey, testUpstreamKey)
	}
	if gotVersion != "2023-06-01" {
		t.Fatalf("anthropic-version = %q, want 2023-06-01", gotVersion)
	}
}
