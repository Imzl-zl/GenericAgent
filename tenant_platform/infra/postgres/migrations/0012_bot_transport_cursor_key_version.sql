-- 0012: bot_transport_state cursor key_version
-- Tracks the AES key version used to encrypt update_cursor_ciphertext so future
-- key rotation can decrypt old cursors. Mirrors bots.token_key_version.
-- Without this column, ResolveCursor would have to hardcode version=1 and
-- break the moment BOT_TOKEN_KEY is rotated.

ALTER TABLE bot_transport_state
    ADD COLUMN IF NOT EXISTS update_cursor_key_version INTEGER NOT NULL DEFAULT 1;

ALTER TABLE bot_transport_state
    ADD CONSTRAINT bot_transport_state_cursor_key_version_pos
        CHECK (update_cursor_key_version > 0);

-- Marker table so applyPendingMigrations skips this file on already-upgraded DBs.
CREATE TABLE IF NOT EXISTS migration_0012_bot_transport_cursor_key_version_marker (
    applied_at TIMESTAMPTZ NOT NULL DEFAULT (timezone('utc', now()))
);
