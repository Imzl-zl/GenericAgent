-- 0032_document_job_pool.sql
-- Durable document job queue, commands, and single-use execution instances.

CREATE TABLE IF NOT EXISTS document_jobs (
    id                     UUID PRIMARY KEY,
    workspace_id           UUID NOT NULL REFERENCES workspaces(id),
    requester_user_id      BIGINT NOT NULL REFERENCES users(id),
    idempotency_key        TEXT NOT NULL CHECK (char_length(idempotency_key) BETWEEN 1 AND 256),
    payload                JSONB NOT NULL,
    payload_hash           TEXT NOT NULL CHECK (char_length(payload_hash) = 64),
    status                 TEXT NOT NULL DEFAULT 'queued',
    instance_id            UUID,
    claim_owner            TEXT,
    generation             BIGINT NOT NULL DEFAULT 0 CHECK (generation >= 0),
    claim_lease_until      TIMESTAMPTZ,
    claimed_at             TIMESTAMPTZ,
    last_activity_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    terminal_error_code    TEXT,
    terminal_error_message TEXT,
    created_at             TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at             TIMESTAMPTZ NOT NULL DEFAULT now(),
    started_at             TIMESTAMPTZ,
    terminal_at            TIMESTAMPTZ,
    CONSTRAINT document_jobs_status_check CHECK (
        status IN ('queued','starting','running','succeeded','failed','cancelled','expired')
    ),
    CONSTRAINT document_jobs_claim_state_check CHECK (
        (status = 'queued' AND instance_id IS NULL AND claim_owner IS NULL AND generation = 0
            AND claim_lease_until IS NULL AND claimed_at IS NULL AND terminal_at IS NULL)
        OR
        (status IN ('starting','running') AND instance_id IS NOT NULL AND claim_owner IS NOT NULL
            AND char_length(claim_owner) > 0 AND generation > 0 AND claim_lease_until IS NOT NULL
            AND claimed_at IS NOT NULL AND started_at IS NOT NULL AND terminal_at IS NULL)
        OR
        (status IN ('succeeded','failed','cancelled','expired') AND claim_owner IS NULL
            AND claim_lease_until IS NULL AND terminal_at IS NOT NULL)
    )
);

CREATE UNIQUE INDEX IF NOT EXISTS document_jobs_idempotency_uq
    ON document_jobs(workspace_id, idempotency_key);
CREATE UNIQUE INDEX IF NOT EXISTS document_jobs_instance_uq
    ON document_jobs(instance_id) WHERE instance_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS document_jobs_queue_idx
    ON document_jobs(status, created_at, id) WHERE status = 'queued';
CREATE INDEX IF NOT EXISTS document_jobs_active_workspace_idx
    ON document_jobs(workspace_id, status) WHERE status IN ('starting','running');
CREATE INDEX IF NOT EXISTS document_jobs_lease_idx
    ON document_jobs(claim_lease_until) WHERE status IN ('starting','running');

CREATE TABLE IF NOT EXISTS document_instances (
    id                 UUID PRIMARY KEY,
    instance_name      TEXT NOT NULL UNIQUE CHECK (char_length(instance_name) > 0),
    slot_path          TEXT NOT NULL UNIQUE CHECK (char_length(slot_path) > 0),
    status             TEXT NOT NULL,
    allocated_job_id   UUID UNIQUE REFERENCES document_jobs(id),
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    ready_at           TIMESTAMPTZ,
    allocated_at       TIMESTAMPTZ,
    destroy_at         TIMESTAMPTZ,
    CONSTRAINT document_instances_status_check CHECK (
        status IN ('ready','allocated','creating','running','destroying','destroyed','lost')
    ),
    CONSTRAINT document_instances_binding_check CHECK (
        (status = 'ready' AND allocated_job_id IS NULL AND ready_at IS NOT NULL AND allocated_at IS NULL)
        OR
        (status IN ('allocated','creating','running') AND allocated_job_id IS NOT NULL AND allocated_at IS NOT NULL)
        OR
        (status IN ('destroying','destroyed','lost'))
    )
);

ALTER TABLE document_jobs
    ADD CONSTRAINT document_jobs_instance_fk
    FOREIGN KEY (instance_id) REFERENCES document_instances(id);

CREATE INDEX IF NOT EXISTS document_instances_ready_idx
    ON document_instances(created_at, id) WHERE status = 'ready' AND allocated_job_id IS NULL;
CREATE INDEX IF NOT EXISTS document_instances_cleanup_idx
    ON document_instances(updated_at, id) WHERE status = 'destroying';

CREATE TABLE IF NOT EXISTS document_commands (
    id            UUID PRIMARY KEY,
    job_id        UUID NOT NULL REFERENCES document_jobs(id) ON DELETE CASCADE,
    command_id    TEXT NOT NULL CHECK (char_length(command_id) BETWEEN 1 AND 256),
    payload       JSONB NOT NULL,
    payload_hash  TEXT NOT NULL CHECK (char_length(payload_hash) = 64),
    status        TEXT NOT NULL DEFAULT 'pending',
    generation    BIGINT NOT NULL DEFAULT 0 CHECK (generation >= 0),
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    started_at    TIMESTAMPTZ,
    completed_at  TIMESTAMPTZ,
    CONSTRAINT document_commands_status_check CHECK (
        status IN ('pending','executing','succeeded','failed','expired')
    ),
    CONSTRAINT document_commands_execution_state_check CHECK (
        (status = 'pending' AND generation = 0 AND started_at IS NULL AND completed_at IS NULL)
        OR
        (status = 'executing' AND generation > 0 AND started_at IS NOT NULL AND completed_at IS NULL)
        OR
        (status IN ('succeeded','failed','expired') AND completed_at IS NOT NULL)
    ),
    UNIQUE(job_id, command_id)
);

CREATE INDEX IF NOT EXISTS document_commands_executing_idx
    ON document_commands(started_at) WHERE status = 'executing';

CREATE TABLE IF NOT EXISTS migration_0032_document_job_pool_marker (
    id BOOLEAN PRIMARY KEY DEFAULT TRUE CHECK (id)
);
INSERT INTO migration_0032_document_job_pool_marker(id)
VALUES(TRUE) ON CONFLICT DO NOTHING;
