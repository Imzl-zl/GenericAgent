package poller

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestNewClientRejectsEmptyURL(t *testing.T) {
	if _, err := NewClient(""); err == nil {
		t.Fatal("expected error for empty URL")
	}
	if _, err := NewClient("   "); err == nil {
		t.Fatal("expected error for whitespace URL")
	}
}

func TestNewClientTrimsTrailingSlash(t *testing.T) {
	c, err := NewClient("http://localhost:8090/")
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	if c.baseURL != "http://localhost:8090" {
		t.Fatalf("base URL not trimmed: %q", c.baseURL)
	}
}

func TestStartBot(t *testing.T) {
	var received StartBotRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/start" || r.Method != http.MethodPost {
			t.Fatalf("unexpected %s %s", r.Method, r.URL.Path)
		}
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &received)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"started":true}`))
	}))
	defer server.Close()

	c, err := NewClient(server.URL)
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	req := StartBotRequest{
		BotUUID:    "bot-1",
		BotToken:   "token-abc",
		ILinkBotID: "ilink-1",
		BaseURL:    "https://api.example.com",
		UpdatesBuf: "cursor-xyz",
		WebhookURL: "http://127.0.0.1:8080/v1/im/webhook",
	}
	if err := c.StartBot(context.Background(), req); err != nil {
		t.Fatalf("start: %v", err)
	}
	if received.BotUUID != "bot-1" || received.BotToken != "token-abc" {
		t.Fatalf("payload mismatch: %+v", received)
	}
	if received.WebhookURL == "" {
		t.Fatal("webhook URL not forwarded")
	}
}

func TestStartBotRejectsMissingFields(t *testing.T) {
	c, err := NewClient("http://127.0.0.1:1")
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	if err := c.StartBot(context.Background(), StartBotRequest{}); err == nil {
		t.Fatal("expected error for empty request")
	}
}

func TestStopBot(t *testing.T) {
	var received string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/stop" {
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		body, _ := io.ReadAll(r.Body)
		var m map[string]string
		_ = json.Unmarshal(body, &m)
		received = m["bot_uuid"]
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"stopped":true,"updates_buf":"final-cursor"}`))
	}))
	defer server.Close()

	c, err := NewClient(server.URL)
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	resp, err := c.StopBot(context.Background(), "bot-1")
	if err != nil {
		t.Fatalf("stop: %v", err)
	}
	if received != "bot-1" {
		t.Fatalf("bot_uuid mismatch: %q", received)
	}
	if !resp.Stopped || resp.UpdatesBuf != "final-cursor" {
		t.Fatalf("response mismatch: %+v", resp)
	}
}

func TestSendMessage(t *testing.T) {
	var received SendMessageRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/send" {
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &received)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"sent":true}`))
	}))
	defer server.Close()

	c, err := NewClient(server.URL)
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	err = c.SendMessage(context.Background(), SendMessageRequest{
		BotUUID:     "bot-1",
		ILinkUserID: "user-1",
		Text:        "hello",
	})
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	if received.BotUUID != "bot-1" || received.ILinkUserID != "user-1" || received.Text != "hello" {
		t.Fatalf("payload mismatch: %+v", received)
	}
}

func TestHealth(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/health" || r.Method != http.MethodGet {
			t.Fatalf("unexpected %s %s", r.Method, r.URL.Path)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"healthy":true,"active_bots":["bot-1","bot-2"]}`))
	}))
	defer server.Close()

	c, err := NewClient(server.URL)
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	h, err := c.Health(context.Background())
	if err != nil {
		t.Fatalf("health: %v", err)
	}
	if !h.Healthy || len(h.ActiveBots) != 2 {
		t.Fatalf("response mismatch: %+v", h)
	}
}

func TestPollerErrorPropagates(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("poller internal error"))
	}))
	defer server.Close()

	c, err := NewClient(server.URL)
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	if err := c.SendMessage(context.Background(), SendMessageRequest{
		BotUUID: "bot-1", ILinkUserID: "user-1", Text: "hi",
	}); err == nil {
		t.Fatal("expected error on 500")
	}
}
