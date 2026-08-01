-- 0036_channel_bindings.sql
-- 渠道账号 → canonical_user_id 绑定(spec §3: 用户身份)。
-- 同一渠道账号最多绑定一个 canonical 用户;一个用户可绑定多个渠道。
-- 团队路由使用 team:<team_id>,不在此表存储。

CREATE TABLE IF NOT EXISTS channel_bindings (
    id                 BIGSERIAL PRIMARY KEY,
    channel_type       TEXT NOT NULL CHECK (octet_length(channel_type) BETWEEN 1 AND 32 AND channel_type = btrim(channel_type)),
    channel_account_id TEXT NOT NULL CHECK (octet_length(channel_account_id) BETWEEN 1 AND 256),
    canonical_user_id  BIGINT NOT NULL REFERENCES users(id),
    created_at         TIMESTAMPTZ NOT NULL DEFAULT timezone('utc', now()),
    updated_at         TIMESTAMPTZ NOT NULL DEFAULT timezone('utc', now()),
    UNIQUE (channel_type, channel_account_id)
);

CREATE INDEX IF NOT EXISTS channel_bindings_user_idx ON channel_bindings (canonical_user_id);

CREATE TABLE IF NOT EXISTS migration_0036_channel_bindings_marker (
    id BOOLEAN PRIMARY KEY DEFAULT TRUE CHECK (id)
);
INSERT INTO migration_0036_channel_bindings_marker(id)
VALUES (TRUE)
ON CONFLICT (id) DO NOTHING;
