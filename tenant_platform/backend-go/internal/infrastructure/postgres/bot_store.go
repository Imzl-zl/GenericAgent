package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/domain"
)

// CreateBot inserts a new bot owned by userID. owner_id is unique (one bot per user).
// ilinkBotID is the external iLink bot identifier supplied by the user.
// tokenCiphertext is the encrypted upstream bot token; plaintext is never stored.
// An internal UUID is generated for bot_uuid.
func (s *Store) CreateBot(ctx context.Context, ilinkBotID string, ownerID int64, tokenCiphertext []byte) (domain.Bot, error) {
	if ilinkBotID == "" {
		return domain.Bot{}, fmt.Errorf("ilink bot id is required")
	}
	if ownerID <= 0 {
		return domain.Bot{}, fmt.Errorf("owner id must be positive")
	}
	if len(tokenCiphertext) == 0 {
		return domain.Bot{}, fmt.Errorf("token ciphertext is required")
	}
	var b domain.Bot
	err := s.withTx(ctx, func(tx pgx.Tx) error {
		return scanBot(tx.QueryRow(ctx, `
INSERT INTO bots (bot_uuid, ilink_bot_id, owner_id, token_ciphertext, state)
VALUES ($1::uuid, $2, $3, $4, 'active')
RETURNING id, bot_uuid, ilink_bot_id, owner_id, ilink_user_id, baseurl, token_ciphertext, token_key_version, state, created_at, updated_at
`, uuid.New().String(), ilinkBotID, ownerID, tokenCiphertext), &b)
	})
	return b, err
}

// CreateBotFromQRSession creates a fully-bound bot from a confirmed WechatQRSession.
// If a bot already exists for this owner_id, it updates the existing bot with new credentials.
// This allows users to re-bind by scanning a new QR code without manual cleanup.
func (s *Store) CreateBotFromQRSession(ctx context.Context, sess domain.WechatQRSession, tokenKeyVersion int) (domain.Bot, error) {
	if sess.ID == "" {
		return domain.Bot{}, fmt.Errorf("session is required")
	}
	if sess.ILINKBotID == "" || sess.ILINKUserID == "" || len(sess.BotTokenCiphertext) == 0 {
		return domain.Bot{}, fmt.Errorf("session is not confirmed")
	}
	var b domain.Bot
	err := s.withTx(ctx, func(tx pgx.Tx) error {
		return scanBot(tx.QueryRow(ctx, `
INSERT INTO bots (bot_uuid, ilink_bot_id, owner_id, ilink_user_id, baseurl, token_ciphertext, token_key_version, state)
VALUES ($1::uuid, $2, $3, $4, $5, $6, $7, 'active')
ON CONFLICT (owner_id)
DO UPDATE SET
  bot_uuid = EXCLUDED.bot_uuid,
  ilink_bot_id = EXCLUDED.ilink_bot_id,
  ilink_user_id = EXCLUDED.ilink_user_id,
  baseurl = EXCLUDED.baseurl,
  token_ciphertext = EXCLUDED.token_ciphertext,
  token_key_version = EXCLUDED.token_key_version,
  state = 'active',
  updated_at = timezone('utc', now())
RETURNING id, bot_uuid, ilink_bot_id, owner_id, ilink_user_id, baseurl, token_ciphertext, token_key_version, state, created_at, updated_at
`, uuid.New().String(), sess.ILINKBotID, sess.UserID, sess.ILINKUserID, nullString(sess.BaseURL), sess.BotTokenCiphertext, tokenKeyVersion), &b)
	})
	return b, err
}

// GetBotByUUID returns the bot with the given bot_uuid.
func (s *Store) GetBotByUUID(ctx context.Context, botUUID string) (domain.Bot, error) {
	var b domain.Bot
	err := scanBot(s.pool.QueryRow(ctx, `
SELECT id, bot_uuid, ilink_bot_id, owner_id, ilink_user_id, baseurl, token_ciphertext, token_key_version, state, created_at, updated_at
FROM bots WHERE bot_uuid = $1::uuid
`, botUUID), &b)
	return b, err
}

// GetBotByIlinkBotID returns the bot with the given external iLink bot id.
func (s *Store) GetBotByIlinkBotID(ctx context.Context, ilinkBotID string) (domain.Bot, error) {
	if ilinkBotID == "" {
		return domain.Bot{}, fmt.Errorf("ilink bot id is required")
	}
	var b domain.Bot
	err := scanBot(s.pool.QueryRow(ctx, `
SELECT id, bot_uuid, ilink_bot_id, owner_id, ilink_user_id, baseurl, token_ciphertext, token_key_version, state, created_at, updated_at
FROM bots WHERE ilink_bot_id = $1
`, ilinkBotID), &b)
	return b, err
}

// GetBotByOwner returns the bot owned by userID. Returns pgx.ErrNoRows if none.
func (s *Store) GetBotByOwner(ctx context.Context, ownerID int64) (domain.Bot, error) {
	var b domain.Bot
	err := scanBot(s.pool.QueryRow(ctx, `
SELECT id, bot_uuid, ilink_bot_id, owner_id, ilink_user_id, baseurl, token_ciphertext, token_key_version, state, created_at, updated_at
FROM bots WHERE owner_id = $1
`, ownerID), &b)
	return b, err
}

// GetBoundBotByIlinkUser returns the bot paired with the given ilink_user_id.
// Used by the Router to resolve an incoming message to a platform user.
func (s *Store) GetBoundBotByIlinkUser(ctx context.Context, ilinkUserID string) (domain.Bot, error) {
	if ilinkUserID == "" {
		return domain.Bot{}, fmt.Errorf("ilink user id is required")
	}
	var b domain.Bot
	err := scanBot(s.pool.QueryRow(ctx, `
SELECT id, bot_uuid, ilink_bot_id, owner_id, ilink_user_id, baseurl, token_ciphertext, token_key_version, state, created_at, updated_at
FROM bots
WHERE ilink_user_id = $1 AND state = 'active'
`, ilinkUserID), &b)
	return b, err
}


// UpdateBotState transitions a bot's state.
func (s *Store) UpdateBotState(ctx context.Context, botUUID string, state domain.BotState) error {
	tag, err := s.pool.Exec(ctx, `
UPDATE bots SET state = $2, updated_at = $3
WHERE bot_uuid = $1::uuid
`, botUUID, state, time.Now().UTC())
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("bot %s not found", botUUID)
	}
	return nil
}

// GetUserStatus returns the status of the user with the given ID.
func (s *Store) GetUserStatus(ctx context.Context, userID int64) (domain.UserStatus, error) {
	if userID <= 0 {
		return "", fmt.Errorf("user id must be positive")
	}
	var status string
	err := s.pool.QueryRow(ctx, `SELECT status FROM users WHERE id = $1`, userID).Scan(&status)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", fmt.Errorf("user %d not found", userID)
	}
	return domain.UserStatus(status), err
}

// GetUserByID returns the user's username and status.
func (s *Store) GetUserByID(ctx context.Context, userID int64) (id int64, username string, status domain.UserStatus, err error) {
	if userID <= 0 {
		return 0, "", "", fmt.Errorf("user id must be positive")
	}
	var rawStatus string
	err = s.pool.QueryRow(ctx, `SELECT id, username, status FROM users WHERE id = $1`, userID).Scan(&id, &username, &rawStatus)
	status = domain.UserStatus(rawStatus)
	return id, username, status, err
}

func scanBot(row pgx.Row, b *domain.Bot) error {
	var ilinkBotID, ilinkUserID, baseurl *string
	err := row.Scan(&b.ID, &b.BotUUID, &ilinkBotID, &b.OwnerID, &ilinkUserID, &baseurl, &b.TokenCiphertext, &b.TokenKeyVersion, &b.State, &b.CreatedAt, &b.UpdatedAt)
	if err != nil {
		return err
	}
	if ilinkBotID != nil {
		b.IlinkBotID = *ilinkBotID
	}
	if ilinkUserID != nil {
		b.IlinkUserID = *ilinkUserID
	}
	if baseurl != nil {
		b.BaseURL = *baseurl
	}
	return nil
}
