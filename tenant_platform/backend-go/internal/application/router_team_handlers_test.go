package application

import (
	"context"
	"errors"
	"testing"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/domain"
	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/infrastructure/transport"
)

// fakeTeamService is an in-memory TeamService for router tests.
type fakeTeamService struct {
	activeContext   domain.ActiveContext
	getCtxErr       error
	teams           []domain.Team
	listTeamsErr    error
	switchPersonal  domain.ActiveContext
	switchTeamErr   error
	submitMember    domain.TeamMember
	submitErr       error
	acceptMember    domain.TeamMember
	acceptErr       error
	approveMember   domain.TeamMember
	approveErr      error
	rejectMember    domain.TeamMember
	rejectErr       error
	removeMember    domain.TeamMember
	removeErr       error
	notifiedTeamID  string
	listPendingErr  error
	pendingMembers  []domain.TeamMember
}

func (f *fakeTeamService) CreateTeam(_ context.Context, _ int64, _ string) (domain.Team, error) {
	return domain.Team{}, nil
}
func (f *fakeTeamService) GenerateInviteCode(_ context.Context, _ string, _ int64) (domain.TeamInviteCode, error) {
	return domain.TeamInviteCode{}, nil
}
func (f *fakeTeamService) SubmitInviteCode(_ context.Context, _ string, _ int64) (domain.TeamMember, error) {
	return f.submitMember, f.submitErr
}
func (f *fakeTeamService) AcceptInvite(_ context.Context, _ string, _ int64) (domain.TeamMember, error) {
	return f.acceptMember, f.acceptErr
}
func (f *fakeTeamService) ApproveMember(_ context.Context, _ string, _ int64) (domain.TeamMember, error) {
	return f.approveMember, f.approveErr
}
func (f *fakeTeamService) RejectMember(_ context.Context, _ string, _ int64) (domain.TeamMember, error) {
	return f.rejectMember, f.rejectErr
}
func (f *fakeTeamService) RemoveMember(_ context.Context, _ string, _ int64) (domain.TeamMember, error) {
	return f.removeMember, f.removeErr
}
func (f *fakeTeamService) GetActiveContext(_ context.Context, _ int64) (domain.ActiveContext, error) {
	return f.activeContext, f.getCtxErr
}
func (f *fakeTeamService) SwitchToPersonal(_ context.Context, _ int64) (domain.ActiveContext, error) {
	return f.switchPersonal, nil
}
func (f *fakeTeamService) SwitchToTeam(_ context.Context, _ int64, _ string) (domain.ActiveContext, error) {
	if f.switchTeamErr != nil {
		return domain.ActiveContext{}, f.switchTeamErr
	}
	return f.activeContext, nil
}
func (f *fakeTeamService) NotifyContextShared(_ context.Context, _ int64, teamID string) (bool, error) {
	f.notifiedTeamID = teamID
	return true, nil
}
func (f *fakeTeamService) ListUserTeams(_ context.Context, _ int64) ([]domain.Team, error) {
	return f.teams, f.listTeamsErr
}
func (f *fakeTeamService) ListPendingMembers(_ context.Context, _ string, _ int64) ([]domain.TeamMember, error) {
	return f.pendingMembers, f.listPendingErr
}

func newTeamTestRouter(store *fakeRouterStore, tr *transport.LoopbackTransport, teams TeamService) Router {
	tasks := &fakeTaskService{}
	r, _ := NewRouter(RouterConfig{
		Store:          store,
		Tasks:          tasks,
		Transport:      tr,
		Messages:       &fakeMessageStore{},
		ToolPolicy:     "foundation.no-host-tools.v1",
		SourceInstance: "test-router",
		Teams:          teams,
	})
	return r
}

func boundBotStore(userID int64) *fakeRouterStore {
	store := newFakeRouterStore()
	store.bots["b1"] = domain.Bot{
		ID: 1, BotUUID: "b1", OwnerID: userID,
		IlinkUserID: "u1", State: domain.BotActive,
	}
	store.statuses[userID] = domain.UserApproved
	return store
}

func TestParseCommandArg(t *testing.T) {
	cases := []struct {
		text   string
		prefix string
		want   string
	}{
		{"/邀请码 ABC123", "/邀请码", "ABC123"},
		{"/邀请码  ABC123 ", "/邀请码", "ABC123"},
		{"/邀请码", "/邀请码", ""},
		{"/团队 项目组", "/团队", "项目组"},
		{"hello", "/团队", ""},
	}
	for _, c := range cases {
		got := parseCommandArg(c.text, c.prefix)
		if got != c.want {
			t.Errorf("parseCommandArg(%q, %q) = %q, want %q", c.text, c.prefix, got, c.want)
		}
	}
}

func TestFormatTeamError(t *testing.T) {
	cases := []struct {
		err    error
		want   string
	}{
		{domain.ErrTeamNotFound, "团队不存在"},
		{domain.ErrMemberNotFound, "成员不存在或编号无效"},
		{domain.ErrAlreadyMember, "已是团队成员"},
		{domain.ErrInviteCodeInvalid, "邀请码无效或已过期"},
		{domain.ErrNotTeamOwner, "仅团队 Owner 可执行此操作"},
		{domain.ErrWrongMemberStatus, "成员状态不允许此操作"},
		{domain.ErrActiveContextBlocked, "你不是该团队的批准成员"},
	}
	for _, c := range cases {
		got := formatTeamError(c.err, "fallback")
		if got != c.want {
			t.Errorf("formatTeamError(%v) = %q, want %q", c.err, got, c.want)
		}
	}
	got := formatTeamError(errors.New("boom"), "操作失败")
	if got != "操作失败: boom" {
		t.Errorf("formatTeamError fallback = %q", got)
	}
}

func TestRouterTeamCommandsDisabledWhenServiceNil(t *testing.T) {
	store := boundBotStore(42)
	tr := transport.NewLoopbackTransport()
	r, _ := NewRouter(RouterConfig{
		Store: store, Tasks: &fakeTaskService{},
		Transport: tr, Messages: &fakeMessageStore{},
		ToolPolicy: "v1", SourceInstance: "t",
		Teams: nil, // P0 mode
	})
	// /团队 with no team service → "功能未启用"
	res, _ := r.HandleMessage(context.Background(), IncomingMessage{
		BotUUID: "b1", IlinkUserID: "u1", MessageID: "m1", Text: "/团队",
	})
	if res.Action != ActionReplied {
		t.Fatalf("expected replied, got %s", res.Action)
	}
	if res.Reply != "团队功能未启用" {
		t.Fatalf("expected disabled reply, got %q", res.Reply)
	}
}

func TestRouterHandlePersonalSwitch(t *testing.T) {
	store := boundBotStore(42)
	tr := transport.NewLoopbackTransport()
	teams := &fakeTeamService{}
	r := newTeamTestRouter(store, tr, teams)
	res, _ := r.HandleMessage(context.Background(), IncomingMessage{
		BotUUID: "b1", IlinkUserID: "u1", MessageID: "m1", Text: "/个人",
	})
	if res.Action != ActionReplied {
		t.Fatalf("expected replied, got %s", res.Action)
	}
	if res.Reply != "已切换到个人助手" {
		t.Fatalf("unexpected reply: %q", res.Reply)
	}
}

func TestRouterHandleTeamList(t *testing.T) {
	store := boundBotStore(42)
	tr := transport.NewLoopbackTransport()
	teams := &fakeTeamService{
		teams: []domain.Team{
			{ID: "t1", Name: "项目组", OwnerID: 42},
			{ID: "t2", Name: "运维组", OwnerID: 42},
		},
	}
	r := newTeamTestRouter(store, tr, teams)
	res, _ := r.HandleMessage(context.Background(), IncomingMessage{
		BotUUID: "b1", IlinkUserID: "u1", MessageID: "m1", Text: "/团队",
	})
	if res.Action != ActionReplied {
		t.Fatalf("expected replied, got %s", res.Action)
	}
	if res.Reply == "" {
		t.Fatal("expected non-empty reply")
	}
}

func TestRouterHandleInviteCode(t *testing.T) {
	store := boundBotStore(42)
	tr := transport.NewLoopbackTransport()
	teams := &fakeTeamService{
		submitMember: domain.TeamMember{ID: 789, TeamID: "t1", UserID: 42, Status: domain.MemberPendingOwner},
	}
	r := newTeamTestRouter(store, tr, teams)
	res, _ := r.HandleMessage(context.Background(), IncomingMessage{
		BotUUID: "b1", IlinkUserID: "u1", MessageID: "m1", Text: "/邀请码 ABC123",
	})
	if res.Action != ActionReplied {
		t.Fatalf("expected replied, got %s", res.Action)
	}
	// Reply should contain the short-id t-789.
	if res.Reply == "" {
		t.Fatal("expected non-empty reply")
	}
}

func TestRouterHandleInviteCodeInvalid(t *testing.T) {
	store := boundBotStore(42)
	tr := transport.NewLoopbackTransport()
	teams := &fakeTeamService{submitErr: domain.ErrInviteCodeInvalid}
	r := newTeamTestRouter(store, tr, teams)
	res, _ := r.HandleMessage(context.Background(), IncomingMessage{
		BotUUID: "b1", IlinkUserID: "u1", MessageID: "m1", Text: "/邀请码 BADCODE",
	})
	if res.Action != ActionRejected {
		t.Fatalf("expected rejected, got %s", res.Action)
	}
	if res.Reply != "邀请码无效或已过期" {
		t.Fatalf("unexpected reply: %q", res.Reply)
	}
}

func TestRouterHandleApprove(t *testing.T) {
	store := boundBotStore(42)
	tr := transport.NewLoopbackTransport()
	teams := &fakeTeamService{
		approveMember: domain.TeamMember{ID: 456, Status: domain.MemberApproved},
	}
	r := newTeamTestRouter(store, tr, teams)
	res, _ := r.HandleMessage(context.Background(), IncomingMessage{
		BotUUID: "b1", IlinkUserID: "u1", MessageID: "m1", Text: "/批准 t-456",
	})
	if res.Action != ActionReplied {
		t.Fatalf("expected replied, got %s", res.Action)
	}
}

func TestRouterHandleApproveMissingArg(t *testing.T) {
	store := boundBotStore(42)
	tr := transport.NewLoopbackTransport()
	teams := &fakeTeamService{}
	r := newTeamTestRouter(store, tr, teams)
	res, _ := r.HandleMessage(context.Background(), IncomingMessage{
		BotUUID: "b1", IlinkUserID: "u1", MessageID: "m1", Text: "/批准",
	})
	if res.Action != ActionRejected {
		t.Fatalf("expected rejected, got %s", res.Action)
	}
	if res.Reply != "用法：/批准 t-456" {
		t.Fatalf("unexpected reply: %q", res.Reply)
	}
}

func TestRouterNormalMessageRoutesToTeamContext(t *testing.T) {
	store := boundBotStore(42)
	tr := transport.NewLoopbackTransport()
	teamID := "team-abc"
	teams := &fakeTeamService{
		activeContext: domain.ActiveContext{UserID: 42, TeamID: &teamID},
	}
	tasks := &fakeTaskService{}
	r, _ := NewRouter(RouterConfig{
		Store: store, Tasks: tasks,
		Transport: tr, Messages: &fakeMessageStore{},
		ToolPolicy: "v1", SourceInstance: "t", Teams: teams,
	})
	res, _ := r.HandleMessage(context.Background(), IncomingMessage{
		BotUUID: "b1", IlinkUserID: "u1", MessageID: "m1", Text: "处理这个任务",
	})
	if res.Action != ActionTaskCreated {
		t.Fatalf("expected task_created, got %s", res.Action)
	}
	wantSession := "team:team-abc"
	if tasks.submittedTask.SessionKey != wantSession {
		t.Fatalf("expected session %q, got %q", wantSession, tasks.submittedTask.SessionKey)
	}
}

func TestRouterNormalMessageRoutesToPersonalByDefault(t *testing.T) {
	store := boundBotStore(42)
	tr := transport.NewLoopbackTransport()
	teams := &fakeTeamService{
		activeContext: domain.ActiveContext{UserID: 42}, // personal
	}
	tasks := &fakeTaskService{}
	r, _ := NewRouter(RouterConfig{
		Store: store, Tasks: tasks,
		Transport: tr, Messages: &fakeMessageStore{},
		ToolPolicy: "v1", SourceInstance: "t", Teams: teams,
	})
	res, _ := r.HandleMessage(context.Background(), IncomingMessage{
		BotUUID: "b1", IlinkUserID: "u1", MessageID: "m1", Text: "hello",
	})
	if res.Action != ActionTaskCreated {
		t.Fatalf("expected task_created, got %s", res.Action)
	}
	wantSession := "personal:42"
	if tasks.submittedTask.SessionKey != wantSession {
		t.Fatalf("expected session %q, got %q", wantSession, tasks.submittedTask.SessionKey)
	}
}

func TestRouterHandleIdentityInTeam(t *testing.T) {
	store := boundBotStore(42)
	tr := transport.NewLoopbackTransport()
	teamID := "team-xyz"
	teams := &fakeTeamService{
		activeContext: domain.ActiveContext{UserID: 42, TeamID: &teamID},
		teams:         []domain.Team{{ID: teamID, Name: "项目组", OwnerID: 42}},
	}
	r := newTeamTestRouter(store, tr, teams)
	res, _ := r.HandleMessage(context.Background(), IncomingMessage{
		BotUUID: "b1", IlinkUserID: "u1", MessageID: "m1", Text: "/我的身份",
	})
	if res.Action != ActionReplied {
		t.Fatalf("expected replied, got %s", res.Action)
	}
	if res.Reply == "当前上下文：个人助手" {
		t.Fatal("expected team context reply, got personal")
	}
}
