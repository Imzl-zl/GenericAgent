package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/application"
	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/domain"
	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/policy"
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

	body, _ := json.Marshal(imWebhookBody{
		BotUUID:    "bot-1",
		IlinkUserID: "user-1",
		MessageID:  "msg-1",
		Text:       "hello",
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/im/webhook", bytes.NewReader(body))
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
	body, _ := json.Marshal(imWebhookBody{BotUUID: "bot-1"})
	req := httptest.NewRequest(http.MethodPost, "/v1/im/webhook", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected bad request, got %d", rec.Code)
	}
}

func TestIMWebhookAuthExpired(t *testing.T) {
	lc := &fakeBotLifecycle{}
	server := newTestServerWithRouterAndLifecycle(t, &fakeRouter{}, lc)

	body, _ := json.Marshal(imWebhookBody{BotUUID: "bot-1", AuthExpired: true})
	req := httptest.NewRequest(http.MethodPost, "/v1/im/webhook", bytes.NewReader(body))
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

	body, _ := json.Marshal(imWebhookBody{
		BotUUID:    "bot-1",
		IlinkUserID: "user-1",
		MessageID:  "msg-1",
		Text:       "hello",
		UpdatesBuf: "cursor-123",
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/im/webhook", bytes.NewReader(body))
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
