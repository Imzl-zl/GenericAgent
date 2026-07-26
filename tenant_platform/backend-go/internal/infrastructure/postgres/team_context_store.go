package postgres

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/domain"
)

// GetActiveContext returns the user's current personal/team context.
// Returns a personal context (TeamID nil) when no active_contexts row exists.
func (s *Store) GetActiveContext(ctx context.Context, userID int64) (domain.ActiveContext, error) {
	var (
		teamID    *uuid.UUID
		updatedAt time.Time
	)
	err := s.pool.QueryRow(ctx, `
SELECT team_id, updated_at FROM active_contexts WHERE user_id = $1
`, userID).Scan(&teamID, &updatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.ActiveContext{UserID: userID}, nil
	}
	if err != nil {
		return domain.ActiveContext{}, err
	}
	ac := domain.ActiveContext{UserID: userID, UpdatedAt: updatedAt}
	if teamID != nil {
		s := teamID.String()
		ac.TeamID = &s
	}
	return ac, nil
}

// SetActiveContextPersonal switches the user to personal:{user_id}.
// Removes the active_contexts row (or sets team_id NULL) so subsequent
// messages route to the user's personal workspace.
func (s *Store) SetActiveContextPersonal(ctx context.Context, userID int64) (domain.ActiveContext, error) {
	now := time.Now().UTC()
	_, err := s.pool.Exec(ctx, `
INSERT INTO active_contexts (user_id, team_id, updated_at)
VALUES ($1, NULL, $2)
ON CONFLICT (user_id) DO UPDATE SET team_id = NULL, updated_at = $2
`, userID, now)
	if err != nil {
		return domain.ActiveContext{}, err
	}
	return domain.ActiveContext{UserID: userID, UpdatedAt: now}, nil
}

// SetActiveContextTeam switches the user to team:{teamID}. The user must be
// an approved member; otherwise ErrActiveContextBlocked is returned.
func (s *Store) SetActiveContextTeam(ctx context.Context, userID int64, teamID string) (domain.ActiveContext, error) {
	now := time.Now().UTC()
	var ac domain.ActiveContext
	err := s.withTx(ctx, func(tx pgx.Tx) error {
		var status string
		err := tx.QueryRow(ctx, `
SELECT status FROM team_members
WHERE team_id = $1::uuid AND user_id = $2
`, teamID, userID).Scan(&status)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrActiveContextBlocked
		}
		if err != nil {
			return err
		}
		if status != string(domain.MemberApproved) {
			return domain.ErrActiveContextBlocked
		}
		if _, err := tx.Exec(ctx, `
INSERT INTO active_contexts (user_id, team_id, updated_at)
VALUES ($1, $2::uuid, $3)
ON CONFLICT (user_id) DO UPDATE SET team_id = $2::uuid, updated_at = $3
`, userID, teamID, now); err != nil {
			return err
		}
		t := teamID
		ac = domain.ActiveContext{UserID: userID, TeamID: &t, UpdatedAt: now}
		return nil
	})
	return ac, err
}

// MarkContextNotified sets context_notified_at for a member, used by the
// one-shot privacy notice when the user first enters team context.
// Returns true when this is the first time (row was updated), false when
// the member was already notified (no row touched). This lets the caller
// decide whether to send the privacy notice.
func (s *Store) MarkContextNotified(ctx context.Context, userID int64, teamID string) (bool, error) {
	tag, err := s.pool.Exec(ctx, `
UPDATE team_members SET context_notified_at = $3, updated_at = $3
WHERE team_id = $1::uuid AND user_id = $2 AND context_notified_at IS NULL
`, teamID, userID, time.Now().UTC())
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

// ListUserTeams returns teams where the user is an approved member.
// Includes both teams the user owns and teams they belong to.
func (s *Store) ListUserTeams(ctx context.Context, userID int64) ([]domain.Team, error) {
	rows, err := s.pool.Query(ctx, `
SELECT t.id::text, t.name, t.owner_user_id, t.created_at
FROM teams t
JOIN team_members m ON m.team_id = t.id
WHERE m.user_id = $1 AND m.status = 'approved'
ORDER BY t.created_at DESC
`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var teams []domain.Team
	for rows.Next() {
		var t domain.Team
		if err := rows.Scan(&t.ID, &t.Name, &t.OwnerID, &t.CreatedAt); err != nil {
			return nil, err
		}
		teams = append(teams, t)
	}
	return teams, rows.Err()
}

// ListPendingMembers returns members awaiting owner action (pending_owner)
// or awaiting member action (pending_member) for a team. Only the owner may call.
func (s *Store) ListPendingMembers(ctx context.Context, teamID string, ownerID int64) ([]domain.TeamMember, error) {
	dbOwner, err := verifyTeamOwnerNoLock(ctx, s.pool, teamID)
	if err != nil {
		return nil, err
	}
	if dbOwner != ownerID {
		return nil, domain.ErrNotTeamOwner
	}
	rows, err := s.pool.Query(ctx, `
SELECT id, team_id::text, user_id, role, status, invited_by, invited_at
FROM team_members
WHERE team_id = $1::uuid AND status IN ('pending_member', 'pending_owner')
ORDER BY id DESC
`, teamID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var members []domain.TeamMember
	for rows.Next() {
		var m domain.TeamMember
		if err := rows.Scan(&m.ID, &m.TeamID, &m.UserID, &m.Role, &m.Status, &m.InvitedBy, &m.InvitedAt); err != nil {
			return nil, err
		}
		members = append(members, m)
	}
	return members, rows.Err()
}

// GetMemberByShortID loads a member by short-id without a row lock.
// Used by read paths (router display, service queries). Mutating paths use scanMemberByID.
func (s *Store) GetMemberByShortID(ctx context.Context, shortID string) (domain.TeamMember, error) {
	memberID, err := parseMemberShortID(shortID)
	if err != nil {
		return domain.TeamMember{}, domain.ErrMemberNotFound
	}
	var m domain.TeamMember
	var (
		teamID uuid.UUID
		role   string
		status string
	)
	err = s.pool.QueryRow(ctx, `
SELECT id, team_id::text, user_id, role, status
FROM team_members WHERE id = $1
`, memberID).Scan(&m.ID, &teamID, &m.UserID, &role, &status)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.TeamMember{}, domain.ErrMemberNotFound
	}
	if err != nil {
		return domain.TeamMember{}, err
	}
	m.TeamID = teamID.String()
	m.Role = domain.TeamRole(role)
	m.Status = domain.TeamMemberStatus(status)
	return m, nil
}
