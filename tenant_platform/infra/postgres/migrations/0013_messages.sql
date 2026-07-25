-- 0013: messages table
-- Stores every inbound and outbound WeChat message so the Web UI can render
-- conversation history and admins can audit cross-tenant traffic.
--
-- Design notes:
--   * Inbound messages are deduplicated by (bot_id, message_id) via a partial
--     UNIQUE index. This is the multi-instance idempotency backstop: when the
--     in-memory `seen` map in transport.ILinkAdapter is cold (restart) or
--     split across instances, the DB constraint rejects the duplicate INSERT.
--     Single-instance deployments keep the in-memory map as a fast path so
--     the hot polling loop does not pay a DB round-trip per message.
--   * Outbound messages have no iLink-assigned message_id, so they are exempt
--     from the dedup index (WHERE direction = 'inbound').
--   * task_id is intentionally NOT a foreign key: a message may arrive
--     before any task is created (rejected / command messages), and deleting
--     a task should not cascade-delete its audit trail.

CREATE TABLE messages (
    id           BIGSERIAL PRIMARY KEY,
    user_id      BIGINT NOT NULL REFERENCES users (id),
    bot_id       BIGINT NOT NULL REFERENCES bots (id),
    session_key  TEXT NOT NULL,
    direction    TEXT NOT NULL,
    message_id   TEXT,
    message_type TEXT NOT NULL DEFAULT 'text',
    content      TEXT,
    media_path   TEXT,
    task_id      TEXT,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT (timezone('utc', now())),
    CONSTRAINT messages_direction_check CHECK (direction IN ('inbound', 'outbound')),
    CONSTRAINT messages_message_type_check CHECK (message_type IN ('text', 'image', 'voice', 'file', 'video')),
    CONSTRAINT messages_session_nonempty CHECK (char_length(session_key) > 0)
);

CREATE INDEX messages_user_created_idx ON messages (user_id, created_at DESC);
CREATE INDEX messages_session_idx ON messages (session_key, created_at);
CREATE INDEX messages_bot_created_idx ON messages (bot_id, created_at DESC);

-- Partial unique index: dedup only inbound messages with a non-null iLink
-- message_id. Outbound messages are exempt (no natural id from iLink).
CREATE UNIQUE INDEX messages_inbound_dedup_uq
    ON messages (bot_id, message_id)
    WHERE direction = 'inbound' AND message_id IS NOT NULL;

-- Marker table so applyPendingMigrations skips this file on already-upgraded DBs.
CREATE TABLE IF NOT EXISTS migration_0013_messages_marker (
    applied_at TIMESTAMPTZ NOT NULL DEFAULT (timezone('utc', now()))
);
