package application

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/domain"
)

// TeamStore is the persistence port for team lifecycle and context operations.
// Implemented by *postgres.Store; defined here so the router/service can be
// wired against an interface without importing postgres.
type TeamStore interface {
	CreateTeam(ctx context.Context, ownerID int64, name string) (domain.Team, error)
	GenerateTeamInviteCode(ctx context.Context, teamID string, createdBy int64, ttl time.Duration) (domain.TeamInviteCode, error)
	SubmitTeamInviteCode(ctx context.Context, code string, userID int64) (domain.TeamMember, error)
	CreateDirectInvite(ctx context.Context, teamID string, ownerID, inviteeID int64) (domain.TeamMember, error)
	AcceptDirectInvite(ctx context.Context, shortID string, userID int64) (domain.TeamMember, error)
	ApproveMember(ctx context.Context, shortID string, ownerID int64) (domain.TeamMember, error)
	RejectMember(ctx context.Context, shortID string, ownerID int64) (domain.TeamMember, error)
	RemoveMember(ctx context.Context, shortID string, ownerID int64) (domain.TeamMember, error)
	GetActiveContext(ctx context.Context, userID int64) (domain.ActiveContext, error)
	SetActiveContextPersonal(ctx context.Context, userID int64) (domain.ActiveContext, error)
	SetActiveContextTeam(ctx context.Context, userID int64, teamID string) (domain.ActiveContext, error)
	MarkContextNotified(ctx context.Context, userID int64, teamID string) (bool, error)
	ListUserTeams(ctx context.Context, userID int64) ([]domain.Team, error)
	ListPendingMembers(ctx context.Context, teamID string, ownerID int64) ([]domain.TeamMember, error)
	GetMemberByShortID(ctx context.Context, shortID string) (domain.TeamMember, error)
}

// TeamMaxNameLen caps team name length (PRD §5: bounded, non-empty).
const TeamMaxNameLen = 64

// TeamService is the application-facing team API. The router calls these
// methods to action /团队 /个人 /邀请码 /同意 /批准 /拒绝 /移除 commands.
type TeamService interface {
	CreateTeam(ctx context.Context, ownerID int64, name string) (domain.Team, error)
	GenerateInviteCode(ctx context.Context, teamID string, ownerID int64) (domain.TeamInviteCode, error)
	SubmitInviteCode(ctx context.Context, code string, userID int64) (domain.TeamMember, error)
	AcceptInvite(ctx context.Context, shortID string, userID int64) (domain.TeamMember, error)
	ApproveMember(ctx context.Context, shortID string, ownerID int64) (domain.TeamMember, error)
	RejectMember(ctx context.Context, shortID string, ownerID int64) (domain.TeamMember, error)
	RemoveMember(ctx context.Context, shortID string, ownerID int64) (domain.TeamMember, error)
	GetActiveContext(ctx context.Context, userID int64) (domain.ActiveContext, error)
	SwitchToPersonal(ctx context.Context, userID int64) (domain.ActiveContext, error)
	SwitchToTeam(ctx context.Context, userID int64, teamID string) (domain.ActiveContext, error)
	NotifyContextShared(ctx context.Context, userID int64, teamID string) (bool, error)
	ListUserTeams(ctx context.Context, userID int64) ([]domain.Team, error)
	ListPendingMembers(ctx context.Context, teamID string, ownerID int64) ([]domain.TeamMember, error)
}

type teamService struct {
	store TeamStore
}

// NewTeamService constructs the service. store must be non-nil.
func NewTeamService(store TeamStore) (TeamService, error) {
	if store == nil {
		return nil, fmt.Errorf("team store is required")
	}
	return &teamService{store: store}, nil
}

func (s *teamService) CreateTeam(ctx context.Context, ownerID int64, name string) (domain.Team, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return domain.Team{}, fmt.Errorf("团队名称不能为空")
	}
	if len(name) > TeamMaxNameLen {
		return domain.Team{}, fmt.Errorf("团队名称不能超过 %d 字符", TeamMaxNameLen)
	}
	if ownerID <= 0 {
		return domain.Team{}, fmt.Errorf("invalid owner id")
	}
	return s.store.CreateTeam(ctx, ownerID, name)
}

func (s *teamService) GenerateInviteCode(ctx context.Context, teamID string, ownerID int64) (domain.TeamInviteCode, error) {
	if strings.TrimSpace(teamID) == "" {
		return domain.TeamInviteCode{}, fmt.Errorf("team id is required")
	}
	return s.store.GenerateTeamInviteCode(ctx, teamID, ownerID, 0)
}

func (s *teamService) SubmitInviteCode(ctx context.Context, code string, userID int64) (domain.TeamMember, error) {
	code = strings.TrimSpace(code)
	if code == "" {
		return domain.TeamMember{}, fmt.Errorf("邀请码不能为空")
	}
	if userID <= 0 {
		return domain.TeamMember{}, fmt.Errorf("invalid user id")
	}
	return s.store.SubmitTeamInviteCode(ctx, code, userID)
}

func (s *teamService) AcceptInvite(ctx context.Context, shortID string, userID int64) (domain.TeamMember, error) {
	shortID = strings.TrimSpace(shortID)
	if shortID == "" {
		return domain.TeamMember{}, fmt.Errorf("成员编号不能为空")
	}
	return s.store.AcceptDirectInvite(ctx, shortID, userID)
}

func (s *teamService) ApproveMember(ctx context.Context, shortID string, ownerID int64) (domain.TeamMember, error) {
	shortID = strings.TrimSpace(shortID)
	if shortID == "" {
		return domain.TeamMember{}, fmt.Errorf("成员编号不能为空")
	}
	return s.store.ApproveMember(ctx, shortID, ownerID)
}

func (s *teamService) RejectMember(ctx context.Context, shortID string, ownerID int64) (domain.TeamMember, error) {
	shortID = strings.TrimSpace(shortID)
	if shortID == "" {
		return domain.TeamMember{}, fmt.Errorf("成员编号不能为空")
	}
	return s.store.RejectMember(ctx, shortID, ownerID)
}

func (s *teamService) RemoveMember(ctx context.Context, shortID string, ownerID int64) (domain.TeamMember, error) {
	shortID = strings.TrimSpace(shortID)
	if shortID == "" {
		return domain.TeamMember{}, fmt.Errorf("成员编号不能为空")
	}
	return s.store.RemoveMember(ctx, shortID, ownerID)
}

func (s *teamService) GetActiveContext(ctx context.Context, userID int64) (domain.ActiveContext, error) {
	if userID <= 0 {
		return domain.ActiveContext{}, fmt.Errorf("invalid user id")
	}
	return s.store.GetActiveContext(ctx, userID)
}

func (s *teamService) SwitchToPersonal(ctx context.Context, userID int64) (domain.ActiveContext, error) {
	if userID <= 0 {
		return domain.ActiveContext{}, fmt.Errorf("invalid user id")
	}
	return s.store.SetActiveContextPersonal(ctx, userID)
}

func (s *teamService) SwitchToTeam(ctx context.Context, userID int64, teamID string) (domain.ActiveContext, error) {
	if userID <= 0 {
		return domain.ActiveContext{}, fmt.Errorf("invalid user id")
	}
	teamID = strings.TrimSpace(teamID)
	if teamID == "" {
		return domain.ActiveContext{}, fmt.Errorf("team id is required")
	}
	return s.store.SetActiveContextTeam(ctx, userID, teamID)
}

func (s *teamService) NotifyContextShared(ctx context.Context, userID int64, teamID string) (bool, error) {
	return s.store.MarkContextNotified(ctx, userID, teamID)
}

func (s *teamService) ListUserTeams(ctx context.Context, userID int64) ([]domain.Team, error) {
	if userID <= 0 {
		return nil, fmt.Errorf("invalid user id")
	}
	return s.store.ListUserTeams(ctx, userID)
}

func (s *teamService) ListPendingMembers(ctx context.Context, teamID string, ownerID int64) ([]domain.TeamMember, error) {
	if strings.TrimSpace(teamID) == "" {
		return nil, fmt.Errorf("team id is required")
	}
	return s.store.ListPendingMembers(ctx, teamID, ownerID)
}
