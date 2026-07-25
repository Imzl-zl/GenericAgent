-- Add wechat_qr_sessions and bot baseurl for official iLink QR login.
CREATE TABLE IF NOT EXISTS migration_0011_wechat_qr_session_marker ();

CREATE TABLE IF NOT EXISTS wechat_qr_sessions (
    id                  UUID PRIMARY KEY,
    user_id             BIGINT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    ilink_qrcode        TEXT NOT NULL,
    qrcode_img_url      TEXT NOT NULL,
    status              TEXT NOT NULL,
    ilink_bot_id        TEXT,
    ilink_user_id       TEXT,
    bot_token_ciphertext BYTEA,
    baseurl             TEXT,
    expires_at          TIMESTAMPTZ NOT NULL,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT (timezone('utc', now())),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT (timezone('utc', now())),
    CONSTRAINT wechat_qr_sessions_status_check CHECK (status IN ('wait', 'scaned', 'scaned_but_redirect', 'expired', 'confirmed'))
);

CREATE INDEX IF NOT EXISTS wechat_qr_sessions_user_idx ON wechat_qr_sessions (user_id);
CREATE INDEX IF NOT EXISTS wechat_qr_sessions_qrcode_idx ON wechat_qr_sessions (ilink_qrcode);

ALTER TABLE bots ADD COLUMN IF NOT EXISTS baseurl TEXT;
