-- Slice 3a: User lifecycle tables — binding_attempts, bots, bot_transport_state,
-- context_tokens, audit_events. Per architecture spec §5.
-- PostgreSQL is the sole fact source. No volatile now() checks in CHECK constraints.

CREATE TABLE binding_attempts (
    id              BIGSERIAL PRIMARY KEY,
    user_id         BIGINT NOT NULL REFERENCES users (id),
    code_hash       TEXT NOT NULL,
    state           TEXT NOT NULL,
    bot_uuid        UUID,
    expires_at      TIMESTAMPTZ NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT (timezone('utc', now())),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT (timezone('utc', now())),
    activated_at    TIMESTAMPTZ,
    CONSTRAINT binding_attempts_code_hash_nonempty CHECK (char_length(code_hash) > 0),
    CONSTRAINT binding_attempts_state_check CHECK (state IN (
        'requested', 'qr_pending', 'awaiting_activation', 'active', 'expired', 'revoked'
    )),
    CONSTRAINT binding_attempts_active_has_bot CHECK (
        state <> 'active' OR bot_uuid IS NOT NULL
    )
);

CREATE INDEX binding_attempts_user_idx ON binding_attempts (user_id);
CREATE INDEX binding_attempts_state_expires_idx ON binding_attempts (state, expires_at);
CREATE UNIQUE INDEX binding_attempts_code_hash_uq ON binding_attempts (code_hash)
    WHERE state IN ('requested', 'qr_pending', 'awaiting_activation');

CREATE TABLE bots (
    id                  BIGSERIAL PRIMARY KEY,
    bot_uuid            UUID NOT NULL,
    owner_id            BIGINT NOT NULL REFERENCES users (id),
    ilink_user_id       TEXT,
    token_ciphertext    BYTEA NOT NULL,
    token_key_version   INTEGER NOT NULL DEFAULT 1,
    state               TEXT NOT NULL,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT (timezone('utc', now())),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT (timezone('utc', now())),
    CONSTRAINT bots_state_check CHECK (state IN ('active', 'expired', 'revoked')),
    CONSTRAINT bots_token_key_version_pos CHECK (token_key_version > 0)
);

CREATE UNIQUE INDEX bots_bot_uuid_uq ON bots (bot_uuid);
CREATE UNIQUE INDEX bots_owner_uq ON bots (owner_id);
CREATE UNIQUE INDEX bots_ilink_user_uq ON bots (ilink_user_id)
    WHERE ilink_user_id IS NOT NULL;

CREATE TABLE bot_transport_state (
    bot_id                      BIGINT PRIMARY KEY REFERENCES bots (id) ON DELETE CASCADE,
    update_cursor_ciphertext    BYTEA,
    reconnect_state             TEXT NOT NULL DEFAULT 'idle',
    last_error_at               TIMESTAMPTZ,
    last_error_code             TEXT,
    updated_at                  TIMESTAMPTZ NOT NULL DEFAULT (timezone('utc', now())),
    CONSTRAINT bot_transport_state_reconnect_check CHECK (
        reconnect_state IN ('idle', 'polling', 'reconnecting', 'error', 'stopped')
    )
);

CREATE TABLE context_tokens (
    id                  BIGSERIAL PRIMARY KEY,
    bot_id              BIGINT NOT NULL REFERENCES bots (id) ON DELETE CASCADE,
    ilink_user_id       TEXT NOT NULL,
    token_ciphertext    BYTEA NOT NULL,
    expires_at          TIMESTAMPTZ NOT NULL,
    last_used_at        TIMESTAMPTZ,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT (timezone('utc', now())),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT (timezone('utc', now()))
);

CREATE UNIQUE INDEX context_tokens_bot_user_uq ON context_tokens (bot_id, ilink_user_id);
CREATE INDEX context_tokens_expires_idx ON context_tokens (expires_at);

CREATE TABLE audit_events (
    id              BIGSERIAL PRIMARY KEY,
    actor_user_id   BIGINT,
    action          TEXT NOT NULL,
    target_type     TEXT,
    target_id       TEXT,
    session_key     TEXT,
    detail          JSONB NOT NULL DEFAULT '{}'::jsonb,
    policy_version  TEXT,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT (timezone('utc', now())),
    CONSTRAINT audit_events_action_nonempty CHECK (char_length(action) > 0)
);

CREATE INDEX audit_events_actor_idx ON audit_events (actor_user_id);
CREATE INDEX audit_events_target_idx ON audit_events (target_type, target_id);
CREATE INDEX audit_events_action_idx ON audit_events (action);
