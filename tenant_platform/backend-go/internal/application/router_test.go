package application

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/domain"
	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/transport"
)

// fakeRouterStore is an in-memory RouterStore for router tests.
type fakeRouterStore struct {
	bots         map[string]domain.Bot // botUUID → bot
	statuses     map[int64]domain.UserStatus
	toolPolicies map[int64]string // userID → tool_policy_version
	runningTask  *domain.Task
	findTaskErr  error
}

func newFakeRouterStore() *fakeRouterStore {
	return &fakeRouterStore{
		bots:         make(map[string]domain.Bot),
		statuses:     make(map[int64]domain.UserStatus),
		toolPolicies: make(map[int64]string),
	}
}

func (f *fakeRouterStore) GetBotByUUID(_ context.Context, botUUID string) (domain.Bot, error) {
	b, ok := f.bots[botUUID]
	if !ok {
		return domain.Bot{}, pgx.ErrNoRows
	}
	return b, nil
}

func (f *fakeRouterStore) GetUserStatus(_ context.Context, userID int64) (domain.UserStatus, error) {
	s, ok := f.statuses[userID]
	if !ok {
		return "", fmt.Errorf("user %d not found", userID)
	}
	return s, nil
}

func (f *fakeRouterStore) GetUserToolPolicy(_ context.Context, userID int64) (string, error) {
	p, ok := f.toolPolicies[userID]
	if !ok {
		return "foundation.no-host-tools.v1", nil // default
	}
	return p, nil
}

func (f *fakeRouterStore) FindRunningTaskBySession(_ context.Context, _ string) (domain.Task, error) {
	if f.findTaskErr != nil {
		return domain.Task{}, f.findTaskErr
	}
	if f.runningTask == nil {
		return domain.Task{}, pgx.ErrNoRows
	}
	return *f.runningTask, nil
}

// fakeTaskService is a minimal TaskService for router tests.
type fakeTaskService struct {
	submittedTask domain.Task
	submitErr     error
	cancelledID   string
	cancelErr     error
}

func (f *fakeTaskService) SubmitTask(_ context.Context, cmd domain.SubmitTaskCommand) (domain.Task, error) {
	if f.submitErr != nil {
		return domain.Task{}, f.submitErr
	}
	f.submittedTask = domain.Task{ID: "task-fake", SessionKey: cmd.SessionKey, Prompt: cmd.Prompt, Source: cmd.Source, Status: domain.TaskQueued}
	return f.submittedTask, nil
}

func (f *fakeTaskService) GetTask(_ context.Context, _ string) (domain.Task, error) { return domain.Task{}, nil }
func (f *fakeTaskService) CancelTask(_ context.Context, taskID string, _ int64) (domain.Task, error) {
	f.cancelledID = taskID
	if f.cancelErr != nil {
		return domain.Task{}, f.cancelErr
	}
	return domain.Task{ID: taskID, Status: domain.TaskCancelled}, nil
}
func (f *fakeTaskService) ClaimNextTask(_ context.Context, _, _ string) (domain.Task, bool, error) {
	return domain.Task{}, false, nil
}
func (f *fakeTaskService) RecoverAfterRestart(_ context.Context, _ string) error { return nil }
func (f *fakeTaskService) ReadResult(_ context.Context, _ string) (domain.ResultPayload, error) {
	return domain.ResultPayload{}, nil
}

// fakeBindingSvc is a minimal BindingService for router tests.
type fakeBindingSvc struct {
	activateErr error
	activated   bool
}

func (f *fakeBindingSvc) GenerateBindingCode(_ context.Context, _ int64) (string, domain.BindingAttempt, error) {
	return "", domain.BindingAttempt{}, nil
}
func (f *fakeBindingSvc) Activate(_ context.Context, _, _, _ string) (domain.BindingAttempt, error) {
	if f.activateErr != nil {
		return domain.BindingAttempt{}, f.activateErr
	}
	f.activated = true
	return domain.BindingAttempt{State: domain.BindingActive}, nil
}

func newTestRouter(store *fakeRouterStore, tr *transport.LoopbackTransport) (Router, *fakeTaskService, *fakeBindingSvc) {
	tasks := &fakeTaskService{}
	binding := &fakeBindingSvc{}
	r, _ := NewRouter(RouterConfig{
		Store:         store,
		Binding:       binding,
		Tasks:         tasks,
		Transport:     tr,
		ToolPolicy:    "foundation.no-host-tools.v1",
		SourceInstance: "test-router",
	})
	return r, tasks, binding
}

func TestRouterRejectsMissingFields(t *testing.T) {
	tr := transport.NewLoopbackTransport()
	r, _, _ := newTestRouter(newFakeRouterStore(), tr)
	res, err := r.HandleMessage(context.Background(), IncomingMessage{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Action != ActionRejected {
		t.Fatalf("expected rejected, got %s", res.Action)
	}
}

func TestRouterDuplicateMessageIgnored(t *testing.T) {
	store := newFakeRouterStore()
	tr := transport.NewLoopbackTransport()
	r, _, _ := newTestRouter(store, tr)
	msg := IncomingMessage{BotUUID: "b1", IlinkUserID: "u1", MessageID: "m1", Text: "hello"}
	if _, err := r.HandleMessage(context.Background(), msg); err != nil {
		t.Fatal(err)
	}
	res, err := r.HandleMessage(context.Background(), msg)
	if err != nil {
		t.Fatal(err)
	}
	if res.Action != ActionDuplicate {
		t.Fatalf("expected duplicate, got %s", res.Action)
	}
}

func TestRouterUnknownBotRejected(t *testing.T) {
	store := newFakeRouterStore()
	tr := transport.NewLoopbackTransport()
	r, _, _ := newTestRouter(store, tr)
	res, _ := r.HandleMessage(context.Background(), IncomingMessage{
		BotUUID: "unknown", IlinkUserID: "u1", MessageID: "m1", Text: "hello",
	})
	if res.Action != ActionRejected {
		t.Fatalf("expected rejected, got %s", res.Action)
	}
}

func TestRouterUnboundBotOnlyActivateAllowed(t *testing.T) {
	store := newFakeRouterStore()
	store.bots["b1"] = domain.Bot{ID: 1, BotUUID: "b1", OwnerID: 42, State: domain.BotActive}
	tr := transport.NewLoopbackTransport()
	r, _, binding := newTestRouter(store, tr)
	res, _ := r.HandleMessage(context.Background(), IncomingMessage{
		BotUUID: "b1", IlinkUserID: "u1", MessageID: "m1", Text: "hello",
	})
	if res.Action != ActionRejected {
		t.Fatalf("expected rejected for non-activate, got %s", res.Action)
	}
	if binding.activated {
		t.Fatal("binding should not be activated for normal message")
	}
	// Now send /activate
	res, _ = r.HandleMessage(context.Background(), IncomingMessage{
		BotUUID: "b1", IlinkUserID: "u1", MessageID: "m2", Text: "/activate ABC123",
	})
	if res.Action != ActionActivated {
		t.Fatalf("expected activated, got %s", res.Action)
	}
	if !binding.activated {
		t.Fatal("binding should be activated")
	}
}

func TestRouterIdentityMismatchRejected(t *testing.T) {
	store := newFakeRouterStore()
	store.bots["b1"] = domain.Bot{ID: 1, BotUUID: "b1", OwnerID: 42, IlinkUserID: "correct-user", State: domain.BotActive}
	tr := transport.NewLoopbackTransport()
	r, _, _ := newTestRouter(store, tr)
	res, _ := r.HandleMessage(context.Background(), IncomingMessage{
		BotUUID: "b1", IlinkUserID: "wrong-user", MessageID: "m1", Text: "hello",
	})
	if res.Action != ActionRejected {
		t.Fatalf("expected rejected for identity mismatch, got %s", res.Action)
	}
}

func TestRouterBlockedUserRejected(t *testing.T) {
	store := newFakeRouterStore()
	store.bots["b1"] = domain.Bot{ID: 1, BotUUID: "b1", OwnerID: 42, IlinkUserID: "u1", State: domain.BotActive}
	store.statuses[42] = domain.UserBlocked
	tr := transport.NewLoopbackTransport()
	r, _, _ := newTestRouter(store, tr)
	res, _ := r.HandleMessage(context.Background(), IncomingMessage{
		BotUUID: "b1", IlinkUserID: "u1", MessageID: "m1", Text: "hello",
	})
	if res.Action != ActionRejected {
		t.Fatalf("expected rejected for blocked user, got %s", res.Action)
	}
}

func TestRouterPendingUserRejected(t *testing.T) {
	store := newFakeRouterStore()
	store.bots["b1"] = domain.Bot{ID: 1, BotUUID: "b1", OwnerID: 42, IlinkUserID: "u1", State: domain.BotActive}
	store.statuses[42] = domain.UserPending
	tr := transport.NewLoopbackTransport()
	r, _, _ := newTestRouter(store, tr)
	res, _ := r.HandleMessage(context.Background(), IncomingMessage{
		BotUUID: "b1", IlinkUserID: "u1", MessageID: "m1", Text: "hello",
	})
	if res.Action != ActionRejected {
		t.Fatalf("expected rejected for pending user, got %s", res.Action)
	}
}

func TestRouterNormalMessageCreatesTask(t *testing.T) {
	store := newFakeRouterStore()
	store.bots["b1"] = domain.Bot{ID: 1, BotUUID: "b1", OwnerID: 42, IlinkUserID: "u1", State: domain.BotActive}
	store.statuses[42] = domain.UserApproved
	tr := transport.NewLoopbackTransport()
	r, tasks, _ := newTestRouter(store, tr)
	res, _ := r.HandleMessage(context.Background(), IncomingMessage{
		BotUUID: "b1", IlinkUserID: "u1", MessageID: "m1", Text: "do something",
	})
	if res.Action != ActionTaskCreated {
		t.Fatalf("expected task_created, got %s", res.Action)
	}
	if tasks.submittedTask.Prompt != "do something" {
		t.Fatalf("expected prompt 'do something', got %q", tasks.submittedTask.Prompt)
	}
	if tasks.submittedTask.SessionKey != "personal:42" {
		t.Fatalf("expected session personal:42, got %s", tasks.submittedTask.SessionKey)
	}
	if tasks.submittedTask.Source != domain.SourceWechat {
		t.Fatalf("expected source wechat, got %s", tasks.submittedTask.Source)
	}
}

func TestRouterMediaPathsAppendedToPrompt(t *testing.T) {
	store := newFakeRouterStore()
	store.bots["b1"] = domain.Bot{ID: 1, BotUUID: "b1", OwnerID: 42, IlinkUserID: "u1", State: domain.BotActive}
	store.statuses[42] = domain.UserApproved
	tr := transport.NewLoopbackTransport()
	r, tasks, _ := newTestRouter(store, tr)
	res, _ := r.HandleMessage(context.Background(), IncomingMessage{
		BotUUID: "b1", IlinkUserID: "u1", MessageID: "m1", Text: "check this image",
		MediaPaths: []string{"/tmp/media/b1/img.jpg"},
	})
	if res.Action != ActionTaskCreated {
		t.Fatalf("expected task_created, got %s", res.Action)
	}
	want := "check this image\n\n[Attached files: /tmp/media/b1/img.jpg]"
	if tasks.submittedTask.Prompt != want {
		t.Fatalf("prompt mismatch:\n got: %q\nwant: %q", tasks.submittedTask.Prompt, want)
	}
}

func TestRouterMediaOnlyMessageUsesPlaceholder(t *testing.T) {
	store := newFakeRouterStore()
	store.bots["b1"] = domain.Bot{ID: 1, BotUUID: "b1", OwnerID: 42, IlinkUserID: "u1", State: domain.BotActive}
	store.statuses[42] = domain.UserApproved
	tr := transport.NewLoopbackTransport()
	r, tasks, _ := newTestRouter(store, tr)
	res, _ := r.HandleMessage(context.Background(), IncomingMessage{
		BotUUID: "b1", IlinkUserID: "u1", MessageID: "m1", Text: "",
		MediaPaths: []string{"/tmp/media/b1/a.pdf", "/tmp/media/b1/b.pdf"},
	})
	if res.Action != ActionTaskCreated {
		t.Fatalf("expected task_created, got %s", res.Action)
	}
	want := "[media message]\n\n[Attached files: /tmp/media/b1/a.pdf, /tmp/media/b1/b.pdf]"
	if tasks.submittedTask.Prompt != want {
		t.Fatalf("prompt mismatch:\n got: %q\nwant: %q", tasks.submittedTask.Prompt, want)
	}
}

func TestRouterStopCancelsRunningTask(t *testing.T) {
	store := newFakeRouterStore()
	store.bots["b1"] = domain.Bot{ID: 1, BotUUID: "b1", OwnerID: 42, IlinkUserID: "u1", State: domain.BotActive}
	store.statuses[42] = domain.UserApproved
	store.runningTask = &domain.Task{ID: "task-running", SessionKey: "personal:42", Status: domain.TaskRunning}
	tr := transport.NewLoopbackTransport()
	r, tasks, _ := newTestRouter(store, tr)
	res, _ := r.HandleMessage(context.Background(), IncomingMessage{
		BotUUID: "b1", IlinkUserID: "u1", MessageID: "m1", Text: "/stop",
	})
	if res.Action != ActionStopped {
		t.Fatalf("expected stopped, got %s", res.Action)
	}
	if tasks.cancelledID != "task-running" {
		t.Fatalf("expected task-running cancelled, got %s", tasks.cancelledID)
	}
}

func TestRouterStopWithNoRunningTask(t *testing.T) {
	store := newFakeRouterStore()
	store.bots["b1"] = domain.Bot{ID: 1, BotUUID: "b1", OwnerID: 42, IlinkUserID: "u1", State: domain.BotActive}
	store.statuses[42] = domain.UserApproved
	tr := transport.NewLoopbackTransport()
	r, _, _ := newTestRouter(store, tr)
	res, _ := r.HandleMessage(context.Background(), IncomingMessage{
		BotUUID: "b1", IlinkUserID: "u1", MessageID: "m1", Text: "/stop",
	})
	if res.Action != ActionNoRunning {
		t.Fatalf("expected no_running, got %s", res.Action)
	}
}

func TestRouterNewCommandAcknowledged(t *testing.T) {
	store := newFakeRouterStore()
	store.bots["b1"] = domain.Bot{ID: 1, BotUUID: "b1", OwnerID: 42, IlinkUserID: "u1", State: domain.BotActive}
	store.statuses[42] = domain.UserApproved
	tr := transport.NewLoopbackTransport()
	r, _, _ := newTestRouter(store, tr)
	res, _ := r.HandleMessage(context.Background(), IncomingMessage{
		BotUUID: "b1", IlinkUserID: "u1", MessageID: "m1", Text: "/new",
	})
	if res.Action != ActionNewSession {
		t.Fatalf("expected new_session, got %s", res.Action)
	}
}

func TestRouterHelpCommand(t *testing.T) {
	store := newFakeRouterStore()
	store.bots["b1"] = domain.Bot{ID: 1, BotUUID: "b1", OwnerID: 42, IlinkUserID: "u1", State: domain.BotActive}
	store.statuses[42] = domain.UserApproved
	tr := transport.NewLoopbackTransport()
	r, _, _ := newTestRouter(store, tr)
	res, _ := r.HandleMessage(context.Background(), IncomingMessage{
		BotUUID: "b1", IlinkUserID: "u1", MessageID: "m1", Text: "/help",
	})
	if res.Action != ActionHelp {
		t.Fatalf("expected help, got %s", res.Action)
	}
	last, ok := tr.LastSentMessage()
	if !ok || !contains(last.Text, "平台命令") {
		t.Fatalf("expected help text with '平台命令', got %q", last.Text)
	}
}

func TestRouterStatusIdle(t *testing.T) {
	store := newFakeRouterStore()
	store.bots["b1"] = domain.Bot{ID: 1, BotUUID: "b1", OwnerID: 42, IlinkUserID: "u1", State: domain.BotActive}
	store.statuses[42] = domain.UserApproved
	tr := transport.NewLoopbackTransport()
	r, _, _ := newTestRouter(store, tr)
	res, _ := r.HandleMessage(context.Background(), IncomingMessage{
		BotUUID: "b1", IlinkUserID: "u1", MessageID: "m1", Text: "/status",
	})
	if res.Action != ActionStatus {
		t.Fatalf("expected status, got %s", res.Action)
	}
	last, _ := tr.LastSentMessage()
	if !contains(last.Text, "idle") {
		t.Fatalf("expected idle status, got %q", last.Text)
	}
}

func TestRouterStatusRunning(t *testing.T) {
	store := newFakeRouterStore()
	store.bots["b1"] = domain.Bot{ID: 1, BotUUID: "b1", OwnerID: 42, IlinkUserID: "u1", State: domain.BotActive}
	store.statuses[42] = domain.UserApproved
	store.runningTask = &domain.Task{ID: "task-1", Status: domain.TaskRunning}
	tr := transport.NewLoopbackTransport()
	r, _, _ := newTestRouter(store, tr)
	res, _ := r.HandleMessage(context.Background(), IncomingMessage{
		BotUUID: "b1", IlinkUserID: "u1", MessageID: "m1", Text: "/status",
	})
	if res.Action != ActionStatus {
		t.Fatalf("expected status, got %s", res.Action)
	}
	last, _ := tr.LastSentMessage()
	if !contains(last.Text, "running") {
		t.Fatalf("expected running status, got %q", last.Text)
	}
}

func TestRouterLLMCommand(t *testing.T) {
	store := newFakeRouterStore()
	store.bots["b1"] = domain.Bot{ID: 1, BotUUID: "b1", OwnerID: 42, IlinkUserID: "u1", State: domain.BotActive}
	store.statuses[42] = domain.UserApproved
	tr := transport.NewLoopbackTransport()
	r, _, _ := newTestRouter(store, tr)
	res, _ := r.HandleMessage(context.Background(), IncomingMessage{
		BotUUID: "b1", IlinkUserID: "u1", MessageID: "m1", Text: "/llm",
	})
	if res.Action != ActionModelInfo {
		t.Fatalf("expected model_info, got %s", res.Action)
	}
	last, _ := tr.LastSentMessage()
	if !contains(last.Text, "LLM Proxy") {
		t.Fatalf("expected LLM Proxy mention, got %q", last.Text)
	}
}

func TestRouterUnknownSlashCommandForwardedAsTask(t *testing.T) {
	store := newFakeRouterStore()
	store.bots["b1"] = domain.Bot{ID: 1, BotUUID: "b1", OwnerID: 42, IlinkUserID: "u1", State: domain.BotActive}
	store.statuses[42] = domain.UserApproved
	tr := transport.NewLoopbackTransport()
	r, tasks, _ := newTestRouter(store, tr)
	res, _ := r.HandleMessage(context.Background(), IncomingMessage{
		BotUUID: "b1", IlinkUserID: "u1", MessageID: "m1", Text: "/restore",
	})
	if res.Action != ActionTaskCreated {
		t.Fatalf("expected task_created for unknown /xxx, got %s", res.Action)
	}
	if tasks.submittedTask.Prompt != "/restore" {
		t.Fatalf("expected prompt '/restore' forwarded verbatim, got %q", tasks.submittedTask.Prompt)
	}
}

func TestRouterActivateFailsSendsErrorReply(t *testing.T) {
	store := newFakeRouterStore()
	store.bots["b1"] = domain.Bot{ID: 1, BotUUID: "b1", OwnerID: 42, State: domain.BotActive}
	tr := transport.NewLoopbackTransport()
	r, _, binding := newTestRouter(store, tr)
	binding.activateErr = errors.New("code expired")
	res, _ := r.HandleMessage(context.Background(), IncomingMessage{
		BotUUID: "b1", IlinkUserID: "u1", MessageID: "m1", Text: "/activate BADCODE",
	})
	if res.Action != ActionRejected {
		t.Fatalf("expected rejected, got %s", res.Action)
	}
	last, ok := tr.LastSentMessage()
	if !ok || !contains(last.Text, "activation failed") {
		t.Fatalf("expected 'activation failed' reply, got %q", last.Text)
	}
}

func TestRouterSendsReplyViaTransport(t *testing.T) {
	store := newFakeRouterStore()
	store.bots["b1"] = domain.Bot{ID: 1, BotUUID: "b1", OwnerID: 42, IlinkUserID: "u1", State: domain.BotActive}
	store.statuses[42] = domain.UserApproved
	tr := transport.NewLoopbackTransport()
	r, _, _ := newTestRouter(store, tr)
	_, _ = r.HandleMessage(context.Background(), IncomingMessage{
		BotUUID: "b1", IlinkUserID: "u1", MessageID: "m1", Text: "hello",
	})
	sent := tr.SentMessages()
	if len(sent) == 0 {
		t.Fatal("expected at least one reply sent via transport")
	}
	if sent[0].BotUUID != "b1" || sent[0].IlinkUserID != "u1" {
		t.Fatalf("reply sent to wrong recipient: %+v", sent[0])
	}
}

func TestParseActivateCommand(t *testing.T) {
	cases := []struct {
		input string
		code  string
		ok    bool
	}{
		{"/activate ABC123", "ABC123", true},
		{"/activate  ABC123 ", "ABC123", true},
		{"/activate", "", false},
		{"hello", "", false},
		{"", "", false},
	}
	for _, c := range cases {
		code, ok := parseActivateCommand(c.input)
		if code != c.code || ok != c.ok {
			t.Errorf("parseActivateCommand(%q) = (%q, %v), want (%q, %v)", c.input, code, ok, c.code, c.ok)
		}
	}
}

func TestNewRouterRejectsNilDeps(t *testing.T) {
	if _, err := NewRouter(RouterConfig{}); err == nil {
		t.Fatal("expected error for nil deps")
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > 0 && containsStr(s, substr))
}

func containsStr(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
