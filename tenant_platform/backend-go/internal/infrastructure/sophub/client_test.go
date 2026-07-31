package sophub

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestClientVerifySearchAndFetch(t *testing.T) {
	const key = "sophub-secret-sentinel"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+key {
			t.Fatalf("authorization=%q", r.Header.Get("Authorization"))
		}
		switch r.URL.Path {
		case "/api/me":
			_, _ = w.Write([]byte(`{"author_type":"agent","agent_uid":"agent-1","display_name":"platform"}`))
		case "/api/sops":
			if r.URL.Query().Get("q") != "document report" || r.URL.Query().Get("page") != "2" || r.URL.Query().Get("page_size") != "5" {
				t.Fatalf("query=%s", r.URL.RawQuery)
			}
			_, _ = w.Write([]byte(`{"items":[{"id":"remote-1","title":"Report","preview":"preview","file_type":"markdown","package_type":"single_file","status":"approved"}],"total":1,"page":2,"page_size":5,"total_pages":1,"has_more":false}`))
		case "/api/sops/remote-1":
			_, _ = w.Write([]byte(`{"id":"remote-1","title":"Report","preview":"preview","file_type":"markdown","package_type":"single_file","status":"approved","content":"# Report\n"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := newClient(server.URL, server.Client())
	identity, err := client.Verify(context.Background(), key)
	if err != nil || identity.AgentUID != "agent-1" || identity.DisplayName != "platform" {
		t.Fatalf("identity=%+v err=%v", identity, err)
	}
	result, err := client.Search(context.Background(), key, "document report", 2, 5)
	if err != nil || len(result.Items) != 1 || result.Items[0].ID != "remote-1" {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	sop, err := client.GetSOP(context.Background(), key, "remote-1")
	if err != nil || sop.Content != "# Report\n" {
		t.Fatalf("sop=%+v err=%v", sop, err)
	}
}

func TestClientRejectsRedirectOversizeAndSecretSafeErrors(t *testing.T) {
	var redirected atomic.Bool
	destination := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		redirected.Store(true)
	}))
	defer destination.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/me":
			http.Redirect(w, r, destination.URL, http.StatusFound)
		case "/api/sops/large":
			_, _ = w.Write([]byte(`{"id":"large","title":"Large","file_type":"markdown","package_type":"single_file","status":"approved","content":"` + strings.Repeat("x", maxSophubResponseBytes) + `"}`))
		case "/api/sops/failure":
			http.Error(w, "upstream echoed sophub-secret-sentinel", http.StatusUnauthorized)
		}
	}))
	defer server.Close()

	client := newClient(server.URL, server.Client())
	if _, err := client.Verify(context.Background(), "sophub-secret-sentinel"); err == nil || strings.Contains(err.Error(), "sophub-secret-sentinel") {
		t.Fatalf("redirect err=%v", err)
	}
	if redirected.Load() {
		t.Fatal("client followed redirect")
	}
	if _, err := client.GetSOP(context.Background(), "sophub-secret-sentinel", "large"); err == nil || !strings.Contains(err.Error(), "too large") {
		t.Fatalf("oversize err=%v", err)
	}
	if _, err := client.GetSOP(context.Background(), "sophub-secret-sentinel", "failure"); err == nil || strings.Contains(err.Error(), "sophub-secret-sentinel") {
		t.Fatalf("secret-safe err=%v", err)
	}
}

func TestNewClientUsesPinnedOfficialOrigin(t *testing.T) {
	client := NewClient()
	if client.baseURL != OfficialBaseURL {
		t.Fatalf("baseURL=%q", client.baseURL)
	}
	if client.httpClient.CheckRedirect == nil {
		t.Fatal("redirect policy is required")
	}
	if fmt.Sprint(client.httpClient.Transport) == "<nil>" {
		t.Fatal("explicit proxy-free transport is required")
	}
}
