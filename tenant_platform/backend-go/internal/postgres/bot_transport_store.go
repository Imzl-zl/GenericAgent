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
	var lastErrorCode *string
	err := s.pool.QueryRow(ctx, `
SELECT bot_id, update_cursor_ciphertext, update_cursor_key_version,
       reconnect_state, last_error_at, last_error_code, updated_at
FROM bot_transport_state WHERE bot_id = $1
`, botID).Scan(&st.BotID, &cursor, &st.UpdateCursorKeyVersion, &st.ReconnectState, &st.LastErrorAt, &lastErrorCode, &st.UpdatedAt)
	if err != nil {
		return domain.BotTransportState{}, err
	}
	st.UpdateCursorCiphertext = cursor
	if lastErrorCode != nil {
		st.LastErrorCode = *lastErrorCode
	}
	return st, nil
}

// UpsertBotTransportState inserts or updates the transport cursor and reconnect
// state for a bot. cursorCiphertext may be nil to keep the existing cursor.
// cursorKeyVersion is the AES key version used to encrypt cursorCiphertext; it
// is required when cursorCiphertext is non-nil so future key rotation can
// decrypt old cursors (mirrors bots.token_key_version). Pass 0 to preserve the
// existing version when only updating reconnect_state/error fields.
//
// CAS: when cursorCiphertext is non-nil, the update is guarded by
// `WHERE EXCLUDED.updated_at >= bot_transport_state.updated_at` to prevent a
// late-arriving webhook from overwriting a fresher cursor written by a newer
// poll cycle (iLink cursors are monotonic; older cursors must not win).
func (s *Store) UpsertBotTransportState(ctx context.Context, botID int64, cursorCiphertext []byte, cursorKeyVersion int, reconnectState, lastErrorCode string) error {
	if botID <= 0 {
		return fmt.Errorf("bot id must be positive")
	}
	if reconnectState == "" {
		reconnectState = "idle"
	}
	if cursorCiphertext != nil && cursorKeyVersion <= 0 {
		return fmt.Errorf("cursor key version must be positive when ciphertext is provided")
	}
	now := time.Now().UTC()
	_, err := s.pool.Exec(ctx, `
INSERT INTO bot_transport_state (bot_id, update_cursor_ciphertext, update_cursor_key_version,
                                 reconnect_state, last_error_code, last_error_at, updated_at)
VALUES ($1, $2, NULLIF($3, 0), $4, NULLIF($5, ''), NULL, $6)
ON CONFLICT (bot_id) DO UPDATE SET
    update_cursor_ciphertext = COALESCE(
        CASE
            WHEN EXCLUDED.update_cursor_ciphertext IS NULL
                THEN bot_transport_state.update_cursor_ciphertext
            WHEN $6 >= bot_transport_state.updated_at
                THEN EXCLUDED.update_cursor_ciphertext
            ELSE bot_transport_state.update_cursor_ciphertext
        END,
        bot_transport_state.update_cursor_ciphertext
    ),
    update_cursor_key_version = CASE
        WHEN EXCLUDED.update_cursor_ciphertext IS NOT NULL AND $6 >= bot_transport_state.updated_at
            THEN COALESCE(NULLIF($3, 0), bot_transport_state.update_cursor_key_version)
        ELSE bot_transport_state.update_cursor_key_version
    END,
    reconnect_state = EXCLUDED.reconnect_state,
    last_error_code = EXCLUDED.last_error_code,
    last_error_at = CASE WHEN EXCLUDED.last_error_code <> '' THEN $6 ELSE bot_transport_state.last_error_at END,
    updated_at = CASE
        WHEN EXCLUDED.update_cursor_ciphertext IS NOT NULL AND $6 < bot_transport_state.updated_at
            THEN bot_transport_state.updated_at
        ELSE $6
    END
`, botID, cursorCiphertext, cursorKeyVersion, reconnectState, lastErrorCode, now)
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
