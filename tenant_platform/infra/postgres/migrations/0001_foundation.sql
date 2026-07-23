-- Foundation vertical path: users, workspaces, tasks, events, deliveries, snapshots.
-- PostgreSQL is the sole fact source. No volatile now() checks in CHECK constraints.

CREATE TABLE users (
    id              BIGINT PRIMARY KEY,
    username        TEXT NOT NULL,
    status          TEXT NOT NULL,
    bootstrap_marker TEXT,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT (timezone('utc', now())),
    approved_at     TIMESTAMPTZ,
    CONSTRAINT users_username_nonempty CHECK (char_length(username) > 0),
    CONSTRAINT users_status_check CHECK (status IN ('approved', 'pending', 'blocked')),
    CONSTRAINT users_bootstrap_marker_check CHECK (
        bootstrap_marker IS NULL OR bootstrap_marker = 'dev-loopback'
    )
);

CREATE UNIQUE INDEX users_username_uq ON users (username);
CREATE UNIQUE INDEX users_bootstrap_marker_uq ON users (bootstrap_marker)
    WHERE bootstrap_marker IS NOT NULL;

CREATE TABLE workspaces (
    id                  UUID PRIMARY KEY,
    session_key         TEXT NOT NULL,
    owner_user_id       BIGINT NOT NULL REFERENCES users (id),
    kind                TEXT NOT NULL,
    team_id             UUID,
    volume_id           TEXT,
    bootstrap_marker    TEXT,
    current_snapshot_id UUID,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT (timezone('utc', now())),
    CONSTRAINT workspaces_session_key_nonempty CHECK (char_length(session_key) > 0),
    CONSTRAINT workspaces_kind_check CHECK (kind = 'personal'),
    CONSTRAINT workspaces_personal_no_team CHECK (team_id IS NULL),
    CONSTRAINT workspaces_bootstrap_marker_check CHECK (
        bootstrap_marker IS NULL OR bootstrap_marker = 'dev-loopback'
    ),
    CONSTRAINT workspaces_null_volume_requires_loopback CHECK (
        volume_id IS NOT NULL OR bootstrap_marker = 'dev-loopback'
    )
);

CREATE UNIQUE INDEX workspaces_session_key_uq ON workspaces (session_key);
CREATE UNIQUE INDEX workspaces_bootstrap_marker_uq ON workspaces (bootstrap_marker)
    WHERE bootstrap_marker IS NOT NULL;

CREATE TABLE tasks (
    id                          TEXT PRIMARY KEY,
    workspace_id                UUID NOT NULL REFERENCES workspaces (id),
    session_key                 TEXT NOT NULL,
    session_sequence            BIGINT NOT NULL,
    requester_user_id           BIGINT NOT NULL REFERENCES users (id),
    source                      TEXT NOT NULL,
    source_instance_id          TEXT NOT NULL,
    message_id                  TEXT NOT NULL,
    message_idempotency_key     TEXT NOT NULL,
    prompt                      TEXT NOT NULL,
    persona_snapshot            JSONB NOT NULL DEFAULT '[]'::jsonb,
    tool_policy_version         TEXT NOT NULL,
    prompt_bytes                INTEGER NOT NULL,
    persona_bytes               INTEGER NOT NULL,
    status                      TEXT NOT NULL,
    claim_owner                 TEXT,
    claimed_at                  TIMESTAMPTZ,
    claim_lease_until           TIMESTAMPTZ,
    worker_instance_id          TEXT,
    worker_dispatch_started_at  TIMESTAMPTZ,
    cancel_requested_at         TIMESTAMPTZ,
    snapshot_id                 UUID,
    snapshot_checksum           TEXT,
    result_ref                  TEXT,
    result_digest               TEXT,
    terminal_error_code         TEXT,
    terminal_error_message      TEXT,
    terminal_error_trace_id     TEXT,
    created_at                  TIMESTAMPTZ NOT NULL DEFAULT (timezone('utc', now())),
    updated_at                  TIMESTAMPTZ NOT NULL DEFAULT (timezone('utc', now())),
    started_at                  TIMESTAMPTZ,
    succeeded_at                TIMESTAMPTZ,
    terminal_at                 TIMESTAMPTZ,
    CONSTRAINT tasks_id_nonempty CHECK (char_length(id) > 0),
    CONSTRAINT tasks_source_nonempty CHECK (char_length(source) > 0),
    CONSTRAINT tasks_source_instance_nonempty CHECK (char_length(source_instance_id) > 0),
    CONSTRAINT tasks_message_id_nonempty CHECK (char_length(message_id) > 0),
    CONSTRAINT tasks_message_key_nonempty CHECK (char_length(message_idempotency_key) > 0),
    CONSTRAINT tasks_prompt_nonempty CHECK (char_length(prompt) > 0),
    CONSTRAINT tasks_tool_policy_nonempty CHECK (char_length(tool_policy_version) > 0),
    CONSTRAINT tasks_prompt_bytes_nonneg CHECK (prompt_bytes >= 0),
    CONSTRAINT tasks_persona_bytes_nonneg CHECK (persona_bytes >= 0),
    CONSTRAINT tasks_status_check CHECK (status IN (
        'queued', 'starting', 'running', 'succeeded', 'failed', 'cancelled', 'interrupted'
    )),
    CONSTRAINT tasks_claim_invariants CHECK (
        (
            status IN ('queued', 'succeeded', 'failed', 'cancelled', 'interrupted')
            AND claim_owner IS NULL
            AND claim_lease_until IS NULL
        )
        OR (
            status IN ('starting', 'running')
            AND claim_owner IS NOT NULL
            AND char_length(claim_owner) > 0
            AND claim_lease_until IS NOT NULL
        )
    )
);

CREATE UNIQUE INDEX tasks_message_dedupe
    ON tasks (source, source_instance_id, message_idempotency_key);

CREATE UNIQUE INDEX tasks_session_order
    ON tasks (session_key, session_sequence);

CREATE UNIQUE INDEX one_running_task_per_session
    ON tasks (session_key)
    WHERE status IN ('starting', 'running');

CREATE INDEX tasks_session_status_idx ON tasks (session_key, status);
CREATE INDEX tasks_claim_lease_idx ON tasks (claim_owner, claim_lease_until)
    WHERE status IN ('starting', 'running');

CREATE TABLE workspace_snapshots (
    id                  UUID PRIMARY KEY,
    workspace_id        UUID NOT NULL REFERENCES workspaces (id),
    task_id             TEXT NOT NULL REFERENCES tasks (id),
    schema_version      TEXT NOT NULL,
    state               TEXT NOT NULL,
    generation          BIGINT NOT NULL,
    lease_owner         TEXT,
    lease_until         TIMESTAMPTZ,
    token               TEXT NOT NULL,
    staging_ref         TEXT,
    file_ref            TEXT,
    checksum            TEXT,
    result_ref          TEXT,
    result_digest       TEXT,
    result_bytes        INTEGER,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT (timezone('utc', now())),
    committed_at        TIMESTAMPTZ,
    CONSTRAINT workspace_snapshots_schema_nonempty CHECK (char_length(schema_version) > 0),
    CONSTRAINT workspace_snapshots_state_check CHECK (
        state IN ('writing', 'committed', 'quarantined')
    ),
    CONSTRAINT workspace_snapshots_generation_pos CHECK (generation > 0),
    CONSTRAINT workspace_snapshots_token_nonempty CHECK (char_length(token) > 0),
    CONSTRAINT workspace_snapshots_writing_lease CHECK (
        state <> 'writing'
        OR (
            lease_owner IS NOT NULL AND char_length(lease_owner) > 0
            AND lease_until IS NOT NULL
            AND staging_ref IS NOT NULL AND char_length(staging_ref) > 0
        )
    ),
    CONSTRAINT workspace_snapshots_committed_fields CHECK (
        state <> 'committed'
        OR (
            file_ref IS NOT NULL AND char_length(file_ref) > 0
            AND checksum IS NOT NULL AND char_length(checksum) > 0
            AND result_ref IS NOT NULL AND char_length(result_ref) > 0
            AND result_digest IS NOT NULL AND char_length(result_digest) > 0
            AND committed_at IS NOT NULL
        )
    )
);

CREATE UNIQUE INDEX workspace_snapshots_token_uq ON workspace_snapshots (token);
CREATE INDEX workspace_snapshots_workspace_idx ON workspace_snapshots (workspace_id, state);

ALTER TABLE workspaces
    ADD CONSTRAINT workspaces_current_snapshot_fk
    FOREIGN KEY (current_snapshot_id) REFERENCES workspace_snapshots (id);

ALTER TABLE tasks
    ADD CONSTRAINT tasks_snapshot_fk
    FOREIGN KEY (snapshot_id) REFERENCES workspace_snapshots (id);

CREATE TABLE task_events (
    id              BIGSERIAL PRIMARY KEY,
    task_id         TEXT NOT NULL REFERENCES tasks (id),
    event_type      TEXT NOT NULL,
    sequence_no     BIGINT NOT NULL,
    byte_count      INTEGER,
    digest          TEXT,
    from_status     TEXT,
    to_status       TEXT,
    worker_instance TEXT,
    error_code      TEXT,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT (timezone('utc', now())),
    CONSTRAINT task_events_type_check CHECK (event_type IN (
        'status_transition', 'chunk', 'tool_progress', 'dispatch', 'cancel_request'
    )),
    CONSTRAINT task_events_sequence_pos CHECK (sequence_no >= 0),
    CONSTRAINT task_events_byte_count_nonneg CHECK (byte_count IS NULL OR byte_count >= 0)
);

CREATE UNIQUE INDEX task_events_task_seq ON task_events (task_id, sequence_no);
CREATE INDEX task_events_task_idx ON task_events (task_id);

CREATE TABLE task_deliveries (
    delivery_id         TEXT PRIMARY KEY,
    task_id             TEXT NOT NULL REFERENCES tasks (id),
    delivery_type       TEXT NOT NULL,
    status              TEXT NOT NULL,
    payload_ref         TEXT,
    payload_digest      TEXT,
    error_code          TEXT,
    error_message       TEXT,
    error_trace_id      TEXT,
    attempt_count       INTEGER NOT NULL DEFAULT 0,
    next_attempt_at     TIMESTAMPTZ,
    attempt_lease_until TIMESTAMPTZ,
    delivery_deadline_at TIMESTAMPTZ,
    sent_at             TIMESTAMPTZ,
    acked_at            TIMESTAMPTZ,
    terminal_at         TIMESTAMPTZ,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT (timezone('utc', now())),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT (timezone('utc', now())),
    CONSTRAINT task_deliveries_id_nonempty CHECK (char_length(delivery_id) > 0),
    CONSTRAINT task_deliveries_type_check CHECK (delivery_type IN (
        'task_complete', 'task_failed', 'task_cancelled', 'task_interrupted'
    )),
    CONSTRAINT task_deliveries_status_check CHECK (status IN (
        'pending', 'sending', 'acked', 'dead_letter'
    )),
    CONSTRAINT task_deliveries_attempt_nonneg CHECK (attempt_count >= 0),
    CONSTRAINT task_deliveries_complete_payload CHECK (
        delivery_type <> 'task_complete'
        OR (
            payload_ref IS NOT NULL AND char_length(payload_ref) > 0
            AND payload_digest IS NOT NULL AND char_length(payload_digest) > 0
        )
    ),
    CONSTRAINT task_deliveries_error_payload CHECK (
        delivery_type = 'task_complete'
        OR (
            error_code IS NOT NULL AND char_length(error_code) > 0
        )
    )
);

CREATE UNIQUE INDEX task_deliveries_task_type
    ON task_deliveries (task_id, delivery_type);

CREATE UNIQUE INDEX task_deliveries_delivery_id
    ON task_deliveries (delivery_id);
