package postgres

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/domain"
)

// TeamInviteCodeLen is the character length of a generated team invite code.
const TeamInviteCodeLen = 10

// TeamInviteCodeTTL is the default validity window for team invite codes.
const TeamInviteCodeTTL = 7 * 24 * time.Hour

// CreateTeam creates a team, a team workspace, and adds the owner as an
// approved member. The operation is atomic: any failure rolls back all three.
func (s *Store) CreateTeam(ctx context.Context, ownerID int64, name string) (domain.Team, error) {
	if ownerID <= 0 {
		return domain.Team{}, fmt.Errorf("owner user id must be positive")
	}
	if strings.TrimSpace(name) == "" {
		return domain.Team{}, fmt.Errorf("team name is required")
	}
	teamID := uuid.New()
	var out domain.Team
	err := s.withTx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `
INSERT INTO teams (id, name, owner_user_id)
VALUES ($1, $2, $3)
`, teamID, name, ownerID); err != nil {
			return fmt.Errorf("insert team: %w", err)
		}
		wsID := uuid.New()
		sessionKey := fmt.Sprintf("team:%s", teamID)
		if _, err := tx.Exec(ctx, `
INSERT INTO workspaces (id, session_key, owner_user_id, kind, team_id)
VALUES ($1, $2, $3, 'team', $4::uuid)
`, wsID, sessionKey, ownerID, teamID); err != nil {
			return fmt.Errorf("insert team workspace: %w", err)
		}
		if _, err := tx.Exec(ctx, `
INSERT INTO team_members (team_id, user_id, role, status)
VALUES ($1::uuid, $2, 'owner', 'approved')
`, teamID, ownerID); err != nil {
			return fmt.Errorf("insert owner member: %w", err)
		}
		out = domain.Team{
			ID:        teamID.String(),
			Name:      name,
			OwnerID:   ownerID,
			CreatedAt: time.Now().UTC(),
		}
		return nil
	})
	return out, err
}

// GenerateTeamInviteCode creates a one-time code for self-service join.
// The code is tied to teamID and expires after ttl.
func (s *Store) GenerateTeamInviteCode(ctx context.Context, teamID string, createdBy int64, ttl time.Duration) (domain.TeamInviteCode, error) {
	if ttl <= 0 {
		ttl = TeamInviteCodeTTL
	}
	code, err := generateTeamInviteCode(TeamInviteCodeLen)
	if err != nil {
		return domain.TeamInviteCode{}, fmt.Errorf("generate code: %w", err)
	}
	expiresAt := time.Now().Add(ttl).UTC()
	var out domain.TeamInviteCode
	err = s.withTx(ctx, func(tx pgx.Tx) error {
		dbOwner, err := verifyTeamOwner(ctx, tx, teamID)
		if err != nil {
			return err
		}
		if dbOwner != createdBy {
			return domain.ErrNotTeamOwner
		}
		if _, err := tx.Exec(ctx, `
INSERT INTO team_invite_codes (code, team_id, created_by, expires_at, state)
VALUES ($1, $2::uuid, $3, $4, 'active')
`, code, teamID, createdBy, expiresAt); err != nil {
			return fmt.Errorf("insert invite code: %w", err)
		}
		out = domain.TeamInviteCode{
			Code:      code,
			TeamID:    teamID,
			CreatedBy: createdBy,
			ExpiresAt: expiresAt,
			State:     domain.TeamInviteActive,
			CreatedAt: time.Now().UTC(),
		}
		return nil
	})
	return out, err
}

// SubmitTeamInviteCode consumes a code and creates a pending_owner membership
// for the user. The code is marked 'used' atomically. Returns the new member.
func (s *Store) SubmitTeamInviteCode(ctx context.Context, code string, userID int64) (domain.TeamMember, error) {
	var out domain.TeamMember
	err := s.withTx(ctx, func(tx pgx.Tx) error {
		var (
			teamID    uuid.UUID
			state     string
			expiresAt time.Time
		)
		err := tx.QueryRow(ctx, `
SELECT team_id, state, expires_at FROM team_invite_codes
WHERE code = $1 FOR UPDATE
`, code).Scan(&teamID, &state, &expiresAt)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrInviteCodeInvalid
		}
		if err != nil {
			return err
		}
		now := time.Now().UTC()
		if state != "active" || !now.Before(expiresAt) {
			return domain.ErrInviteCodeInvalid
		}
		// Block re-application by an existing non-removed member.
		if existing, mErr := scanActiveMember(ctx, tx, teamID, userID); mErr == nil && existing.Status != domain.MemberRemoved {
			return domain.ErrAlreadyMember
		}
		out, err = upsertPendingMember(ctx, tx, teamID, userID, domain.MemberPendingOwner, nil)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
UPDATE team_invite_codes SET state = 'used', used_by = $2, used_at = $3
WHERE code = $1
`, code, userID, now); err != nil {
			return fmt.Errorf("mark code used: %w", err)
		}
		return nil
	})
	return out, err
}

// CreateDirectInvite creates a pending_member row (owner invited user directly).
// The user must later /同意 to advance to pending_owner.
func (s *Store) CreateDirectInvite(ctx context.Context, teamID string, ownerID, inviteeID int64) (domain.TeamMember, error) {
	var out domain.TeamMember
	err := s.withTx(ctx, func(tx pgx.Tx) error {
		dbOwner, err := verifyTeamOwner(ctx, tx, teamID)
		if err != nil {
			return err
		}
		if dbOwner != ownerID {
			return domain.ErrNotTeamOwner
		}
		teamUUID, _ := uuid.Parse(teamID)
		if existing, mErr := scanActiveMember(ctx, tx, teamUUID, inviteeID); mErr == nil && existing.Status != domain.MemberRemoved {
			return domain.ErrAlreadyMember
		}
		out, err = upsertPendingMember(ctx, tx, teamUUID, inviteeID, domain.MemberPendingMember, &ownerID)
		return err
	})
	return out, err
}
