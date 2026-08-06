package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/domain"
)

// GetRelayRecipient resolves a username to the full relay target: user identity,
// relay opt-out flag, and bot binding. Returns domain.ErrRelayUserNotFound when
// no user matches the username.
//
// The query LEFT JOINs relay_preferences (defaulting to opt_out=FALSE when no
// row exists) and bots (only active bots). One round-trip per relay.
func (s *Store) GetRelayRecipient(ctx context.Context, username string) (domain.RelayRecipient, error) {
	if username == "" {
		return domain.RelayRecipient{}, fmt.Errorf("username is required")
	}
	var (
		r        domain.RelayRecipient
		botID    *int64
		botUUID  *string
		ilinkUsr *string
	)
	err := s.pool.QueryRow(ctx, `
SELECT u.id,
       u.username,
       u.status,
       COALESCE(rp.opt_out, FALSE),
       b.id,
       b.bot_uuid::text,
       b.ilink_user_id
FROM users u
LEFT JOIN relay_preferences rp ON rp.user_id = u.id
LEFT JOIN bots b ON b.owner_id = u.id AND b.state = 'active'
WHERE u.username = $1
`, username).Scan(&r.UserID, &r.Username, &r.Status, &r.OptOut, &botID, &botUUID, &ilinkUsr)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.RelayRecipient{}, domain.ErrRelayUserNotFound
		}
		return domain.RelayRecipient{}, fmt.Errorf("get relay recipient: %w", err)
	}
	if botID != nil {
		r.BotID = *botID
	}
	if botUUID != nil {
		r.BotUUID = *botUUID
	}
	if ilinkUsr != nil {
		r.IlinkUserID = *ilinkUsr
	}
	return r, nil
}

// SetRelayOptOut upserts the user's relay opt-out preference. When optOut is
// true the user will not receive @username relay messages; false re-enables.
func (s *Store) SetRelayOptOut(ctx context.Context, userID int64, optOut bool) error {
	if userID <= 0 {
		return fmt.Errorf("user id must be positive")
	}
	_, err := s.pool.Exec(ctx, `
INSERT INTO relay_preferences (user_id, opt_out, updated_at)
VALUES ($1, $2, $3)
ON CONFLICT (user_id) DO UPDATE
SET opt_out = EXCLUDED.opt_out,
    updated_at = EXCLUDED.updated_at
`, userID, optOut, time.Now().UTC())
	if err != nil {
		return fmt.Errorf("set relay opt out: %w", err)
	}
	return nil
}

