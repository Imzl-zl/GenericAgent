package postgres

import (
	"context"
	"fmt"
	"time"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/domain"
)

// CreateInviteCode inserts a new invite code.
func (s *Store) CreateInviteCode(ctx context.Context, code string, createdByUserID int64, expiresAt time.Time) (domain.InviteCode, error) {
	if code == "" {
		return domain.InviteCode{}, fmt.Errorf("code is required")
	}
	if createdByUserID <= 0 {
		return domain.InviteCode{}, fmt.Errorf("created by user id must be positive")
	}
	if expiresAt.IsZero() {
		return domain.InviteCode{}, fmt.Errorf("expires at is required")
	}
	var ic domain.InviteCode
	err := s.pool.QueryRow(ctx, `
INSERT INTO invite_codes (code, created_by_user_id, expires_at, state)
VALUES ($1, $2, $3, 'active')
RETURNING code, created_by_user_id, used_by_user_id, used_at, expires_at, state, created_at
`, code, createdByUserID, expiresAt).Scan(
		&ic.Code, &ic.CreatedByUserID, &ic.UsedByUserID, &ic.UsedAt, &ic.ExpiresAt, &ic.State, &ic.CreatedAt,
	)
	return ic, err
}

// GetInviteCode returns an invite code by its plaintext code.
func (s *Store) GetInviteCode(ctx context.Context, code string) (domain.InviteCode, error) {
	var ic domain.InviteCode
	err := s.pool.QueryRow(ctx, `
SELECT code, created_by_user_id, used_by_user_id, used_at, expires_at, state, created_at
FROM invite_codes WHERE code = $1
`, code).Scan(
		&ic.Code, &ic.CreatedByUserID, &ic.UsedByUserID, &ic.UsedAt, &ic.ExpiresAt, &ic.State, &ic.CreatedAt,
	)
	return ic, err
}

// ConsumeInviteCode marks an invite code as used by the given user.
func (s *Store) ConsumeInviteCode(ctx context.Context, code string, usedByUserID int64, now time.Time) error {
	if code == "" || usedByUserID <= 0 {
		return fmt.Errorf("code and used by user id are required")
	}
	tag, err := s.pool.Exec(ctx, `
UPDATE invite_codes
SET state = 'used', used_by_user_id = $2, used_at = $3
WHERE code = $1 AND state = 'active' AND expires_at > $3
`, code, usedByUserID, now)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("invite code %s is invalid, used, expired, or revoked", code)
	}
	return nil
}

// RevokeInviteCode revokes an unused invite code.
func (s *Store) RevokeInviteCode(ctx context.Context, code string) error {
	if code == "" {
		return fmt.Errorf("code is required")
	}
	tag, err := s.pool.Exec(ctx, `
UPDATE invite_codes SET state = 'revoked' WHERE code = $1 AND state = 'active'
`, code)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("invite code %s not found or not active", code)
	}
	return nil
}

// ListInviteCodes returns invite codes ordered by creation time.
func (s *Store) ListInviteCodes(ctx context.Context) ([]domain.InviteCode, error) {
	rows, err := s.pool.Query(ctx, `
SELECT code, created_by_user_id, used_by_user_id, used_at, expires_at, state, created_at
FROM invite_codes ORDER BY created_at DESC
`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.InviteCode
	for rows.Next() {
		var ic domain.InviteCode
		if err := rows.Scan(
			&ic.Code, &ic.CreatedByUserID, &ic.UsedByUserID, &ic.UsedAt, &ic.ExpiresAt, &ic.State, &ic.CreatedAt,
		); err != nil {
			return nil, err
		}
		out = append(out, ic)
	}
	return out, rows.Err()
}

// CreateUserSession inserts a bearer session token hash.
func (s *Store) CreateUserSession(ctx context.Context, tokenHash string, userID int64, expiresAt time.Time) (domain.UserSession, error) {
	if tokenHash == "" || userID <= 0 || expiresAt.IsZero() {
		return domain.UserSession{}, fmt.Errorf("token hash, user id, and expires at are required")
	}
	var sess domain.UserSession
	err := s.pool.QueryRow(ctx, `
INSERT INTO user_sessions (token_hash, user_id, expires_at)
VALUES ($1, $2, $3)
RETURNING token_hash, user_id, expires_at, created_at
`, tokenHash, userID, expiresAt).Scan(&sess.TokenHash, &sess.UserID, &sess.ExpiresAt, &sess.CreatedAt)
	return sess, err
}

// GetUserSession returns the session if it exists and has not expired.
func (s *Store) GetUserSession(ctx context.Context, tokenHash string) (domain.UserSession, error) {
	var sess domain.UserSession
	err := s.pool.QueryRow(ctx, `
SELECT token_hash, user_id, expires_at, created_at
FROM user_sessions
WHERE token_hash = $1 AND expires_at > timezone('utc', now())
`, tokenHash).Scan(&sess.TokenHash, &sess.UserID, &sess.ExpiresAt, &sess.CreatedAt)
	return sess, err
}

// DeleteUserSession removes a session.
func (s *Store) DeleteUserSession(ctx context.Context, tokenHash string) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM user_sessions WHERE token_hash = $1`, tokenHash)
	return err
}
