-- 0014: media_assets table
-- Stores metadata for inbound and outbound media files (images, videos,
-- files, voice). Binary content stays on the file system under
-- {media_dir}/{bot_uuid}/{YYYY-MM}/{file_id}.{ext}; this table is the
-- control plane (metadata index) only.
--
-- Design (industry pattern: separate metadata plane from data plane):
--   * storage_path is RELATIVE to media_dir so the same row works when
--     media_dir is re-pointed (local disk -> NFS -> S3 mount). Multi-instance
--     deployments only need to mount the same volume.
--   * UNIQUE(message_id, storage_path) is the cross-instance idempotency
--     backstop: when the in-memory `seen` map in the Poller / ILinkAdapter
--     is cold (restart) or split across instances, the DB constraint rejects
--     duplicate INSERTs for the same (message, file) pair.
--   * message_id is a soft reference (no FK): deleting a message should not
--     cascade-delete the media audit row. Media cleanup is a separate
--     lifecycle concern (P1: retention job).
--   * sha256 is optional and left NULL in P0; computing it for 100MB videos
--     on the hot path is too costly. It can be backfilled by a batch job
--     later for dedup / integrity checks.

CREATE TABLE media_assets (
    id              BIGSERIAL PRIMARY KEY,
    user_id         BIGINT NOT NULL REFERENCES users (id),
    bot_id          BIGINT NOT NULL REFERENCES bots (id),
    message_id      BIGINT,
    file_name       TEXT NOT NULL,
    storage_path    TEXT NOT NULL,
    content_type    TEXT NOT NULL,
    size_bytes      BIGINT NOT NULL,
    sha256          TEXT,
    direction       TEXT NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT (timezone('utc', now())),
    CONSTRAINT media_direction_check CHECK (direction IN ('inbound', 'outbound')),
    CONSTRAINT media_storage_path_nonempty CHECK (char_length(storage_path) > 0),
    CONSTRAINT media_size_nonneg CHECK (size_bytes >= 0)
);

CREATE INDEX media_user_created_idx ON media_assets (user_id, created_at DESC);
CREATE INDEX media_message_idx ON media_assets (message_id);
CREATE INDEX media_bot_created_idx ON media_assets (bot_id, created_at DESC);

-- Dedup: same file from the same message cannot be inserted twice. This is
-- the multi-instance idempotency backstop paired with messages_inbound_dedup_uq.
CREATE UNIQUE INDEX media_message_path_uq
    ON media_assets (message_id, storage_path)
    WHERE message_id IS NOT NULL;

CREATE TABLE IF NOT EXISTS migration_0014_media_assets_marker (
    applied_at TIMESTAMPTZ NOT NULL DEFAULT (timezone('utc', now()))
);
