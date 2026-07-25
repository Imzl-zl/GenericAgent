package transport

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/domain"
	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/secret"
)

type fakeBotResolver struct {
	bot domain.Bot
	err error
}

func (r *fakeBotResolver) GetBotByUUID(_ context.Context, _ string) (domain.Bot, error) {
	return r.bot, r.err
}

func setupILinkTest(t *testing.T, handler http.HandlerFunc) (*ILinkAdapter, *httptest.Server, *secret.StaticKeyCipher) {
	t.Helper()
	cipher := secretMust(t)
	server := httptest.NewServer(handler)
	adapter, err := NewILinkAdapter(ILinkAdapterConfig{
		BaseURL:  server.URL,
		Cipher:   cipher,
		Resolver: &fakeBotResolver{},
	})
	if err != nil {
		t.Fatalf("new adapter: %v", err)
	}
	return adapter, server, cipher
}

func secretMust(t *testing.T) *secret.StaticKeyCipher {
	t.Helper()
	c, err := secret.NewStaticKeyCipherFromHex("000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f")
	if err != nil {
		t.Fatalf("cipher: %v", err)
	}
	return c
}

func TestILinkAdapterSendMessage(t *testing.T) {
	var received *ilinkSendRequest
	adapter, server, cipher := setupILinkTest(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages/send" {
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		body, _ := io.ReadAll(r.Body)
		var req ilinkSendRequest
		if err := json.Unmarshal(body, &req); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		received = &req
		w.WriteHeader(http.StatusOK)
	})
	defer server.Close()

	ct, ver, _ := cipher.Encrypt([]byte("bot-token-123"))
	adapter.cfg.Resolver = &fakeBotResolver{bot: domain.Bot{
		BotUUID:         "bot-1",
		IlinkUserID:     "user-1",
		TokenCiphertext: ct,
		TokenKeyVersion: ver,
	}}

	ctx := context.Background()
	err := adapter.SendMessage(ctx, "bot-1", "user-1", "hello")
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	if received == nil {
		t.Fatal("no request received")
	}
	if received.Token != "bot-token-123" {
		t.Fatalf("token mismatch: %q", received.Token)
	}
	if received.ToUser != "user-1" {
		t.Fatalf("to_user mismatch: %q", received.ToUser)
	}
	if received.Content != "hello" {
		t.Fatalf("content mismatch: %q", received.Content)
	}
}

func TestILinkAdapterSendMessageFailure(t *testing.T) {
	adapter, server, cipher := setupILinkTest(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte("invalid token"))
	})
	defer server.Close()

	ct, ver, _ := cipher.Encrypt([]byte("bot-token"))
	adapter.cfg.Resolver = &fakeBotResolver{bot: domain.Bot{
		BotUUID:         "bot-1",
		IlinkUserID:     "user-1",
		TokenCiphertext: ct,
		TokenKeyVersion: ver,
	}}

	err := adapter.SendMessage(context.Background(), "bot-1", "user-1", "hello")
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestILinkAdapterRejectsMismatchedUserID(t *testing.T) {
	adapter, server, cipher := setupILinkTest(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	defer server.Close()

	ct, ver, _ := cipher.Encrypt([]byte("bot-token"))
	adapter.cfg.Resolver = &fakeBotResolver{bot: domain.Bot{
		BotUUID:         "bot-1",
		IlinkUserID:     "user-1",
		TokenCiphertext: ct,
		TokenKeyVersion: ver,
	}}

	err := adapter.SendMessage(context.Background(), "bot-1", "user-2", "hello")
	if err == nil {
		t.Fatal("expected mismatch error")
	}
}

func TestILinkAdapterRecordIdempotency(t *testing.T) {
	adapter, server, _ := setupILinkTest(t, func(w http.ResponseWriter, _ *http.Request) {})
	defer server.Close()

	ctx := context.Background()
	first, err := adapter.RecordMessageIdempotency(ctx, "bot-1", "msg-1")
	if err != nil || !first {
		t.Fatalf("first: %v %v", first, err)
	}
	second, err := adapter.RecordMessageIdempotency(ctx, "bot-1", "msg-1")
	if err != nil || second {
		t.Fatalf("second: %v %v", second, err)
	}
}

func TestNewILinkAdapterRejectsMissingConfig(t *testing.T) {
	_, err := NewILinkAdapter(ILinkAdapterConfig{})
	if err == nil {
		t.Fatal("expected error")
	}
}
