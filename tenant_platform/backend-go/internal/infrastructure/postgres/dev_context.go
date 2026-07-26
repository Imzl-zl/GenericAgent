package postgres

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// EnsureDevelopmentContext inserts or refreshes only bootstrap_marker='dev-loopback' rows.
// If the requested ID exists with another marker or non-approved status, it fails visibly.
func (s *Store) EnsureDevelopmentContext(ctx context.Context, userID int64, username string) (DevelopmentContext, error) {
	if userID <= 0 {
		return DevelopmentContext{}, fmt.Errorf("dev user id must be positive")
	}
	if strings.TrimSpace(username) == "" {
		username = fmt.Sprintf("dev-user-%d", userID)
	}
	sessionKey := fmt.Sprintf("personal:%d", userID)
	var out DevelopmentContext
	err := s.withTx(ctx, func(tx pgx.Tx) error {
		var existingID int64
		var status, marker *string
		err := tx.QueryRow(ctx, `
SELECT id, status, bootstrap_marker FROM users WHERE id = $1 FOR UPDATE
`, userID).Scan(&existingID, &status, &marker)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if err == nil {
			if status == nil || *status != "approved" {
				return fmt.Errorf("user %d exists with non-approved status; refusing bootstrap promotion", userID)
			}
			if marker == nil || *marker != "dev-loopback" {
				return fmt.Errorf("user %d exists without bootstrap_marker=dev-loopback; refusing bootstrap", userID)
			}
			if _, err := tx.Exec(ctx, `
UPDATE users SET username = $2, approved_at = COALESCE(approved_at, timezone('utc', now()))
WHERE id = $1 AND bootstrap_marker = 'dev-loopback'
`, userID, username); err != nil {
				return err
			}
		} else {
			if _, err := tx.Exec(ctx, `
INSERT INTO users (id, username, status, bootstrap_marker, approved_at)
VALUES ($1, $2, 'approved', 'dev-loopback', timezone('utc', now()))
`, userID, username); err != nil {
				return err
			}
		}

		var wsID uuid.UUID
		var wsMarker *string
		var vol *string
		err = tx.QueryRow(ctx, `
SELECT id, bootstrap_marker, volume_id FROM workspaces WHERE session_key = $1 FOR UPDATE
`, sessionKey).Scan(&wsID, &wsMarker, &vol)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if err == nil {
			if wsMarker == nil || *wsMarker != "dev-loopback" {
				return fmt.Errorf("workspace %s exists without bootstrap_marker=dev-loopback", sessionKey)
			}
			if _, err := tx.Exec(ctx, `
UPDATE workspaces SET owner_user_id = $2, kind = 'personal', team_id = NULL
WHERE id = $1 AND bootstrap_marker = 'dev-loopback'
`, wsID, userID); err != nil {
				return err
			}
		} else {
			wsID = uuid.New()
			if _, err := tx.Exec(ctx, `
INSERT INTO workspaces (id, session_key, owner_user_id, kind, team_id, volume_id, bootstrap_marker)
VALUES ($1, $2, $3, 'personal', NULL, NULL, 'dev-loopback')
`, wsID, sessionKey, userID); err != nil {
				return err
			}
		}
		out = DevelopmentContext{
			UserID:      userID,
			Username:    username,
			WorkspaceID: wsID.String(),
			SessionKey:  sessionKey,
		}
		return nil
	})
	return out, err
}
