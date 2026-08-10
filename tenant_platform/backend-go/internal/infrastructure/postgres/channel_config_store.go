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

// CreateChannelConfig inserts a new wechat channel config owned by userID.
// One row per (owner_id, channel_type); ilinkBotID is the external iLink bot
// identifier supplied by the user. configCiphertext is the encrypted channel
// config JSON; plaintext is never stored. An internal UUID is generated for
// bot_uuid.
func (s *Store) CreateChannelConfig(ctx context.Context, ilinkBotID string, ownerID int64, configCiphertext []byte) (domain.ChannelConfig, error) {
	if ilinkBotID == "" {
		return domain.ChannelConfig{}, fmt.Errorf("ilink bot id is required")
	}
	if ownerID <= 0 {
		return domain.ChannelConfig{}, fmt.Errorf("owner id must be positive")
	}
	if len(configCiphertext) == 0 {
		return domain.ChannelConfig{}, fmt.Errorf("config ciphertext is required")
	}
	var c domain.ChannelConfig
	err := s.withTx(ctx, func(tx pgx.Tx) error {
		return scanChannelConfig(tx.QueryRow(ctx, `
INSERT INTO channel_configs (bot_uuid, channel_type, ilink_bot_id, owner_id, config_ciphertext, state)
VALUES ($1::uuid, 'wechat', $2, $3, $4, 'active')
RETURNING id, bot_uuid, channel_type, ilink_bot_id, owner_id, ilink_user_id, baseurl, config_ciphertext, config_key_version, state, created_at, updated_at
`, uuid.New().String(), ilinkBotID, ownerID, configCiphertext), &c)
	})
	return c, err
}

// CreateChannelConfigFromQRSession creates a fully-bound wechat channel config
// from a confirmed WechatQRSession. If a wechat config already exists for this
// owner_id, it updates the existing row with new credentials. This allows
// users to re-bind by scanning a new QR code without manual cleanup.
func (s *Store) CreateChannelConfigFromQRSession(ctx context.Context, sess domain.WechatQRSession, configKeyVersion int) (domain.ChannelConfig, error) {
	if sess.ID == "" {
		return domain.ChannelConfig{}, fmt.Errorf("session is required")
	}
	if sess.ILINKBotID == "" || sess.ILINKUserID == "" || len(sess.BotTokenCiphertext) == 0 {
		return domain.ChannelConfig{}, fmt.Errorf("session is not confirmed")
	}
	var c domain.ChannelConfig
	err := s.withTx(ctx, func(tx pgx.Tx) error {
		return scanChannelConfig(tx.QueryRow(ctx, `
INSERT INTO channel_configs (bot_uuid, channel_type, ilink_bot_id, owner_id, ilink_user_id, baseurl, config_ciphertext, config_key_version, state)
VALUES ($1::uuid, 'wechat', $2, $3, $4, $5, $6, $7, 'active')
ON CONFLICT (owner_id, channel_type)
DO UPDATE SET
  bot_uuid = EXCLUDED.bot_uuid,
  ilink_bot_id = EXCLUDED.ilink_bot_id,
  ilink_user_id = EXCLUDED.ilink_user_id,
  baseurl = EXCLUDED.baseurl,
  config_ciphertext = EXCLUDED.config_ciphertext,
  config_key_version = EXCLUDED.config_key_version,
  state = 'active',
  updated_at = timezone('utc', now())
RETURNING id, bot_uuid, channel_type, ilink_bot_id, owner_id, ilink_user_id, baseurl, config_ciphertext, config_key_version, state, created_at, updated_at
`, uuid.New().String(), sess.ILINKBotID, sess.UserID, sess.ILINKUserID, nullString(sess.BaseURL), sess.BotTokenCiphertext, configKeyVersion), &c)
	})
	return c, err
}

// UpsertChannelConfigCredentials saves or replaces the encrypted config JSON
// for (ownerID, channelType) and marks the config active. Used by the
// im-bindings PUT API for feishu/dingtalk/qq credential forms; wechat
// credentials come exclusively from the QR flow.
func (s *Store) UpsertChannelConfigCredentials(ctx context.Context, ownerID int64, channelType domain.ChannelType, configCiphertext []byte, configKeyVersion int) (domain.ChannelConfig, error) {
	if ownerID <= 0 {
		return domain.ChannelConfig{}, fmt.Errorf("owner id must be positive")
	}
	if !domain.IsValidChannelType(string(channelType)) || channelType == domain.ChannelWechat {
		return domain.ChannelConfig{}, fmt.Errorf("channel type %q cannot be configured via credentials", channelType)
	}
	if len(configCiphertext) == 0 {
		return domain.ChannelConfig{}, fmt.Errorf("config ciphertext is required")
	}
	var c domain.ChannelConfig
	err := s.withTx(ctx, func(tx pgx.Tx) error {
		return scanChannelConfig(tx.QueryRow(ctx, `
INSERT INTO channel_configs (bot_uuid, channel_type, owner_id, config_ciphertext, config_key_version, state)
VALUES ($1::uuid, $2, $3, $4, $5, 'active')
ON CONFLICT (owner_id, channel_type)
DO UPDATE SET
  config_ciphertext = EXCLUDED.config_ciphertext,
  config_key_version = EXCLUDED.config_key_version,
  state = 'active',
  updated_at = timezone('utc', now())
RETURNING id, bot_uuid, channel_type, ilink_bot_id, owner_id, ilink_user_id, baseurl, config_ciphertext, config_key_version, state, created_at, updated_at
`, uuid.New().String(), string(channelType), ownerID, configCiphertext, configKeyVersion), &c)
	})
	return c, err
}

// GetChannelConfigByUUID returns the config with the given bot_uuid.
// 非法 UUID 格式直接按"不存在"返回: 否则 $1::uuid 强转产生 22P02 永久性
// 错误, 会被 Router 当瞬态故障 5xx 重试, 永远无法收敛。
func (s *Store) GetChannelConfigByUUID(ctx context.Context, botUUID string) (domain.ChannelConfig, error) {
	if _, err := uuid.Parse(botUUID); err != nil {
		return domain.ChannelConfig{}, pgx.ErrNoRows
	}
	var c domain.ChannelConfig
	err := scanChannelConfig(s.pool.QueryRow(ctx, `
SELECT id, bot_uuid, channel_type, ilink_bot_id, owner_id, ilink_user_id, baseurl, config_ciphertext, config_key_version, state, created_at, updated_at
FROM channel_configs WHERE bot_uuid = $1::uuid
`, botUUID), &c)
	return c, err
}

// GetChannelConfigByIlinkBotID returns the config with the given external
// iLink bot id (wechat only).
func (s *Store) GetChannelConfigByIlinkBotID(ctx context.Context, ilinkBotID string) (domain.ChannelConfig, error) {
	if ilinkBotID == "" {
		return domain.ChannelConfig{}, fmt.Errorf("ilink bot id is required")
	}
	var c domain.ChannelConfig
	err := scanChannelConfig(s.pool.QueryRow(ctx, `
SELECT id, bot_uuid, channel_type, ilink_bot_id, owner_id, ilink_user_id, baseurl, config_ciphertext, config_key_version, state, created_at, updated_at
FROM channel_configs WHERE ilink_bot_id = $1
`, ilinkBotID), &c)
	return c, err
}

// GetChannelConfigByOwnerAndType returns the config owned by userID for the
// given channel type. Returns pgx.ErrNoRows if none.
func (s *Store) GetChannelConfigByOwnerAndType(ctx context.Context, ownerID int64, channelType domain.ChannelType) (domain.ChannelConfig, error) {
	if ownerID <= 0 {
		return domain.ChannelConfig{}, fmt.Errorf("owner id must be positive")
	}
	var c domain.ChannelConfig
	err := scanChannelConfig(s.pool.QueryRow(ctx, `
SELECT id, bot_uuid, channel_type, ilink_bot_id, owner_id, ilink_user_id, baseurl, config_ciphertext, config_key_version, state, created_at, updated_at
FROM channel_configs WHERE owner_id = $1 AND channel_type = $2
`, ownerID, string(channelType)), &c)
	return c, err
}

// GetChannelConfigsByOwner returns every channel config owned by userID
// (wechat + feishu/dingtalk/qq), newest first. Used by GET /v1/me/im-bindings.
func (s *Store) GetChannelConfigsByOwner(ctx context.Context, ownerID int64) ([]domain.ChannelConfig, error) {
	if ownerID <= 0 {
		return nil, fmt.Errorf("owner id must be positive")
	}
	rows, err := s.pool.Query(ctx, `
SELECT id, bot_uuid, channel_type, ilink_bot_id, owner_id, ilink_user_id, baseurl, config_ciphertext, config_key_version, state, created_at, updated_at
FROM channel_configs WHERE owner_id = $1
ORDER BY created_at DESC
`, ownerID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.ChannelConfig
	for rows.Next() {
		var c domain.ChannelConfig
		if err := scanChannelConfig(rows, &c); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// GetBoundChannelConfigByIlinkUser returns the active wechat config paired
// with the given ilink_user_id. Used by the Router to resolve an incoming
// message to a platform user.
func (s *Store) GetBoundChannelConfigByIlinkUser(ctx context.Context, ilinkUserID string) (domain.ChannelConfig, error) {
	if ilinkUserID == "" {
		return domain.ChannelConfig{}, fmt.Errorf("ilink user id is required")
	}
	var c domain.ChannelConfig
	err := scanChannelConfig(s.pool.QueryRow(ctx, `
SELECT id, bot_uuid, channel_type, ilink_bot_id, owner_id, ilink_user_id, baseurl, config_ciphertext, config_key_version, state, created_at, updated_at
FROM channel_configs
WHERE ilink_user_id = $1 AND channel_type = 'wechat' AND state = 'active'
`, ilinkUserID), &c)
	return c, err
}

// UpdateChannelConfigState transitions a channel config's state.
func (s *Store) UpdateChannelConfigState(ctx context.Context, botUUID string, state domain.ChannelConfigState) error {
	tag, err := s.pool.Exec(ctx, `
UPDATE channel_configs SET state = $2, updated_at = $3
WHERE bot_uuid = $1::uuid
`, botUUID, string(state), time.Now().UTC())
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("channel config %s not found", botUUID)
	}
	return nil
}

// DisableChannelConfig unbinds (ownerID, channelType): state=disabled, used by
// DELETE /v1/me/im-bindings/{channel_type}. Returns pgx.ErrNoRows if the
// config does not exist.
func (s *Store) DisableChannelConfig(ctx context.Context, ownerID int64, channelType domain.ChannelType) error {
	if ownerID <= 0 {
		return fmt.Errorf("owner id must be positive")
	}
	tag, err := s.pool.Exec(ctx, `
UPDATE channel_configs SET state = 'disabled', updated_at = $3
WHERE owner_id = $1 AND channel_type = $2 AND state <> 'disabled'
`, ownerID, string(channelType), time.Now().UTC())
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		var exists bool
		if err := s.pool.QueryRow(ctx, `
SELECT EXISTS (SELECT 1 FROM channel_configs WHERE owner_id = $1 AND channel_type = $2)
`, ownerID, string(channelType)).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			return pgx.ErrNoRows
		}
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

func scanChannelConfig(row pgx.Row, c *domain.ChannelConfig) error {
	var ilinkBotID, ilinkUserID, baseurl *string
	err := row.Scan(&c.ID, &c.BotUUID, &c.ChannelType, &ilinkBotID, &c.OwnerID, &ilinkUserID, &baseurl, &c.ConfigCiphertext, &c.ConfigKeyVersion, &c.State, &c.CreatedAt, &c.UpdatedAt)
	if err != nil {
		return err
	}
	if ilinkBotID != nil {
		c.IlinkBotID = *ilinkBotID
	}
	if ilinkUserID != nil {
		c.IlinkUserID = *ilinkUserID
	}
	if baseurl != nil {
		c.BaseURL = *baseurl
	}
	return nil
}
