package postgres

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

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

// CheckInviteCode cheaply rejects invalid public registration attempts.
// The later transaction lock remains the authoritative consume check.
func (s *Store) CheckInviteCode(ctx context.Context, code string, now time.Time) error {
	if code == "" || now.IsZero() {
		return fmt.Errorf("code and current time are required")
	}
	var exists bool
	if err := s.pool.QueryRow(ctx, `
SELECT EXISTS (
    SELECT 1 FROM invite_codes
    WHERE code = $1 AND state = 'active' AND expires_at > $2
)
`, code, now).Scan(&exists); err != nil {
		return err
	}
	if !exists {
		return fmt.Errorf("invite code is invalid, used, expired, or revoked")
	}
	return nil
}

// CreateUserWithInvite atomically consumes an invite code and creates the user session.
func (s *Store) CreateUserWithInvite(
	ctx context.Context,
	username, passwordHash, code, tokenHash string,
	now, sessionExpiresAt time.Time,
) (domain.User, error) {
	if username == "" || passwordHash == "" || code == "" || tokenHash == "" {
		return domain.User{}, fmt.Errorf("username, password hash, invite code, and token hash are required")
	}
	if now.IsZero() || !sessionExpiresAt.After(now) {
		return domain.User{}, fmt.Errorf("valid registration and session expiry times are required")
	}

	var user domain.User
	err := s.withTx(ctx, func(tx pgx.Tx) error {
		var state domain.InviteCodeState
		var expiresAt time.Time
		if err := tx.QueryRow(ctx, `
SELECT state, expires_at
FROM invite_codes
WHERE code = $1
FOR UPDATE
`, code).Scan(&state, &expiresAt); err != nil {
			return fmt.Errorf("invalid invite code: %w", err)
		}
		if state != domain.InviteCodeActive || !now.Before(expiresAt) {
			return fmt.Errorf("invite code is used, expired, or revoked")
		}

		if err := scanUser(tx.QueryRow(ctx, `
INSERT INTO users (username, status, password_hash)
VALUES ($1, 'pending', $2)
RETURNING id, username, COALESCE(password_hash,''), status, COALESCE(bootstrap_marker,''), created_at, approved_at
`, username, passwordHash), &user); err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `
UPDATE invite_codes
SET state = 'used', used_by_user_id = $2, used_at = $3
WHERE code = $1 AND state = 'active'
`, code, user.ID, now)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return fmt.Errorf("invite code changed during registration")
		}
		_, err = tx.Exec(ctx, `
INSERT INTO user_sessions (token_hash, user_id, expires_at)
VALUES ($1, $2, $3)
`, tokenHash, user.ID, sessionExpiresAt)
		return err
	})
	return user, err
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

// DeleteInviteCodes permanently removes invite codes in one statement.
func (s *Store) DeleteInviteCodes(ctx context.Context, codes []string) (int64, error) {
	if len(codes) == 0 {
		return 0, fmt.Errorf("at least one invite code is required")
	}
	tag, err := s.pool.Exec(ctx, `
DELETE FROM invite_codes WHERE code = ANY($1::text[])
`, codes)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
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
