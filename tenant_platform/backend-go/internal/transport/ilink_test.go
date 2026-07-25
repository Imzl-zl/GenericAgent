package transport

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/poller"
)

// newPollerAdapter starts an httptest server mimicking the Python Bot Poller
// and returns an ILinkAdapter pointed at it. The handler receives POST /send.
func newPollerAdapter(t *testing.T, handler http.HandlerFunc) (*ILinkAdapter, *httptest.Server) {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	pollerClient, err := poller.NewClient(server.URL)
	if err != nil {
		t.Fatalf("poller client: %v", err)
	}
	adapter, err := NewILinkAdapter(ILinkAdapterConfig{Poller: pollerClient})
	if err != nil {
		t.Fatalf("new adapter: %v", err)
	}
	return adapter, server
}

func TestILinkAdapterSendMessage(t *testing.T) {
	var received poller.SendMessageRequest
	adapter, _ := newPollerAdapter(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/send" {
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &received); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"sent":true}`))
	})

	err := adapter.SendMessage(context.Background(), "bot-1", "user-1", "hello")
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	if received.BotUUID != "bot-1" {
		t.Fatalf("bot_uuid mismatch: %q", received.BotUUID)
	}
	if received.ILinkUserID != "user-1" {
		t.Fatalf("ilink_user_id mismatch: %q", received.ILinkUserID)
	}
	if received.Text != "hello" {
		t.Fatalf("text mismatch: %q", received.Text)
	}
}

func TestILinkAdapterSendMessagePollerFailure(t *testing.T) {
	adapter, _ := newPollerAdapter(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte("poller downstream error"))
	})

	err := adapter.SendMessage(context.Background(), "bot-1", "user-1", "hello")
	if err == nil {
		t.Fatal("expected error on poller failure")
	}
}

func TestILinkAdapterSendMessageRejectsEmpty(t *testing.T) {
	adapter, _ := newPollerAdapter(t, func(w http.ResponseWriter, _ *http.Request) {
		t.Fatal("poller should not be called")
	})

	cases := []struct{ botUUID, ilinkUserID, text string }{
		{"", "user-1", "hello"},
		{"bot-1", "", "hello"},
		{"bot-1", "user-1", ""},
	}
	for _, c := range cases {
		if err := adapter.SendMessage(context.Background(), c.botUUID, c.ilinkUserID, c.text); err == nil {
			t.Fatalf("expected error for %+v", c)
		}
	}
}

func TestILinkAdapterRecordIdempotency(t *testing.T) {
	adapter, _ := newPollerAdapter(t, func(w http.ResponseWriter, _ *http.Request) {})

	ctx := context.Background()
	first, err := adapter.RecordMessageIdempotency(ctx, "bot-1", "msg-1")
	if err != nil || !first {
		t.Fatalf("first: first=%v err=%v", first, err)
	}
	second, err := adapter.RecordMessageIdempotency(ctx, "bot-1", "msg-1")
	if err != nil || second {
		t.Fatalf("second: first=%v err=%v", second, err)
	}
	// Different message under same bot → first.
	third, err := adapter.RecordMessageIdempotency(ctx, "bot-1", "msg-2")
	if err != nil || !third {
		t.Fatalf("third: first=%v err=%v", third, err)
	}
}

func TestILinkAdapterIdempotencyRejectsEmpty(t *testing.T) {
	adapter, _ := newPollerAdapter(t, func(w http.ResponseWriter, _ *http.Request) {})

	if _, err := adapter.RecordMessageIdempotency(context.Background(), "", "msg-1"); err == nil {
		t.Fatal("expected error for empty bot uuid")
	}
	if _, err := adapter.RecordMessageIdempotency(context.Background(), "bot-1", ""); err == nil {
		t.Fatal("expected error for empty message id")
	}
}

func TestNewILinkAdapterRejectsNilPoller(t *testing.T) {
	if _, err := NewILinkAdapter(ILinkAdapterConfig{}); err == nil {
		t.Fatal("expected error for nil poller")
	}
}
