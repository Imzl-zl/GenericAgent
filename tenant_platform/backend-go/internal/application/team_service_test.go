package application

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/domain"
)

// fakeTeamStore is an in-memory TeamStore for service-level tests.
type fakeTeamStore struct {
	createdTeam      domain.Team
	createErr        error
	inviteCode       domain.TeamInviteCode
	inviteCodeErr    error
	submitMember     domain.TeamMember
	submitErr        error
	directInvite     domain.TeamMember
	directInviteErr  error
	acceptMember     domain.TeamMember
	acceptErr        error
	approveMember    domain.TeamMember
	approveErr       error
	rejectMember     domain.TeamMember
	rejectErr        error
	removeMember     domain.TeamMember
	removeErr        error
	activeContext    domain.ActiveContext
	getCtxErr        error
	setPersonalCtx   domain.ActiveContext
	setPersonalErr   error
	setTeamCtx       domain.ActiveContext
	setTeamErr       error
	notifiedTeamID   string
	notifyErr        error
	listTeams        []domain.Team
	listTeamsErr     error
	pendingMembers   []domain.TeamMember
	listPendingErr   error
	memberByShortID  domain.TeamMember
	getMemberErr     error
}

func (f *fakeTeamStore) CreateTeam(_ context.Context, _ int64, _ string) (domain.Team, error) {
	return f.createdTeam, f.createErr
}
func (f *fakeTeamStore) GenerateTeamInviteCode(_ context.Context, _ string, _ int64, _ time.Duration) (domain.TeamInviteCode, error) {
	return f.inviteCode, f.inviteCodeErr
}
func (f *fakeTeamStore) SubmitTeamInviteCode(_ context.Context, _ string, _ int64) (domain.TeamMember, error) {
	return f.submitMember, f.submitErr
}
func (f *fakeTeamStore) CreateDirectInvite(_ context.Context, _ string, _, _ int64) (domain.TeamMember, error) {
	return f.directInvite, f.directInviteErr
}
func (f *fakeTeamStore) AcceptDirectInvite(_ context.Context, _ string, _ int64) (domain.TeamMember, error) {
	return f.acceptMember, f.acceptErr
}
func (f *fakeTeamStore) ApproveMember(_ context.Context, _ string, _ int64) (domain.TeamMember, error) {
	return f.approveMember, f.approveErr
}
func (f *fakeTeamStore) RejectMember(_ context.Context, _ string, _ int64) (domain.TeamMember, error) {
	return f.rejectMember, f.rejectErr
}
func (f *fakeTeamStore) RemoveMember(_ context.Context, _ string, _ int64) (domain.TeamMember, error) {
	return f.removeMember, f.removeErr
}
func (f *fakeTeamStore) GetActiveContext(_ context.Context, _ int64) (domain.ActiveContext, error) {
	return f.activeContext, f.getCtxErr
}
func (f *fakeTeamStore) SetActiveContextPersonal(_ context.Context, _ int64) (domain.ActiveContext, error) {
	return f.setPersonalCtx, f.setPersonalErr
}
func (f *fakeTeamStore) SetActiveContextTeam(_ context.Context, _ int64, _ string) (domain.ActiveContext, error) {
	return f.setTeamCtx, f.setTeamErr
}
func (f *fakeTeamStore) MarkContextNotified(_ context.Context, _ int64, teamID string) (bool, error) {
	f.notifiedTeamID = teamID
	return true, f.notifyErr
}
func (f *fakeTeamStore) ListUserTeams(_ context.Context, _ int64) ([]domain.Team, error) {
	return f.listTeams, f.listTeamsErr
}
func (f *fakeTeamStore) ListPendingMembers(_ context.Context, _ string, _ int64) ([]domain.TeamMember, error) {
	return f.pendingMembers, f.listPendingErr
}
func (f *fakeTeamStore) GetMemberByShortID(_ context.Context, _ string) (domain.TeamMember, error) {
	return f.memberByShortID, f.getMemberErr
}

func TestNewTeamServiceRequiresStore(t *testing.T) {
	if _, err := NewTeamService(nil); err == nil {
		t.Fatal("expected error for nil store")
	}
}

func TestTeamServiceCreateTeamValidation(t *testing.T) {
	store := &fakeTeamStore{}
	svc, _ := NewTeamService(store)
	cases := []struct {
		name    string
		owner   int64
		team    string
		wantErr string
	}{
		{"empty name", 1, "", "团队名称不能为空"},
		{"whitespace name", 1, "   ", "团队名称不能为空"},
		{"invalid owner", 0, "team", "invalid owner id"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := svc.CreateTeam(context.Background(), c.owner, c.team)
			if err == nil {
				t.Fatal("expected error")
			}
			if err.Error() != c.wantErr {
				t.Errorf("got %q, want %q", err.Error(), c.wantErr)
			}
		})
	}
}

func TestTeamServiceCreateTeamNameTooLong(t *testing.T) {
	store := &fakeTeamStore{}
	svc, _ := NewTeamService(store)
	longName := make([]byte, TeamMaxNameLen+1)
	for i := range longName {
		longName[i] = 'a'
	}
	_, err := svc.CreateTeam(context.Background(), 1, string(longName))
	if err == nil {
		t.Fatal("expected length error")
	}
}

func TestTeamServiceCreateTeamDelegatesToStore(t *testing.T) {
	want := domain.Team{ID: "t1", Name: "项目组", OwnerID: 42}
	store := &fakeTeamStore{createdTeam: want}
	svc, _ := NewTeamService(store)
	got, err := svc.CreateTeam(context.Background(), 42, "项目组")
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if got.ID != want.ID || got.Name != want.Name {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

func TestTeamServiceSubmitInviteCodeValidation(t *testing.T) {
	store := &fakeTeamStore{}
	svc, _ := NewTeamService(store)
	if _, err := svc.SubmitInviteCode(context.Background(), "", 1); err == nil {
		t.Fatal("expected error for empty code")
	}
	if _, err := svc.SubmitInviteCode(context.Background(), "CODE", 0); err == nil {
		t.Fatal("expected error for invalid user")
	}
}

func TestTeamServiceSubmitInviteCodePropagatesDomainError(t *testing.T) {
	store := &fakeTeamStore{submitErr: domain.ErrInviteCodeInvalid}
	svc, _ := NewTeamService(store)
	_, err := svc.SubmitInviteCode(context.Background(), "BADCODE", 1)
	if !errors.Is(err, domain.ErrInviteCodeInvalid) {
		t.Fatalf("expected ErrInviteCodeInvalid, got %v", err)
	}
}

func TestTeamServiceApproveMemberValidation(t *testing.T) {
	store := &fakeTeamStore{}
	svc, _ := NewTeamService(store)
	if _, err := svc.ApproveMember(context.Background(), "", 1); err == nil {
		t.Fatal("expected error for empty short id")
	}
}

func TestTeamServiceSwitchToTeamValidation(t *testing.T) {
	store := &fakeTeamStore{}
	svc, _ := NewTeamService(store)
	if _, err := svc.SwitchToTeam(context.Background(), 0, "t1"); err == nil {
		t.Fatal("expected error for invalid user")
	}
	if _, err := svc.SwitchToTeam(context.Background(), 1, ""); err == nil {
		t.Fatal("expected error for empty team id")
	}
	if _, err := svc.SwitchToTeam(context.Background(), 1, "  "); err == nil {
		t.Fatal("expected error for whitespace team id")
	}
}

func TestTeamServiceSwitchToTeamPropagatesBlocked(t *testing.T) {
	store := &fakeTeamStore{setTeamErr: domain.ErrActiveContextBlocked}
	svc, _ := NewTeamService(store)
	_, err := svc.SwitchToTeam(context.Background(), 1, "t1")
	if !errors.Is(err, domain.ErrActiveContextBlocked) {
		t.Fatalf("expected ErrActiveContextBlocked, got %v", err)
	}
}

func TestTeamServiceListUserTeamsValidation(t *testing.T) {
	store := &fakeTeamStore{}
	svc, _ := NewTeamService(store)
	if _, err := svc.ListUserTeams(context.Background(), 0); err == nil {
		t.Fatal("expected error for invalid user")
	}
}
