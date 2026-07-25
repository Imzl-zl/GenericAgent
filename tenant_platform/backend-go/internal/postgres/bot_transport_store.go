package postgres

import (
	"context"
	"fmt"
	"time"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/domain"
)

// GetBotTransportState returns the persisted transport cursor for a bot.
// Returns pgx.ErrNoRows when no row exists (first start or after purge).
func (s *Store) GetBotTransportState(ctx context.Context, botID int64) (domain.BotTransportState, error) {
	if botID <= 0 {
		return domain.BotTransportState{}, fmt.Errorf("bot id must be positive")
	}
	var st domain.BotTransportState
	var cursor []byte
	err := s.pool.QueryRow(ctx, `
SELECT bot_id, update_cursor_ciphertext, reconnect_state, last_error_at, last_error_code, updated_at
FROM bot_transport_state WHERE bot_id = $1
`, botID).Scan(&st.BotID, &cursor, &st.ReconnectState, &st.LastErrorAt, &st.LastErrorCode, &st.UpdatedAt)
	if err != nil {
		return domain.BotTransportState{}, err
	}
	st.UpdateCursorCiphertext = cursor
	return st, nil
}

// UpsertBotTransportState inserts or updates the transport cursor and reconnect
// state for a bot. cursorCiphertext may be nil to keep the existing cursor.
func (s *Store) UpsertBotTransportState(ctx context.Context, botID int64, cursorCiphertext []byte, reconnectState, lastErrorCode string) error {
	if botID <= 0 {
		return fmt.Errorf("bot id must be positive")
	}
	if reconnectState == "" {
		reconnectState = "idle"
	}
	now := time.Now().UTC()
	_, err := s.pool.Exec(ctx, `
INSERT INTO bot_transport_state (bot_id, update_cursor_ciphertext, reconnect_state, last_error_code, last_error_at, updated_at)
VALUES ($1, $2, $3, NULLIF($4, ''), NULL, $5)
ON CONFLICT (bot_id) DO UPDATE SET
    update_cursor_ciphertext = COALESCE(EXCLUDED.update_cursor_ciphertext, bot_transport_state.update_cursor_ciphertext),
    reconnect_state = EXCLUDED.reconnect_state,
    last_error_code = EXCLUDED.last_error_code,
    last_error_at = CASE WHEN EXCLUDED.last_error_code <> '' THEN $5 ELSE bot_transport_state.last_error_at END,
    updated_at = $5
`, botID, cursorCiphertext, reconnectState, lastErrorCode, now)
	return err
}

// ListActiveBoundBots returns all bots in 'active' state that have a bound
// ilink_user_id. Used by RestoreActiveBots on platform startup to re-register
// each bot with the Bot Poller.
func (s *Store) ListActiveBoundBots(ctx context.Context) ([]domain.Bot, error) {
	rows, err := s.pool.Query(ctx, `
SELECT id, bot_uuid, ilink_bot_id, owner_id, ilink_user_id, baseurl, token_ciphertext, token_key_version, state, created_at, updated_at
FROM bots
WHERE state = 'active' AND ilink_user_id IS NOT NULL
ORDER BY created_at
`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var bots []domain.Bot
	for rows.Next() {
		var b domain.Bot
		if err := scanBot(rows, &b); err != nil {
			return nil, err
		}
		bots = append(bots, b)
	}
	return bots, rows.Err()
}
