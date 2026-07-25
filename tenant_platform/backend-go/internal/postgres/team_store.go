package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// TeamContext is the dev-bootstrap result for a team workspace.
type TeamContext struct {
	TeamID      string
	TeamName    string
	OwnerID     int64
	MemberIDs   []int64
	WorkspaceID string
	SessionKey  string
}

// EnsureTeamContext creates a dev-loopback team with owner + members and a
// dedicated team workspace. It is idempotent: re-running with the same
// (ownerID, teamName) reuses the existing team (even if the caller passes a
// fresh teamID), refreshes membership, and returns the existing workspace.
// This is the only team bootstrap path for the minimal-team slice.
func (s *Store) EnsureTeamContext(ctx context.Context, teamID uuid.UUID, teamName string, ownerID int64, memberIDs []int64) (TeamContext, error) {
	if ownerID <= 0 {
		return TeamContext{}, fmt.Errorf("owner user id must be positive")
	}
	if len(teamName) == 0 {
		return TeamContext{}, fmt.Errorf("team name is required")
	}
	var out TeamContext
	err := s.withTx(ctx, func(tx pgx.Tx) error {
		// Look up team by ID first; if not found, fall back to (owner, name)
		// so a restart that generates a fresh teamID reuses the existing row
		// instead of violating teams_owner_name_uq.
		var (
			actualTeamID  uuid.UUID
			existingOwner int64
			marker        *string
		)
		err := tx.QueryRow(ctx, `
SELECT id, owner_user_id, bootstrap_marker FROM teams WHERE id = $1 FOR UPDATE
`, teamID).Scan(&actualTeamID, &existingOwner, &marker)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if errors.Is(err, pgx.ErrNoRows) {
			err = tx.QueryRow(ctx, `
SELECT id, owner_user_id, bootstrap_marker FROM teams
WHERE owner_user_id = $1 AND name = $2 FOR UPDATE
`, ownerID, teamName).Scan(&actualTeamID, &existingOwner, &marker)
			if err != nil && !errors.Is(err, pgx.ErrNoRows) {
				return err
			}
		}
		if err == nil {
			if marker == nil || *marker != "dev-loopback" {
				return fmt.Errorf("team %s exists without bootstrap_marker=dev-loopback", actualTeamID)
			}
			if existingOwner != ownerID {
				return fmt.Errorf("team %s exists with different owner %d", actualTeamID, existingOwner)
			}
			// Name already matches (lookup was by owner+name) — no update needed.
		} else {
			actualTeamID = teamID
			if _, err := tx.Exec(ctx, `
INSERT INTO teams (id, name, owner_user_id, bootstrap_marker)
VALUES ($1, $2, $3, 'dev-loopback')
`, actualTeamID, teamName, ownerID); err != nil {
				return err
			}
		}
		sessionKey := fmt.Sprintf("team:%s", actualTeamID)

		// Refresh membership: owner first, then members. We delete and re-insert
		// to keep this idempotent and simple for the dev bootstrap path.
		if _, err := tx.Exec(ctx, `DELETE FROM team_members WHERE team_id = $1`, actualTeamID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
INSERT INTO team_members (team_id, user_id, role) VALUES ($1, $2, 'owner')
`, actualTeamID, ownerID); err != nil {
			return err
		}
		for _, m := range memberIDs {
			if m == ownerID || m <= 0 {
				continue
			}
			if _, err := tx.Exec(ctx, `
INSERT INTO team_members (team_id, user_id, role) VALUES ($1, $2, 'member')
ON CONFLICT (team_id, user_id) DO UPDATE SET role = 'member'
`, actualTeamID, m); err != nil {
				return err
			}
		}

		// Upsert team workspace.
		var wsID uuid.UUID
		var wsMarker *string
		err = tx.QueryRow(ctx, `
SELECT id, bootstrap_marker FROM workspaces WHERE session_key = $1 FOR UPDATE
`, sessionKey).Scan(&wsID, &wsMarker)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if err == nil {
			if wsMarker == nil || *wsMarker != "dev-loopback" {
				return fmt.Errorf("workspace %s exists without bootstrap_marker=dev-loopback", sessionKey)
			}
			if _, err := tx.Exec(ctx, `
UPDATE workspaces SET owner_user_id = $2, kind = 'team', team_id = $3::uuid
WHERE id = $1 AND bootstrap_marker = 'dev-loopback'
`, wsID, ownerID, actualTeamID); err != nil {
				return err
			}
		} else {
			wsID = uuid.New()
			if _, err := tx.Exec(ctx, `
INSERT INTO workspaces (id, session_key, owner_user_id, kind, team_id, volume_id, bootstrap_marker)
VALUES ($1, $2, $3, 'team', $4::uuid, NULL, 'dev-loopback')
`, wsID, sessionKey, ownerID, actualTeamID); err != nil {
				return err
			}
		}

		members := append([]int64(nil), memberIDs...)
		out = TeamContext{
			TeamID:      actualTeamID.String(),
			TeamName:    teamName,
			OwnerID:     ownerID,
			MemberIDs:   members,
			WorkspaceID: wsID.String(),
			SessionKey:  sessionKey,
		}
		return nil
	})
	return out, err
}

// authorizeSubmitter enforces that requester may submit to the workspace:
//   - personal workspace: requester must equal ownerID
//   - team workspace: requester must be owner or a team_member row
//
// Called inside the SubmitTask transaction so the workspace row lock is held.
func authorizeSubmitter(tx pgx.Tx, ctx context.Context, kind string, ownerID int64, teamID string, requester int64) error {
	switch kind {
	case "personal":
		if requester != ownerID {
			return fmt.Errorf("requester %d is not owner of personal session", requester)
		}
		return nil
	case "team":
		if teamID == "" {
			return fmt.Errorf("team workspace missing team_id")
		}
		if requester == ownerID {
			return nil
		}
		var count int
		if err := tx.QueryRow(ctx, `
SELECT COUNT(*) FROM team_members WHERE team_id = $1::uuid AND user_id = $2
`, teamID, requester).Scan(&count); err != nil {
			return err
		}
		if count == 0 {
			return fmt.Errorf("requester %d is not a member of team %s", requester, teamID)
		}
		return nil
	default:
		return fmt.Errorf("unknown workspace kind %q", kind)
	}
}

// IsTeamMember reports whether userID is a member of teamID. Used by the HTTP
// layer for cancel authorization on team tasks.
func (s *Store) IsTeamMember(ctx context.Context, teamID string, userID int64) (bool, error) {
	if userID <= 0 {
		return false, nil
	}
	var count int
	err := s.pool.QueryRow(ctx, `
SELECT COUNT(*) FROM team_members WHERE team_id = $1::uuid AND user_id = $2
`, teamID, userID).Scan(&count)
	if err != nil {
		return false, err
	}
	return count > 0, nil
}

// WorkspaceOwnerForSession returns the owner_user_id for a session_key. Used
// by the HTTP cancel path to authorize team task cancellation by the owner.
func (s *Store) WorkspaceOwnerForSession(ctx context.Context, sessionKey string) (int64, string, error) {
	var ownerID int64
	var kind string
	err := s.pool.QueryRow(ctx, `
SELECT owner_user_id, kind FROM workspaces WHERE session_key = $1
`, sessionKey).Scan(&ownerID, &kind)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, "", fmt.Errorf("workspace not found for session %s", sessionKey)
	}
	return ownerID, kind, err
}
