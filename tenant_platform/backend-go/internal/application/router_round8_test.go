package application

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/domain"
	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/infrastructure/transport"
)

// round8Router 构造带可观测 MessageStore 的 router。
func round8Router(store *fakeRouterStore, tr *transport.LoopbackTransport, messages *fakeMessageStore, tasks *fakeTaskService) Router {
	r, _ := NewRouter(RouterConfig{
		Store:          store,
		Tasks:          tasks,
		Transport:      tr,
		Messages:       messages,
		ToolPolicy:     "foundation.no-host-tools.v1",
		SourceInstance: "test-router",
	})
	return r
}

// Round8 审查: 身份不匹配的发送者不得向目标租户写入任何 messages/media_assets 记录。
func TestRouterIdentityMismatchDoesNotPersistInbound(t *testing.T) {
	store := newFakeRouterStore()
	store.bots["b1"] = domain.Bot{ID: 1, BotUUID: "b1", OwnerID: 42, IlinkUserID: "correct-user", State: domain.BotActive}
	store.statuses[42] = domain.UserApproved
	tr := transport.NewLoopbackTransport()
	messages := &fakeMessageStore{}
	tasks := &fakeTaskService{}
	r := round8Router(store, tr, messages, tasks)

	res, err := r.HandleMessage(context.Background(), IncomingMessage{
		BotUUID: "b1", IlinkUserID: "wrong-user", MessageID: "m1", Text: "hello",
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Action != ActionRejected {
		t.Fatalf("expected rejected for identity mismatch, got %s", res.Action)
	}
	if len(messages.inbound) != 0 {
		t.Fatalf("identity mismatch must not persist inbound message, got %d rows", len(messages.inbound))
	}
	if len(messages.assets) != 0 {
		t.Fatalf("identity mismatch must not persist media assets, got %d rows", len(messages.assets))
	}
	if tasks.submittedTask.ID != "" {
		t.Fatalf("identity mismatch must not submit a task")
	}
}

// Round8 审查: 未绑定 bot 的消息不得写入租户记录。
func TestRouterUnboundBotDoesNotPersistInbound(t *testing.T) {
	store := newFakeRouterStore()
	store.bots["b1"] = domain.Bot{ID: 1, BotUUID: "b1", OwnerID: 42, State: domain.BotActive}
	store.statuses[42] = domain.UserApproved
	tr := transport.NewLoopbackTransport()
	messages := &fakeMessageStore{}
	r := round8Router(store, tr, messages, &fakeTaskService{})

	res, err := r.HandleMessage(context.Background(), IncomingMessage{
		BotUUID: "b1", IlinkUserID: "u1", MessageID: "m1", Text: "hello",
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Action != ActionRejected {
		t.Fatalf("expected rejected for unbound bot, got %s", res.Action)
	}
	if len(messages.inbound) != 0 || len(messages.assets) != 0 {
		t.Fatalf("unbound bot must not persist records: inbound=%d assets=%d", len(messages.inbound), len(messages.assets))
	}
}

// Round8 审查: 被阻止/待审用户的消息不得写入租户记录。
func TestRouterBlockedUserDoesNotPersistInbound(t *testing.T) {
	store := newFakeRouterStore()
	store.bots["b1"] = domain.Bot{ID: 1, BotUUID: "b1", OwnerID: 42, IlinkUserID: "u1", State: domain.BotActive}
	store.statuses[42] = domain.UserBlocked
	tr := transport.NewLoopbackTransport()
	messages := &fakeMessageStore{}
	r := round8Router(store, tr, messages, &fakeTaskService{})

	res, err := r.HandleMessage(context.Background(), IncomingMessage{
		BotUUID: "b1", IlinkUserID: "u1", MessageID: "m1", Text: "hello",
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Action != ActionRejected {
		t.Fatalf("expected rejected for blocked user, got %s", res.Action)
	}
	if len(messages.inbound) != 0 || len(messages.assets) != 0 {
		t.Fatalf("blocked user must not persist records: inbound=%d assets=%d", len(messages.inbound), len(messages.assets))
	}
}

// Round8 审查: 任务提交失败必须返回 error(不标记幂等), Poller 重试能真正重新处理。
func TestRouterFailedTaskSubmitIsRetryable(t *testing.T) {
	store := newFakeRouterStore()
	store.bots["b1"] = domain.Bot{ID: 1, BotUUID: "b1", OwnerID: 42, IlinkUserID: "u1", State: domain.BotActive}
	store.statuses[42] = domain.UserApproved
	tr := transport.NewLoopbackTransport()
	messages := &fakeMessageStore{}
	tasks := &fakeTaskService{submitErr: errors.New("database unavailable")}
	r := round8Router(store, tr, messages, tasks)

	_, err := r.HandleMessage(context.Background(), IncomingMessage{
		BotUUID: "b1", IlinkUserID: "u1", MessageID: "m1", Text: "do something",
	})
	if err == nil {
		t.Fatal("expected error when task submission fails")
	}
	// 失败路径不得持久化消息(否则重试会撞 DB 唯一键提前返回)。
	if len(messages.inbound) != 0 {
		t.Fatalf("failed submission must not persist inbound message, got %d rows", len(messages.inbound))
	}
	// Poller 重试: 必须能真正重新处理并创建任务。
	tasks.submitErr = nil
	res, err := r.HandleMessage(context.Background(), IncomingMessage{
		BotUUID: "b1", IlinkUserID: "u1", MessageID: "m1", Text: "do something",
	})
	if err != nil {
		t.Fatalf("retry after transient failure must succeed, got: %v", err)
	}
	if res.Action != ActionTaskCreated {
		t.Fatalf("expected task_created on retry, got %s", res.Action)
	}
	if tasks.submittedTask.Prompt != "do something" {
		t.Fatalf("expected prompt on retry, got %q", tasks.submittedTask.Prompt)
	}
	// 成功后消息入库一次且幂等标记生效: 第三次直接 Duplicate。
	dupe, err := r.HandleMessage(context.Background(), IncomingMessage{
		BotUUID: "b1", IlinkUserID: "u1", MessageID: "m1", Text: "do something",
	})
	if err != nil {
		t.Fatal(err)
	}
	if dupe.Action != ActionDuplicate {
		t.Fatalf("expected duplicate after success, got %s", dupe.Action)
	}
	if len(messages.inbound) != 1 {
		t.Fatalf("expected exactly 1 inbound row after success, got %d", len(messages.inbound))
	}
}

// Round8 审查: media_paths 必须绑定 BotMediaRoot; 越界路径拒绝且不产生副作用。
func TestRouterRejectsMediaPathOutsideBotMediaRoot(t *testing.T) {
	store := newFakeRouterStore()
	store.bots["b1"] = domain.Bot{ID: 1, BotUUID: "b1", OwnerID: 42, IlinkUserID: "u1", State: domain.BotActive}
	store.statuses[42] = domain.UserApproved
	tr := transport.NewLoopbackTransport()
	messages := &fakeMessageStore{}
	tasks := &fakeTaskService{}
	root := t.TempDir()
	r, _ := NewRouter(RouterConfig{
		Store:          store,
		Tasks:          tasks,
		Transport:      tr,
		Messages:       messages,
		ToolPolicy:     "foundation.no-host-tools.v1",
		SourceInstance: "test-router",
		BotMediaRoot:   root,
	})

	secret := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(secret, []byte("s3cret"), 0o600); err != nil {
		t.Fatal(err)
	}
	res, err := r.HandleMessage(context.Background(), IncomingMessage{
		BotUUID:     "b1",
		IlinkUserID: "u1",
		MessageID:   "m1",
		Text:        "read this",
		MediaPaths:  []string{secret},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Action != ActionRejected {
		t.Fatalf("expected rejected for out-of-root media path, got %s", res.Action)
	}
	if tasks.submittedTask.ID != "" || len(messages.inbound) != 0 {
		t.Fatalf("out-of-root media path must not create task or persist: task=%q inbound=%d",
			tasks.submittedTask.ID, len(messages.inbound))
	}
}

// Round8 审查: media_paths 在 BotMediaRoot 内且文件真实存在时正常放行。
func TestRouterAcceptsMediaPathInsideBotMediaRoot(t *testing.T) {
	store := newFakeRouterStore()
	store.bots["b1"] = domain.Bot{ID: 1, BotUUID: "b1", OwnerID: 42, IlinkUserID: "u1", State: domain.BotActive}
	store.statuses[42] = domain.UserApproved
	tr := transport.NewLoopbackTransport()
	messages := &fakeMessageStore{}
	tasks := &fakeTaskService{}
	root := t.TempDir()
	media := filepath.Join(root, "b1", "img.jpg")
	if err := os.MkdirAll(filepath.Dir(media), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(media, []byte("jpg"), 0o644); err != nil {
		t.Fatal(err)
	}
	r, _ := NewRouter(RouterConfig{
		Store:          store,
		Tasks:          tasks,
		Transport:      tr,
		Messages:       messages,
		ToolPolicy:     "foundation.no-host-tools.v1",
		SourceInstance: "test-router",
		BotMediaRoot:   root,
	})

	res, err := r.HandleMessage(context.Background(), IncomingMessage{
		BotUUID:     "b1",
		IlinkUserID: "u1",
		MessageID:   "m1",
		Text:        "check image",
		MediaPaths:  []string{media},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Action != ActionTaskCreated {
		t.Fatalf("expected task_created for in-root media path, got %s", res.Action)
	}
}

// Round8 审查: 成功处理后同消息重试被幂等缓存判定为 Duplicate(不重复创建任务)。
func TestRouterSuccessfulMessageMarkedIdempotent(t *testing.T) {
	store := newFakeRouterStore()
	store.bots["b1"] = domain.Bot{ID: 1, BotUUID: "b1", OwnerID: 42, IlinkUserID: "u1", State: domain.BotActive}
	store.statuses[42] = domain.UserApproved
	tr := transport.NewLoopbackTransport()
	messages := &fakeMessageStore{}
	tasks := &fakeTaskService{}
	r := round8Router(store, tr, messages, tasks)

	msg := IncomingMessage{BotUUID: "b1", IlinkUserID: "u1", MessageID: "m1", Text: "hello"}
	res, err := r.HandleMessage(context.Background(), msg)
	if err != nil || res.Action != ActionTaskCreated {
		t.Fatalf("first handling: action=%s err=%v", res.Action, err)
	}
	seen, err := tr.CheckMessageIdempotency(context.Background(), "b1", "m1")
	if err != nil {
		t.Fatal(err)
	}
	if !seen {
		t.Fatal("successfully processed message must be marked idempotent")
	}
	dupe, err := r.HandleMessage(context.Background(), msg)
	if err != nil {
		t.Fatal(err)
	}
	if dupe.Action != ActionDuplicate {
		t.Fatalf("expected duplicate, got %s", dupe.Action)
	}
}

var _ = errors.Is // keep errors import when tests above are edited

// round9 审查: 重启/多实例后内存 seen 缓存变冷时, 消息行已存在必须短路
// 路由——relay 转发与团队命令无唯一键兜底, 重复执行会产生重复副作用。
// 该测试模拟"内存缓存空 + messages 行已存在"的重启窗口。
func TestRouterDurableInboundDedupShortCircuitsRouting(t *testing.T) {
	store := newFakeRouterStore()
	store.bots["b1"] = domain.Bot{ID: 1, BotUUID: "b1", OwnerID: 42, IlinkUserID: "u1", State: domain.BotActive}
	store.statuses[42] = domain.UserApproved
	tr := transport.NewLoopbackTransport()
	messages := &fakeMessageStore{}
	tasks := &fakeTaskService{}
	r := round8Router(store, tr, messages, tasks)

	// 首次处理成功: 任务提交 + 消息入库。
	msg := IncomingMessage{BotUUID: "b1", IlinkUserID: "u1", MessageID: "m1", Text: "hello"}
	res, err := r.HandleMessage(context.Background(), msg)
	if err != nil || res.Action != ActionTaskCreated {
		t.Fatalf("first handling: action=%s err=%v", res.Action, err)
	}
	if len(messages.inbound) != 1 || tasks.submittedTask.ID == "" {
		t.Fatalf("first handling must persist message and submit task: inbound=%d task=%q", len(messages.inbound), tasks.submittedTask.ID)
	}

	// 模拟重启: 内存 seen 清空, messages 行保留。
	tr.Reset()

	dupe, err := r.HandleMessage(context.Background(), msg)
	if err != nil {
		t.Fatal(err)
	}
	if dupe.Action != ActionDuplicate {
		t.Fatalf("expected durable duplicate, got %s", dupe.Action)
	}
	// 不得再次提交任务/重复入库/发送回复。
	if tasks.submitCount != 1 || len(messages.inbound) != 1 {
		t.Fatalf("duplicate must not re-route: submits=%d inbound=%d", tasks.submitCount, len(messages.inbound))
	}
	if len(tr.SentMessages()) != 0 {
		t.Fatalf("duplicate must not re-send replies: %d", len(tr.SentMessages()))
	}
}
