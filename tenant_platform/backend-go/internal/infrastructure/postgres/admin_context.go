package postgres

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// EnsureAdminContext inserts or refreshes only bootstrap_marker='dev-loopback' rows.
// If the requested ID exists with another marker or non-approved status, it fails visibly.
func (s *Store) EnsureAdminContext(ctx context.Context, userID int64, username string) (AdminContext, error) {
	return s.ensureContextUser(ctx, userID, username, "dev-loopback")
}

// EnsurePlatformAdminUser 是生产路径的管理员引导(方案 §7): 创建/刷新
// bootstrap_marker='platform-admin' 的 approved 用户与个人 workspace,
// 供管理端 API(persona/policy/invite)使用。与 dev-loopback 隔离,
// 不互相覆盖。
func (s *Store) EnsurePlatformAdminUser(ctx context.Context, userID int64, username string) (AdminContext, error) {
	return s.ensureContextUser(ctx, userID, username, "platform-admin")
}

func (s *Store) ensureContextUser(ctx context.Context, userID int64, username, bootstrapMarker string) (AdminContext, error) {
	if userID <= 0 {
		return AdminContext{}, fmt.Errorf("user id must be positive")
	}
	if strings.TrimSpace(username) == "" {
		username = fmt.Sprintf("ga-user-%d", userID)
	}
	sessionKey := fmt.Sprintf("personal:%d", userID)
	var out AdminContext
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
			if marker == nil || *marker != bootstrapMarker {
				return fmt.Errorf("user %d exists without bootstrap_marker=%s; refusing bootstrap", userID, bootstrapMarker)
			}
			if _, err := tx.Exec(ctx, `
UPDATE users SET username = $2, approved_at = COALESCE(approved_at, timezone('utc', now()))
WHERE id = $1 AND bootstrap_marker = $3
`, userID, username, bootstrapMarker); err != nil {
				return err
			}
		} else {
			if _, err := tx.Exec(ctx, `
INSERT INTO users (id, username, status, bootstrap_marker, approved_at)
VALUES ($1, $2, 'approved', $3, timezone('utc', now()))
`, userID, username, bootstrapMarker); err != nil {
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
			if wsMarker == nil || *wsMarker != bootstrapMarker {
				return fmt.Errorf("workspace %s exists without bootstrap_marker=%s", sessionKey, bootstrapMarker)
			}
			if _, err := tx.Exec(ctx, `
UPDATE workspaces SET owner_user_id = $2, kind = 'personal', team_id = NULL
WHERE id = $1 AND bootstrap_marker = $3
`, wsID, userID, bootstrapMarker); err != nil {
				return err
			}
		} else {
			// 统一走 insertPersonalWorkspaceTx(唯一实现): bootstrap 用户
			// volume_id 为 NULL(共享卷由 WorkspaceCoordinator 首调写入)。
			if _, err := insertPersonalWorkspaceTx(ctx, tx, userID, nil, &bootstrapMarker); err != nil {
				return err
			}
			wsID, err = s.WorkspaceIDByKeyTx(ctx, tx, sessionKey)
			if err != nil {
				return err
			}
		}
		out = AdminContext{
			UserID:      userID,
			Username:    username,
			WorkspaceID: wsID.String(),
			SessionKey:  sessionKey,
		}
		return nil
	})
	return out, err
}

// WorkspaceKeyByID 返回 workspaces 表的 session_key(workspace UUID → key)。
// 供 WorkspaceCoordinator 将 checkpoint workspace 映射到共享卷 hash。
func (s *Store) WorkspaceKeyByID(ctx context.Context, workspaceID string) (string, error) {
	var sessionKey string
	err := s.pool.QueryRow(ctx, `SELECT session_key FROM workspaces WHERE id = $1::uuid`, workspaceID).Scan(&sessionKey)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", fmt.Errorf("workspace %s not found", workspaceID)
	}
	if err != nil {
		return "", err
	}
	return sessionKey, nil
}
