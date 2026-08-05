package application

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/domain"
	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/infrastructure/transport"
)

// round12 审查(I7): 会话 key 在路由入口唯一解析一次, 任务/消息行/附件共用;
// GetActiveContext 的真实错误 fail-closed 拒绝消息, 不再静默降级个人——
// 否则团队消息可能被写入个人工作区(审计/隐私分叉)。

// TestRouterFailsClosedOnActiveContextError: 上下文解析出错(非"无行")时
// 消息必须被拒绝, 不得静默按个人上下文路由。用"第一次报错、第二次成功"
// 的 fake 确保不依赖 handleNormalMessage 的二次解析掩盖(修复前: 静默降级
// 个人并成功提交任务)。
func TestRouterFailsClosedOnActiveContextError(t *testing.T) {
	store := boundBotStore(1)
	tr := transport.NewLoopbackTransport()
	teams := &errThenPersonalTeamService{fakeTeamService: &fakeTeamService{}}
	r := newTeamTestRouter(store, tr, teams)

	_, err := r.HandleMessage(context.Background(), IncomingMessage{
		BotUUID: "b1", IlinkUserID: "u1", MessageID: "m1", Text: "hello",
	})
	if err == nil {
		t.Fatal("expected error when active context resolution fails")
	}
	taskSvc := r.(*router).tasks.(*fakeTaskService)
	if taskSvc.submitCount != 0 {
		t.Fatal("task must not be submitted when context resolution fails")
	}
}

// errThenPersonalTeamService 第一次解析返回错误, 后续返回个人上下文。
type errThenPersonalTeamService struct {
	*fakeTeamService
	calls atomic.Int32
}

func (s *errThenPersonalTeamService) GetActiveContext(ctx context.Context, userID int64) (domain.ActiveContext, error) {
	if s.calls.Add(1) == 1 {
		return domain.ActiveContext{}, errors.New("database unavailable")
	}
	return domain.ActiveContext{UserID: userID}, nil
}

// switchingTeamService 第一次解析返回团队上下文, 后续返回个人——模拟
// 并发切换/瞬时错误场景下两次独立解析可能分叉。
type switchingTeamService struct {
	*fakeTeamService
	calls atomic.Int32
}

func (s *switchingTeamService) GetActiveContext(ctx context.Context, userID int64) (domain.ActiveContext, error) {
	if s.calls.Add(1) == 1 {
		team := "11111111-2222-3333-4444-555555555555"
		return domain.ActiveContext{UserID: userID, TeamID: &team}, nil
	}
	return domain.ActiveContext{UserID: userID}, nil
}

// round13 审查(D2): @mention 的 relay 判定必须使用入口单次解析的上下文。
// 修复前 relay 分支二次解析 active context: 首次解析为团队、二次解析切换
// 为个人时, 团队消息会被误转发给其他用户(跨用户隐私泄露)。用
// switchingTeamService(第一次团队、后续个人)验证——若 relay 分支独立重查,
// 本测试会观察到 relayCalls 非空。
func TestRouterRelayUsesSingleResolvedContext(t *testing.T) {
	store := boundBotStore(1)
	tr := transport.NewLoopbackTransport()
	msgStore := &fakeMessageStore{}
	tasks := &fakeTaskService{messages: msgStore}
	relay := &fakeRelayService{}
	teams := &switchingTeamService{fakeTeamService: &fakeTeamService{}}
	r, err := NewRouter(RouterConfig{
		Store:          store,
		Tasks:          tasks,
		Transport:      tr,
		Messages:       msgStore,
		ToolPolicy:     "foundation.no-host-tools.v1",
		SourceInstance: "test-router",
		Relay:          relay,
		Teams:          teams,
	})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := r.HandleMessage(context.Background(), IncomingMessage{
		BotUUID: "b1", IlinkUserID: "u1", MessageID: "m1", Text: "@alice 你好",
	}); err != nil {
		t.Fatal(err)
	}
	if len(relay.relayCalls) != 0 {
		t.Fatalf("team-context @mention must not relay, relayCalls=%v", relay.relayCalls)
	}
	want := "team:11111111-2222-3333-4444-555555555555"
	if tasks.submittedTask.SessionKey != want {
		t.Fatalf("team-context @mention must be submitted to the team session, got %+v", tasks.submittedTask)
	}
}

// TestRouterTaskAndMessageShareSingleSessionKey: 任务与消息行必须使用同一
// 次解析的会话 key(团队), 第二次独立解析不得产生分叉。
func TestRouterTaskAndMessageShareSingleSessionKey(t *testing.T) {
	store := boundBotStore(1)
	tr := transport.NewLoopbackTransport()
	msgStore := &fakeMessageStore{}
	tasks := &fakeTaskService{messages: msgStore}
	r, err := NewRouter(RouterConfig{
		Store:          store,
		Tasks:          tasks,
		Transport:      tr,
		Messages:       msgStore,
		ToolPolicy:     "foundation.no-host-tools.v1",
		SourceInstance: "test-router",
		Teams:          &switchingTeamService{fakeTeamService: &fakeTeamService{}},
	})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := r.HandleMessage(context.Background(), IncomingMessage{
		BotUUID: "b1", IlinkUserID: "u1", MessageID: "m1", Text: "hello",
	}); err != nil {
		t.Fatal(err)
	}
	want := "team:11111111-2222-3333-4444-555555555555"
	if tasks.submittedTask.SessionKey != want {
		t.Fatalf("task session key = %q, want %q (must use the single resolved key)", tasks.submittedTask.SessionKey, want)
	}
	if len(msgStore.inbound) != 1 {
		t.Fatalf("inbound rows = %d, want 1", len(msgStore.inbound))
	}
	if got := msgStore.inbound[0].SessionKey; got != want {
		t.Fatalf("message row session key = %q, want %q (must not diverge from task)", got, want)
	}
}
