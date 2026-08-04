package application

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/domain"
	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/infrastructure/transport"
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

func (f *fakeRouterStore) ResetWorkspaceForNewSession(_ context.Context, _ string) (int, error) { return 0, nil }

// fakeTaskService is a minimal TaskService for router tests.
type fakeTaskService struct {
	submittedTask domain.Task
	submitErr     error
	cancelledID   string
	cancelErr     error
	submitCount   int
	// messages 模拟真实 store 的"任务+消息行同事务"语义(round10 审查 B7):
	// 成功提交时写入 inbound 行, 失败时不写(真实实现中二者同事务回滚)。
	messages *fakeMessageStore
}

func (f *fakeTaskService) SubmitTaskWithInboundMessage(_ context.Context, cmd domain.SubmitTaskCommand, msg domain.Message) (domain.Task, domain.Message, error) {
	t, err := f.SubmitTask(context.Background(), cmd)
	if err != nil {
		return domain.Task{}, domain.Message{}, err
	}
	if f.messages != nil {
		row, ierr := f.messages.InsertInboundMessage(context.Background(), msg)
		if ierr != nil {
			return domain.Task{}, domain.Message{}, ierr
		}
		return t, row, nil
	}
	return t, domain.Message{}, nil
}

func (f *fakeTaskService) SubmitTask(_ context.Context, cmd domain.SubmitTaskCommand) (domain.Task, error) {
	f.submitCount++
	if f.submitErr != nil {
		return domain.Task{}, f.submitErr
	}
	f.submittedTask = domain.Task{ID: "task-fake", SessionKey: cmd.SessionKey, Prompt: cmd.Prompt, Source: cmd.Source, ToolPolicyVersion: cmd.ToolPolicyVersion, Status: domain.TaskQueued}
	return f.submittedTask, nil
}

func (f *fakeTaskService) GetTask(_ context.Context, _ string) (domain.Task, error) {
	return domain.Task{}, nil
}
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

func newTestRouter(store *fakeRouterStore, tr *transport.LoopbackTransport) (Router, *fakeTaskService) {
	tasks := &fakeTaskService{}
	r, _ := NewRouter(RouterConfig{
		Store:          store,
		Tasks:          tasks,
		Transport:      tr,
		Messages:       &fakeMessageStore{},
		ToolPolicy:     "foundation.no-host-tools.v1",
		SourceInstance: "test-router",
	})
	return r, tasks
}

func newTestRouterWithSessionFiles(t *testing.T, store *fakeRouterStore, tr *transport.LoopbackTransport) (Router, *fakeTaskService, SessionFiles) {
	t.Helper()
	tasks := &fakeTaskService{}
	sessionFiles, err := NewSessionFiles(t.TempDir(), "")
	if err != nil {
		t.Fatalf("new session files: %v", err)
	}
	r, _ := NewRouter(RouterConfig{
		Store:          store,
		Tasks:          tasks,
		Transport:      tr,
		Messages:       &fakeMessageStore{},
		SessionFiles:   sessionFiles,
		ToolPolicy:     "foundation.no-host-tools.v1",
		SourceInstance: "test-router",
	})
	return r, tasks, sessionFiles
}

func TestRouterRejectsMissingFields(t *testing.T) {
	tr := transport.NewLoopbackTransport()
	r, _ := newTestRouter(newFakeRouterStore(), tr)
	res, err := r.HandleMessage(context.Background(), IncomingMessage{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Action != ActionRejected {
		t.Fatalf("expected rejected, got %s", res.Action)
	}
}

// Round8 语义: 未消费的失败(unknown bot)不得标记幂等——Poller 重试必须
// 能重新处理; 若 bot 恢复存在, 同一条消息重试应能正常路由而非 Duplicate。
func TestRouterUnknownBotDoesNotConsumeIdempotency(t *testing.T) {
	store := newFakeRouterStore()
	tr := transport.NewLoopbackTransport()
	r, _ := newTestRouter(store, tr)
	msg := IncomingMessage{BotUUID: "b1", IlinkUserID: "u1", MessageID: "m1", Text: "hello"}
	if _, err := r.HandleMessage(context.Background(), msg); err != nil {
		t.Fatal(err)
	}
	// bot 恢复后, 同消息重试必须真正处理(而非被幂等缓存挡住)。
	store.bots["b1"] = domain.Bot{ID: 1, BotUUID: "b1", OwnerID: 42, IlinkUserID: "u1", State: domain.BotActive}
	store.statuses[42] = domain.UserApproved
	res, err := r.HandleMessage(context.Background(), msg)
	if err != nil {
		t.Fatal(err)
	}
	if res.Action != ActionTaskCreated {
		t.Fatalf("retry after unknown-bot failure must be processed, got %s", res.Action)
	}
	// 成功后再重试才是 Duplicate。
	res, err = r.HandleMessage(context.Background(), msg)
	if err != nil {
		t.Fatal(err)
	}
	if res.Action != ActionDuplicate {
		t.Fatalf("expected duplicate after success, got %s", res.Action)
	}
}

func TestRouterDuplicateMessageIgnored(t *testing.T) {
	store := newFakeRouterStore()
	store.bots["b1"] = domain.Bot{ID: 1, BotUUID: "b1", OwnerID: 42, IlinkUserID: "u1", State: domain.BotActive}
	store.statuses[42] = domain.UserApproved
	tr := transport.NewLoopbackTransport()
	r, _ := newTestRouter(store, tr)
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
	r, _ := newTestRouter(store, tr)
	res, _ := r.HandleMessage(context.Background(), IncomingMessage{
		BotUUID: "unknown", IlinkUserID: "u1", MessageID: "m1", Text: "hello",
	})
	if res.Action != ActionRejected {
		t.Fatalf("expected rejected, got %s", res.Action)
	}
}

func TestRouterUnboundBotRejected(t *testing.T) {
	store := newFakeRouterStore()
	store.bots["b1"] = domain.Bot{ID: 1, BotUUID: "b1", OwnerID: 42, State: domain.BotActive}
	tr := transport.NewLoopbackTransport()
	r, _ := newTestRouter(store, tr)
	res, _ := r.HandleMessage(context.Background(), IncomingMessage{
		BotUUID: "b1", IlinkUserID: "u1", MessageID: "m1", Text: "hello",
	})
	if res.Action != ActionRejected {
		t.Fatalf("expected rejected for unbound bot, got %s", res.Action)
	}
	last, ok := tr.LastSentMessage()
	if !ok || !contains(last.Text, "not bound") {
		t.Fatalf("expected 'not bound' reply, got %q", last.Text)
	}
}

func TestRouterIdentityMismatchRejected(t *testing.T) {
	store := newFakeRouterStore()
	store.bots["b1"] = domain.Bot{ID: 1, BotUUID: "b1", OwnerID: 42, IlinkUserID: "correct-user", State: domain.BotActive}
	tr := transport.NewLoopbackTransport()
	r, _ := newTestRouter(store, tr)
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
	r, _ := newTestRouter(store, tr)
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
	r, _ := newTestRouter(store, tr)
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
	r, tasks := newTestRouter(store, tr)
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
	r, tasks := newTestRouter(store, tr)
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
	r, tasks := newTestRouter(store, tr)
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

func TestRouterStagesMediaIntoSessionSandboxAndUpgradesPolicy(t *testing.T) {
	store := newFakeRouterStore()
	store.bots["b1"] = domain.Bot{ID: 1, BotUUID: "b1", OwnerID: 42, IlinkUserID: "u1", State: domain.BotActive}
	store.statuses[42] = domain.UserApproved
	src := filepath.Join(t.TempDir(), "resume.txt")
	if err := os.WriteFile(src, []byte("resume"), 0o644); err != nil {
		t.Fatalf("write src: %v", err)
	}
	tr := transport.NewLoopbackTransport()
	r, tasks, sessionFiles := newTestRouterWithSessionFiles(t, store, tr)
	res, _ := r.HandleMessage(context.Background(), IncomingMessage{
		BotUUID: "b1", IlinkUserID: "u1", MessageID: "m1", Text: "整理一下",
		MediaPaths: []string{src},
	})
	if res.Action != ActionTaskCreated {
		t.Fatalf("expected task_created, got %s", res.Action)
	}
	if tasks.submittedTask.ToolPolicyVersion != "foundation.session-files.v1" {
		t.Fatalf("unexpected policy: %s", tasks.submittedTask.ToolPolicyVersion)
	}
	if !strings.Contains(tasks.submittedTask.Prompt, "attachments/F001_resume.txt") {
		t.Fatalf("expected sandbox attachment path in prompt, got %q", tasks.submittedTask.Prompt)
	}
	refs, err := sessionFiles.Recent("personal:42", 8)
	if err != nil {
		t.Fatalf("recent: %v", err)
	}
	if len(refs) != 1 || refs[0].RelativePath != "attachments/F001_resume.txt" {
		t.Fatalf("unexpected session refs: %+v", refs)
	}
}

func TestRouterStopCancelsRunningTask(t *testing.T) {
	store := newFakeRouterStore()
	store.bots["b1"] = domain.Bot{ID: 1, BotUUID: "b1", OwnerID: 42, IlinkUserID: "u1", State: domain.BotActive}
	store.statuses[42] = domain.UserApproved
	store.runningTask = &domain.Task{ID: "task-running", SessionKey: "personal:42", Status: domain.TaskRunning}
	tr := transport.NewLoopbackTransport()
	r, tasks := newTestRouter(store, tr)
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
	r, _ := newTestRouter(store, tr)
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
	r, _ := newTestRouter(store, tr)
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
	r, _ := newTestRouter(store, tr)
	res, _ := r.HandleMessage(context.Background(), IncomingMessage{
		BotUUID: "b1", IlinkUserID: "u1", MessageID: "m1", Text: "/help",
	})
	if res.Action != ActionHelp {
		t.Fatalf("expected help, got %s", res.Action)
	}
	last, ok := tr.LastSentMessage()
	if !ok || !contains(last.Text, "可用命令") {
		t.Fatalf("expected help text with '可用命令', got %q", last.Text)
	}
	for _, hidden := range []string{"/llm", "/activate", "/session."} {
		if contains(last.Text, hidden) {
			t.Fatalf("help must not expose restricted command %q: %s", hidden, last.Text)
		}
	}
	if !contains(last.Text, "/abort") {
		t.Fatalf("help should expose /abort alias: %s", last.Text)
	}
}

func TestRouterStatusIdle(t *testing.T) {
	store := newFakeRouterStore()
	store.bots["b1"] = domain.Bot{ID: 1, BotUUID: "b1", OwnerID: 42, IlinkUserID: "u1", State: domain.BotActive}
	store.statuses[42] = domain.UserApproved
	tr := transport.NewLoopbackTransport()
	r, _ := newTestRouter(store, tr)
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
	r, _ := newTestRouter(store, tr)
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

func TestRouterLLMCommandIsRestricted(t *testing.T) {
	store := newFakeRouterStore()
	store.bots["b1"] = domain.Bot{ID: 1, BotUUID: "b1", OwnerID: 42, IlinkUserID: "u1", State: domain.BotActive}
	store.statuses[42] = domain.UserApproved
	tr := transport.NewLoopbackTransport()
	r, tasks := newTestRouter(store, tr)
	res, _ := r.HandleMessage(context.Background(), IncomingMessage{
		BotUUID: "b1", IlinkUserID: "u1", MessageID: "m1", Text: "/llm 1",
	})
	if res.Action != ActionRejected {
		t.Fatalf("expected restricted /llm to be rejected, got %s", res.Action)
	}
	if tasks.submittedTask.ID != "" {
		t.Fatalf("restricted command must not create a task: %+v", tasks.submittedTask)
	}
}

func TestRouterAbortAliasStopsRunningTask(t *testing.T) {
	store := newFakeRouterStore()
	store.bots["b1"] = domain.Bot{ID: 1, BotUUID: "b1", OwnerID: 42, IlinkUserID: "u1", State: domain.BotActive}
	store.statuses[42] = domain.UserApproved
	store.runningTask = &domain.Task{ID: "task-running", SessionKey: "personal:42", Status: domain.TaskRunning}
	tr := transport.NewLoopbackTransport()
	r, tasks := newTestRouter(store, tr)
	res, _ := r.HandleMessage(context.Background(), IncomingMessage{
		BotUUID: "b1", IlinkUserID: "u1", MessageID: "m1", Text: "/abort",
	})
	if res.Action != ActionStopped || tasks.cancelledID != "task-running" {
		t.Fatalf("expected /abort to stop task, result=%+v cancelled=%q", res, tasks.cancelledID)
	}
}

func TestRouterUnknownSlashCommandIsRejected(t *testing.T) {
	store := newFakeRouterStore()
	store.bots["b1"] = domain.Bot{ID: 1, BotUUID: "b1", OwnerID: 42, IlinkUserID: "u1", State: domain.BotActive}
	store.statuses[42] = domain.UserApproved
	tr := transport.NewLoopbackTransport()
	r, tasks := newTestRouter(store, tr)
	res, _ := r.HandleMessage(context.Background(), IncomingMessage{
		BotUUID: "b1", IlinkUserID: "u1", MessageID: "m1", Text: "/restore",
	})
	if res.Action != ActionRejected {
		t.Fatalf("expected unknown /xxx to be rejected, got %s", res.Action)
	}
	if tasks.submittedTask.ID != "" {
		t.Fatalf("unknown slash command must not create task: %+v", tasks.submittedTask)
	}
	last, _ := tr.LastSentMessage()
	if !contains(last.Text, "/help") {
		t.Fatalf("expected /help guidance, got %q", last.Text)
	}
}

func TestRouterSessionMutationCommandIsRejected(t *testing.T) {
	store := newFakeRouterStore()
	store.bots["b1"] = domain.Bot{ID: 1, BotUUID: "b1", OwnerID: 42, IlinkUserID: "u1", State: domain.BotActive}
	store.statuses[42] = domain.UserApproved
	tr := transport.NewLoopbackTransport()
	r, tasks := newTestRouter(store, tr)
	res, _ := r.HandleMessage(context.Background(), IncomingMessage{
		BotUUID: "b1", IlinkUserID: "u1", MessageID: "m1", Text: "/session.temperature=2",
	})
	if res.Action != ActionRejected || tasks.submittedTask.ID != "" {
		t.Fatalf("session mutation must be rejected before worker dispatch: result=%+v task=%+v", res, tasks.submittedTask)
	}
}

func TestRouterNormalMessageDoesNotSendImmediateReply(t *testing.T) {
	store := newFakeRouterStore()
	store.bots["b1"] = domain.Bot{ID: 1, BotUUID: "b1", OwnerID: 42, IlinkUserID: "u1", State: domain.BotActive}
	store.statuses[42] = domain.UserApproved
	tr := transport.NewLoopbackTransport()
	r, _ := newTestRouter(store, tr)
	_, _ = r.HandleMessage(context.Background(), IncomingMessage{
		BotUUID: "b1", IlinkUserID: "u1", MessageID: "m1", Text: "hello",
	})
	if sent := tr.SentMessages(); len(sent) != 0 {
		t.Fatalf("expected no immediate transport reply for normal message, got %+v", sent)
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
