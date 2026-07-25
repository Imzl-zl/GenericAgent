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
)

const testUpstreamKey = "real-upstream-key-do-not-leak"

func newTestForwarder(t *testing.T, srv *httptest.Server) *Forwarder {
	t.Helper()
	f, err := NewForwarder(srv.URL, testUpstreamKey, srv.Client())
	if err != nil {
		t.Fatalf("NewForwarder: %v", err)
	}
	return f
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

	f := newTestForwarder(t, srv)
	resp, err := f.Forward(context.Background(), UpstreamRequest{Body: []byte(`{"model":"gpt-test","messages":[]}`)})
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

	f := newTestForwarder(t, srv)
	_, err := f.Forward(context.Background(), UpstreamRequest{Body: []byte(`{}`)})
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

	f := newTestForwarder(t, srv)
	_, err := f.Forward(context.Background(), UpstreamRequest{Body: []byte(`{}`)})
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

	f := newTestForwarder(t, srv)
	resp, err := f.Forward(context.Background(), UpstreamRequest{Body: []byte(`{}`)})
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
	f := newTestForwarder(t, srv)
	f.SetLogger(log.New(&logBuf, "", 0))

	_, err := f.Forward(context.Background(), UpstreamRequest{Body: []byte(`{}`)})
	if err == nil {
		t.Fatal("expected error")
	}
	if strings.Contains(logBuf.String(), testUpstreamKey) {
		t.Fatalf("upstream key leaked into log: %s", logBuf.String())
	}
}

func TestUpstreamRejectsEmptyConfig(t *testing.T) {
	if _, err := NewForwarder("", "k", nil); err == nil {
		t.Fatal("expected baseURL error")
	}
	if _, err := NewForwarder("http://x", "", nil); err == nil {
		t.Fatal("expected apiKey error")
	}
}

func TestUpstreamContextCancel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer srv.Close()

	f := newTestForwarder(t, srv)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := f.Forward(ctx, UpstreamRequest{Body: []byte(`{}`)}); err == nil {
		t.Fatal("expected context cancellation error")
	}
}
