package api

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/application"
	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/domain"
	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/infrastructure/policy"
)

type fakeRouter struct {
	result application.RouterResult
	err    error
}

func (r *fakeRouter) HandleMessage(_ context.Context, _ application.IncomingMessage) (application.RouterResult, error) {
	return r.result, r.err
}

func (r *fakeRouter) InvalidateCommandCache() {}

// fakeBotLifecycle records calls for assertion.
type fakeBotLifecycle struct {
	persistBufBotUUID string
	persistBuf        string
	expiredBotUUID    string
	startBotBot       domain.Bot
	persistErr        error
}

func (f *fakeBotLifecycle) StartBotForBoundUser(_ context.Context, bot domain.Bot) error {
	f.startBotBot = bot
	return nil
}
func (f *fakeBotLifecycle) StopBot(_ context.Context, _ string) error { return nil }
func (f *fakeBotLifecycle) RestoreActiveBots(_ context.Context) error { return nil }
func (f *fakeBotLifecycle) PersistUpdatesBuf(_ context.Context, botUUID, buf string) error {
	f.persistBufBotUUID = botUUID
	f.persistBuf = buf
	return f.persistErr
}
func (f *fakeBotLifecycle) HandleAuthExpired(_ context.Context, botUUID string) error {
	f.expiredBotUUID = botUUID
	return nil
}

func TestIMWebhookRoutesMessage(t *testing.T) {
	router := &fakeRouter{result: application.RouterResult{
		Action: application.ActionTaskCreated,
		Reply:  "task queued",
		UserID: 42,
	}}
	server := newTestServerWithRouter(t, router)

	req := newSignedWebhookRequest(t, "test-secret", imWebhookBody{
		BotUUID:    "bot-1",
		IlinkUserID: "user-1",
		MessageID:  "msg-1",
		Text:       "hello",
	})
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("unexpected status %d: %s", rec.Code, rec.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp["action"] != "task_created" {
		t.Fatalf("unexpected action: %v", resp["action"])
	}
}

func TestIMWebhookRejectsMissingFields(t *testing.T) {
	server := newTestServerWithRouter(t, &fakeRouter{})
	req := newSignedWebhookRequest(t, "test-secret", imWebhookBody{BotUUID: "bot-1"})
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected bad request, got %d", rec.Code)
	}
}

func TestIMWebhookAuthExpired(t *testing.T) {
	lc := &fakeBotLifecycle{}
	server := newTestServerWithRouterAndLifecycle(t, &fakeRouter{}, lc)

	req := newSignedWebhookRequest(t, "test-secret", imWebhookBody{BotUUID: "bot-1", AuthExpired: true})
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("unexpected status %d: %s", rec.Code, rec.Body.String())
	}
	if lc.expiredBotUUID != "bot-1" {
		t.Fatalf("HandleAuthExpired not called with bot-1, got %q", lc.expiredBotUUID)
	}
	var resp map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp["action"] != "auth_expired" {
		t.Fatalf("unexpected action: %v", resp["action"])
	}
}

func TestIMWebhookPersistsUpdatesBuf(t *testing.T) {
	lc := &fakeBotLifecycle{}
	router := &fakeRouter{result: application.RouterResult{Action: application.ActionTaskCreated}}
	server := newTestServerWithRouterAndLifecycle(t, router, lc)

	req := newSignedWebhookRequest(t, "test-secret", imWebhookBody{
		BotUUID:    "bot-1",
		IlinkUserID: "user-1",
		MessageID:  "msg-1",
		Text:       "hello",
		UpdatesBuf: "cursor-123",
	})
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("unexpected status %d: %s", rec.Code, rec.Body.String())
	}
	if lc.persistBufBotUUID != "bot-1" || lc.persistBuf != "cursor-123" {
		t.Fatalf("PersistUpdatesBuf not called correctly: bot=%q buf=%q",
			lc.persistBufBotUUID, lc.persistBuf)
	}
}

// TestIMWebhookForwardsMediaPaths verifies that media_paths from the Poller
// webhook body are forwarded to the router via IncomingMessage.MediaPaths.
// Uses a capturing fake router to assert the field is plumbed through.
func TestIMWebhookForwardsMediaPaths(t *testing.T) {
	captured := &captureRouter{result: application.RouterResult{Action: application.ActionTaskCreated}}
	server := newTestServerWithRouter(t, captured)

	req := newSignedWebhookRequest(t, "test-secret", imWebhookBody{
		BotUUID:    "bot-1",
		IlinkUserID: "user-1",
		MessageID:  "msg-1",
		Text:       "see attached",
		MediaPaths: []string{"/tmp/media/bot-1/img.jpg", "/tmp/media/bot-1/img2.jpg"},
	})
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("unexpected status %d: %s", rec.Code, rec.Body.String())
	}
	if len(captured.last.MediaPaths) != 2 {
		t.Fatalf("expected 2 media paths, got %d", len(captured.last.MediaPaths))
	}
	if captured.last.MediaPaths[0] != "/tmp/media/bot-1/img.jpg" {
		t.Fatalf("first media path mismatch: %q", captured.last.MediaPaths[0])
	}
}

type captureRouter struct {
	last   application.IncomingMessage
	result application.RouterResult
	err    error
}

func (r *captureRouter) HandleMessage(_ context.Context, msg application.IncomingMessage) (application.RouterResult, error) {
	r.last = msg
	return r.result, r.err
}

func (r *captureRouter) InvalidateCommandCache() {}

func newTestServerWithRouter(t *testing.T, router application.Router) *Server {
	return newTestServerWithRouterAndLifecycle(t, router, nil)
}

func newTestServerWithRouterAndLifecycle(t *testing.T, router application.Router, lc application.BotLifecycleService) *Server {
	t.Helper()
	srv, err := NewServer(ServerConfig{
		Service:      &fakeTaskService{},
		Registry:     &fakeRegistry{},
		Router:       router,
		BotLifecycle: lc,
		DevToken:     "dev-token",
		DevUserID:    1,
		SessionKey:   "personal:1",
		WebhookSecret: "test-secret",
	})
	if err != nil {
		t.Fatalf("server: %v", err)
	}
	return srv
}

type fakeRegistry struct{}

func (r *fakeRegistry) Digest() string { return "sha256:test" }
func (r *fakeRegistry) Resolve(_, version string) (policy.ToolPolicy, error) {
	return policy.ToolPolicy{Version: version, AllowedTools: []string{"shell", "python"}}, nil
}

type fakeTaskService struct{}

func (s *fakeTaskService) SubmitTask(_ context.Context, _ domain.SubmitTaskCommand) (domain.Task, error) {
	return domain.Task{}, nil
}
func (s *fakeTaskService) GetTask(_ context.Context, _ string) (domain.Task, error) {
	return domain.Task{}, nil
}
func (s *fakeTaskService) CancelTask(_ context.Context, _ string, _ int64) (domain.Task, error) {
	return domain.Task{}, nil
}
func (s *fakeTaskService) ClaimNextTask(_ context.Context, _, _ string) (domain.Task, bool, error) {
	return domain.Task{}, false, nil
}
func (s *fakeTaskService) RecoverAfterRestart(_ context.Context, _ string) error { return nil }
func (s *fakeTaskService) ReadResult(_ context.Context, _ string) (domain.ResultPayload, error) {
	return domain.ResultPayload{}, nil
}

var _ application.TaskService = (*fakeTaskService)(nil)
var _ policy.Registry = (*fakeRegistry)(nil)

// signWebhook computes the HMAC-SHA256 signature the Bot Poller would send.
func signWebhook(t *testing.T, secret string, body []byte) string {
	t.Helper()
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

func newSignedWebhookRequest(t *testing.T, secret string, body imWebhookBody) *http.Request {
	t.Helper()
	bodyBytes, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/v1/im/webhook", bytes.NewReader(bodyBytes))
	if secret != "" {
		req.Header.Set("X-Webhook-Signature", signWebhook(t, secret, bodyBytes))
	}
	return req
}

func TestIMWebhookRejectsMissingSignature(t *testing.T) {
	router := &fakeRouter{result: application.RouterResult{Action: application.ActionTaskCreated}}
	server := newTestServerWithRouter(t, router)
	server.webhookSecret = "test-secret"

	req := newSignedWebhookRequest(t, "", imWebhookBody{ // empty secret → no header
		BotUUID: "bot-1", IlinkUserID: "user-1", MessageID: "msg-1", Text: "hi",
	})
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestIMWebhookRejectsInvalidSignature(t *testing.T) {
	router := &fakeRouter{result: application.RouterResult{Action: application.ActionTaskCreated}}
	server := newTestServerWithRouter(t, router)
	server.webhookSecret = "test-secret"

	req := newSignedWebhookRequest(t, "wrong-secret", imWebhookBody{
		BotUUID: "bot-1", IlinkUserID: "user-1", MessageID: "msg-1", Text: "hi",
	})
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for bad signature, got %d", rec.Code)
	}
}

func TestIMWebhookAcceptsValidSignature(t *testing.T) {
	router := &fakeRouter{result: application.RouterResult{
		Action: application.ActionTaskCreated, Reply: "queued",
	}}
	server := newTestServerWithRouter(t, router)
	server.webhookSecret = "test-secret"

	req := newSignedWebhookRequest(t, "test-secret", imWebhookBody{
		BotUUID: "bot-1", IlinkUserID: "user-1", MessageID: "msg-1", Text: "hello",
	})
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 with valid signature, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp["action"] != "task_created" {
		t.Fatalf("expected task_created, got %v", resp["action"])
	}
}

func TestIMWebhookRejectsTamperedBody(t *testing.T) {
	// Sign the original body, then send a different body with the same
	// signature. Must be rejected (protects against body tampering).
	router := &fakeRouter{result: application.RouterResult{Action: application.ActionTaskCreated}}
	server := newTestServerWithRouter(t, router)
	server.webhookSecret = "test-secret"

	origBody := imWebhookBody{BotUUID: "bot-1", IlinkUserID: "user-1", MessageID: "msg-1", Text: "original"}
	origBytes, _ := json.Marshal(origBody)
	tamperedBytes, _ := json.Marshal(imWebhookBody{
		BotUUID: "bot-1", IlinkUserID: "user-1", MessageID: "msg-1", Text: "tampered",
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/im/webhook", bytes.NewReader(tamperedBytes))
	req.Header.Set("X-Webhook-Signature", signWebhook(t, "test-secret", origBytes))

	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for tampered body, got %d", rec.Code)
	}
}
