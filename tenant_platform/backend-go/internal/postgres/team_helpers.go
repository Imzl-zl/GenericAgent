package postgres

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/domain"
)

// parseMemberShortID parses "t-456" into 456. Returns error on malformed input.
func parseMemberShortID(s string) (int64, error) {
	if !strings.HasPrefix(s, "t-") {
		return 0, fmt.Errorf("invalid short id %q", s)
	}
	num := s[2:]
	if num == "" {
		return 0, fmt.Errorf("empty short id")
	}
	var n int64
	for _, c := range num {
		if c < '0' || c > '9' {
			return 0, fmt.Errorf("invalid short id %q", s)
		}
		n = n*10 + int64(c-'0')
	}
	if n <= 0 {
		return 0, fmt.Errorf("invalid short id %q", s)
	}
	return n, nil
}

// scanMemberByID loads a team_members row by its BIGSERIAL id, taking a row lock.
func scanMemberByID(ctx context.Context, tx pgx.Tx, id int64) (domain.TeamMember, error) {
	var (
		m          domain.TeamMember
		teamID     uuid.UUID
		role       string
		status     string
		personaID  *uuid.UUID
		invitedBy  *int64
		invitedAt  *time.Time
		approvedAt *time.Time
		removedAt  *time.Time
		notifiedAt *time.Time
	)
	err := tx.QueryRow(ctx, `
SELECT id, team_id::text, user_id, role, status, persona_id::text,
       context_notified_at, invited_by, invited_at, approved_at, removed_at,
       joined_at, updated_at
FROM team_members WHERE id = $1 FOR UPDATE
`, id).Scan(&m.ID, &teamID, &m.UserID, &role, &status, &personaID,
		&notifiedAt, &invitedBy, &invitedAt, &approvedAt, &removedAt,
		&m.JoinedAt, &m.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.TeamMember{}, domain.ErrMemberNotFound
	}
	if err != nil {
		return domain.TeamMember{}, err
	}
	m.TeamID = teamID.String()
	m.Role = domain.TeamRole(role)
	m.Status = domain.TeamMemberStatus(status)
	m.ContextNotifiedAt = notifiedAt
	m.InvitedBy = invitedBy
	m.InvitedAt = invitedAt
	m.ApprovedAt = approvedAt
	m.RemovedAt = removedAt
	if personaID != nil {
		s := personaID.String()
		m.PersonaID = &s
	}
	return m, nil
}

// scanActiveMember loads a member by (team, user) without a row lock.
// Returns ErrMemberNotFound when no row exists.
func scanActiveMember(ctx context.Context, tx pgx.Tx, teamID uuid.UUID, userID int64) (domain.TeamMember, error) {
	var (
		m      domain.TeamMember
		id     int64
		role   string
		status string
	)
	err := tx.QueryRow(ctx, `
SELECT id, role, status FROM team_members
WHERE team_id = $1::uuid AND user_id = $2
`, teamID, userID).Scan(&id, &role, &status)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.TeamMember{}, domain.ErrMemberNotFound
	}
	if err != nil {
		return domain.TeamMember{}, err
	}
	m.ID = id
	m.TeamID = teamID.String()
	m.UserID = userID
	m.Role = domain.TeamRole(role)
	m.Status = domain.TeamMemberStatus(status)
	return m, nil
}

// upsertPendingMember inserts a new pending row or revives a removed row.
// Used by both invite-code submission and direct-invite creation.
func upsertPendingMember(ctx context.Context, tx pgx.Tx, teamID uuid.UUID, userID int64, status domain.TeamMemberStatus, invitedBy *int64) (domain.TeamMember, error) {
	now := time.Now().UTC()
	var id int64
	err := tx.QueryRow(ctx, `
INSERT INTO team_members (team_id, user_id, role, status, invited_by, invited_at)
VALUES ($1::uuid, $2, 'member', $3, $4, $5)
ON CONFLICT (team_id, user_id) DO UPDATE
SET status = $3, role = 'member', invited_by = $4, invited_at = $5,
    removed_at = NULL, approved_at = NULL, updated_at = $5
RETURNING id
`, teamID, userID, string(status), invitedBy, now).Scan(&id)
	if err != nil {
		return domain.TeamMember{}, fmt.Errorf("upsert member: %w", err)
	}
	return domain.TeamMember{
		ID:        id,
		TeamID:    teamID.String(),
		UserID:    userID,
		Role:      domain.RoleMember,
		Status:    status,
		InvitedBy: invitedBy,
		InvitedAt: &now,
		UpdatedAt: now,
	}, nil
}

// generateTeamInviteCode produces a random alphanumeric code.
// Uses crypto/rand and an unambiguous alphabet (no 0/O/1/I confusion).
func generateTeamInviteCode(length int) (string, error) {
	const alphabet = "23456789ABCDEFGHJKLMNPQRSTUVWXYZ"
	buf := make([]byte, length)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	for i := range buf {
		buf[i] = alphabet[int(buf[i])%len(alphabet)]
	}
	return string(buf), nil
}

// verifyTeamOwner loads a team's owner_user_id under a row lock.
// Returns ErrTeamNotFound when the team does not exist.
func verifyTeamOwner(ctx context.Context, tx pgx.Tx, teamID string) (int64, error) {
	var ownerID int64
	err := tx.QueryRow(ctx, `
SELECT owner_user_id FROM teams WHERE id = $1::uuid FOR UPDATE
`, teamID).Scan(&ownerID)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, domain.ErrTeamNotFound
	}
	if err != nil {
		return 0, err
	}
	return ownerID, nil
}

// verifyTeamOwnerNoLock loads a team's owner_user_id without a row lock.
// Used by read-only paths (list queries) that only need to authorize the caller.
func verifyTeamOwnerNoLock(ctx context.Context, pool pgxscan, teamID string) (int64, error) {
	var ownerID int64
	err := pool.QueryRow(ctx, `
SELECT owner_user_id FROM teams WHERE id = $1::uuid
`, teamID).Scan(&ownerID)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, domain.ErrTeamNotFound
	}
	if err != nil {
		return 0, err
	}
	return ownerID, nil
}

// pgxscan is the minimal QueryRow surface used by verifyTeamOwnerNoLock.
// Implemented by *pgxpool.Pool and pgx.Tx.
type pgxscan interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}
