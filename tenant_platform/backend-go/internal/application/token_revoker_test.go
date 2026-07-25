package application

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestHTTPTokenRevoker_RevokePostsJTI(t *testing.T) {
	var (
		gotJTI    string
		requests  atomic.Int32
		mu        sync.Mutex
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.URL.Path != "/internal/revoke" {
			t.Errorf("path=%s want /internal/revoke", r.URL.Path)
		}
		var body map[string]string
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode: %v", err)
		}
		mu.Lock()
		gotJTI = body["jti"]
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	revoker := newHTTPTokenRevoker(srv.URL)
	if err := revoker.Revoke(context.Background(), "jti-abc-123"); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("requests=%d want 1", got)
	}
	mu.Lock()
	defer mu.Unlock()
	if gotJTI != "jti-abc-123" {
		t.Fatalf("jti=%q want jti-abc-123", gotJTI)
	}
}

func TestHTTPTokenRevoker_EmptyJTINoCall(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Fatal("unexpected request")
	}))
	defer srv.Close()
	revoker := newHTTPTokenRevoker(srv.URL)
	if err := revoker.Revoke(context.Background(), ""); err != nil {
		t.Fatalf("revoke empty: %v", err)
	}
}

func TestHTTPTokenRevoker_NonNoContentReturnsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	revoker := newHTTPTokenRevoker(srv.URL)
	err := revoker.Revoke(context.Background(), "jti-1")
	if err == nil || !strings.Contains(err.Error(), "500") {
		t.Fatalf("want 500 error, got %v", err)
	}
}

func TestHTTPTokenRevoker_TimeoutPropagates(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(2 * time.Second)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	revoker := newHTTPTokenRevoker(srv.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	err := revoker.Revoke(ctx, "jti-1")
	if err == nil {
		t.Fatal("expected timeout error")
	}
}

func TestNewHTTPTokenRevoker_EmptyAddrReturnsNoop(t *testing.T) {
	r := NewHTTPTokenRevoker("")
	if _, ok := r.(noopTokenRevoker); !ok {
		t.Fatalf("want noopTokenRevoker, got %T", r)
	}
	if err := r.Revoke(context.Background(), "any"); err != nil {
		t.Fatalf("noop revoke: %v", err)
	}
}

func TestScheduler_RevokeTokenBestEffort_NoError(t *testing.T) {
	var revoked atomic.Int32
	revoker := fakeRevoker{onRevoke: func(_ string) { revoked.Add(1) }}
	s := &scheduler{cfg: SchedulerConfig{TokenRevoker: revoker}}
	s.revokeTokenBestEffort(context.Background(), "jti-1")
	if got := revoked.Load(); got != 1 {
		t.Fatalf("revoked=%d want 1", got)
	}
}

func TestScheduler_RevokeTokenBestEffort_NilRevokerNoPanic(t *testing.T) {
	s := &scheduler{}
	s.revokeTokenBestEffort(context.Background(), "jti-1")
}

func TestScheduler_RevokeTokenBestEffort_EmptyJTINoCall(t *testing.T) {
	revoker := fakeRevoker{onRevoke: func(_ string) { t.Fatal("must not revoke empty jti") }}
	s := &scheduler{cfg: SchedulerConfig{TokenRevoker: revoker}}
	s.revokeTokenBestEffort(context.Background(), "")
}

// fakeRevoker is a test double for TokenRevoker.
type fakeRevoker struct {
	onRevoke func(jti string)
	err      error
}

func (f fakeRevoker) Revoke(_ context.Context, jti string) error {
	if f.onRevoke != nil {
		f.onRevoke(jti)
	}
	return f.err
}
