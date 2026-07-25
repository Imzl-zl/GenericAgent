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
// tokenCiphertext is the encrypted upstream bot token; plaintext is never stored.
func (s *Store) CreateBot(ctx context.Context, botUUID string, ownerID int64, tokenCiphertext []byte) (domain.Bot, error) {
	if _, err := uuid.Parse(botUUID); err != nil {
		return domain.Bot{}, fmt.Errorf("invalid bot_uuid: %w", err)
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
INSERT INTO bots (bot_uuid, owner_id, token_ciphertext, state)
VALUES ($1::uuid, $2, $3, 'active')
RETURNING id, bot_uuid, owner_id, ilink_user_id, token_ciphertext, token_key_version, state, created_at, updated_at
`, botUUID, ownerID, tokenCiphertext), &b)
	})
	return b, err
}

// GetBotByUUID returns the bot with the given bot_uuid.
func (s *Store) GetBotByUUID(ctx context.Context, botUUID string) (domain.Bot, error) {
	var b domain.Bot
	err := s.pool.QueryRow(ctx, `
SELECT id, bot_uuid, owner_id, ilink_user_id, token_ciphertext, token_key_version, state, created_at, updated_at
FROM bots WHERE bot_uuid = $1::uuid
`, botUUID).Scan(&b.ID, &b.BotUUID, &b.OwnerID, &b.IlinkUserID, &b.TokenCiphertext, &b.TokenKeyVersion, &b.State, &b.CreatedAt, &b.UpdatedAt)
	return b, err
}

// GetBotByOwner returns the bot owned by userID. Returns pgx.ErrNoRows if none.
func (s *Store) GetBotByOwner(ctx context.Context, ownerID int64) (domain.Bot, error) {
	var b domain.Bot
	err := s.pool.QueryRow(ctx, `
SELECT id, bot_uuid, owner_id, ilink_user_id, token_ciphertext, token_key_version, state, created_at, updated_at
FROM bots WHERE owner_id = $1
`, ownerID).Scan(&b.ID, &b.BotUUID, &b.OwnerID, &b.IlinkUserID, &b.TokenCiphertext, &b.TokenKeyVersion, &b.State, &b.CreatedAt, &b.UpdatedAt)
	return b, err
}

// GetBoundBotByIlinkUser returns the bot paired with the given ilink_user_id.
// Used by the Router to resolve an incoming message to a platform user.
func (s *Store) GetBoundBotByIlinkUser(ctx context.Context, ilinkUserID string) (domain.Bot, error) {
	if ilinkUserID == "" {
		return domain.Bot{}, fmt.Errorf("ilink user id is required")
	}
	var b domain.Bot
	err := s.pool.QueryRow(ctx, `
SELECT id, bot_uuid, owner_id, ilink_user_id, token_ciphertext, token_key_version, state, created_at, updated_at
FROM bots
WHERE ilink_user_id = $1 AND state = 'active'
`, ilinkUserID).Scan(&b.ID, &b.BotUUID, &b.OwnerID, &b.IlinkUserID, &b.TokenCiphertext, &b.TokenKeyVersion, &b.State, &b.CreatedAt, &b.UpdatedAt)
	return b, err
}

// BindBotIlinkUserTx pairs a bot with an ilink_user_id inside a transaction.
// Called by the binding consume flow so the binding state change and the bot
// pairing are atomic.
func (s *Store) BindBotIlinkUserTx(ctx context.Context, tx pgx.Tx, botUUID, ilinkUserID string) error {
	if botUUID == "" || ilinkUserID == "" {
		return fmt.Errorf("bot uuid and ilink user id are required")
	}
	tag, err := tx.Exec(ctx, `
UPDATE bots SET ilink_user_id = $2, updated_at = $3
WHERE bot_uuid = $1::uuid AND state = 'active'
`, botUUID, ilinkUserID, time.Now().UTC())
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("no active bot found for uuid %s", botUUID)
	}
	return nil
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
	return row.Scan(&b.ID, &b.BotUUID, &b.OwnerID, &b.IlinkUserID, &b.TokenCiphertext, &b.TokenKeyVersion, &b.State, &b.CreatedAt, &b.UpdatedAt)
}
