-- 0035_sophub_sop_registry.sql
-- Admin-controlled Sophub import and immutable native GA SOP loading.

CREATE TABLE IF NOT EXISTS sophub_bindings (
    id                    BOOLEAN PRIMARY KEY DEFAULT TRUE CHECK (id),
    api_key_ciphertext    BYTEA NOT NULL CHECK (octet_length(api_key_ciphertext) > 0),
    api_key_version       INTEGER NOT NULL CHECK (api_key_version > 0),
    remote_author_type    TEXT NOT NULL DEFAULT '' CHECK (octet_length(remote_author_type) <= 64),
    remote_agent_uid      TEXT NOT NULL DEFAULT '' CHECK (octet_length(remote_agent_uid) <= 256),
    remote_display_name   TEXT NOT NULL DEFAULT '' CHECK (octet_length(remote_display_name) <= 256),
    verified_at           TIMESTAMPTZ,
    updated_by            BIGINT NOT NULL REFERENCES users(id),
    created_at            TIMESTAMPTZ NOT NULL DEFAULT timezone('utc', now()),
    updated_at            TIMESTAMPTZ NOT NULL DEFAULT timezone('utc', now())
);

CREATE TABLE IF NOT EXISTS sop_candidates (
    id              UUID PRIMARY KEY,
    remote_sop_id   TEXT NOT NULL CHECK (octet_length(remote_sop_id) BETWEEN 1 AND 256 AND remote_sop_id = btrim(remote_sop_id)),
    title           TEXT NOT NULL CHECK (octet_length(title) BETWEEN 1 AND 200 AND title = btrim(title)),
    description     TEXT NOT NULL DEFAULT '' CHECK (octet_length(description) <= 2048 AND description = btrim(description)),
    file_type       TEXT NOT NULL CHECK (file_type = 'markdown'),
    content         TEXT NOT NULL CHECK (octet_length(content) BETWEEN 1 AND 65536),
    source_digest   TEXT NOT NULL CHECK (source_digest ~ '^[a-f0-9]{64}$'),
    status          TEXT NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'approved', 'rejected')),
    reviewed_by     BIGINT REFERENCES users(id),
    review_note     TEXT NOT NULL DEFAULT '' CHECK (octet_length(review_note) <= 2048),
    reviewed_at     TIMESTAMPTZ,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT timezone('utc', now()),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT timezone('utc', now()),
    UNIQUE(remote_sop_id, source_digest),
    CHECK (
        (status = 'pending' AND reviewed_by IS NULL AND reviewed_at IS NULL)
        OR (status IN ('approved', 'rejected') AND reviewed_by IS NOT NULL AND reviewed_at IS NOT NULL)
    )
);

CREATE TABLE IF NOT EXISTS sop_entries (
    id                  UUID PRIMARY KEY,
    remote_sop_id       TEXT NOT NULL UNIQUE CHECK (octet_length(remote_sop_id) BETWEEN 1 AND 256),
    created_at          TIMESTAMPTZ NOT NULL DEFAULT timezone('utc', now()),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT timezone('utc', now())
);

CREATE TABLE IF NOT EXISTS sop_versions (
    id              UUID PRIMARY KEY,
    entry_id        UUID NOT NULL REFERENCES sop_entries(id),
    candidate_id    UUID NOT NULL UNIQUE REFERENCES sop_candidates(id),
    version         INTEGER NOT NULL CHECK (version > 0),
    title           TEXT NOT NULL CHECK (octet_length(title) BETWEEN 1 AND 200),
    description     TEXT NOT NULL DEFAULT '' CHECK (octet_length(description) <= 2048),
    content         TEXT NOT NULL CHECK (octet_length(content) BETWEEN 1 AND 65536),
    content_digest  TEXT NOT NULL CHECK (content_digest ~ '^[a-f0-9]{64}$'),
    approved_by     BIGINT NOT NULL REFERENCES users(id),
    approved_at     TIMESTAMPTZ NOT NULL DEFAULT timezone('utc', now()),
    UNIQUE(entry_id, version),
    UNIQUE(entry_id, content_digest),
    UNIQUE(entry_id, id)
);

ALTER TABLE sop_entries
    ADD COLUMN IF NOT EXISTS loaded_version_id UUID,
    ADD COLUMN IF NOT EXISTS loaded_by BIGINT REFERENCES users(id),
    ADD COLUMN IF NOT EXISTS loaded_at TIMESTAMPTZ;

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint WHERE conname = 'sop_entries_loaded_version_fk'
    ) THEN
        ALTER TABLE sop_entries
            ADD CONSTRAINT sop_entries_loaded_version_fk
            FOREIGN KEY (id, loaded_version_id)
            REFERENCES sop_versions(entry_id, id);
    END IF;
END $$;

CREATE OR REPLACE FUNCTION reject_sop_version_mutation()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    RAISE EXCEPTION 'installed SOP versions are append-only' USING ERRCODE = '55000';
END;
$$;

DROP TRIGGER IF EXISTS sop_versions_append_only ON sop_versions;
CREATE TRIGGER sop_versions_append_only
BEFORE UPDATE OR DELETE ON sop_versions
FOR EACH ROW EXECUTE FUNCTION reject_sop_version_mutation();

CREATE TABLE IF NOT EXISTS task_sop_snapshots (
    task_id         TEXT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    ordinal         INTEGER NOT NULL CHECK (ordinal >= 0 AND ordinal < 16),
    sop_version_id  UUID NOT NULL REFERENCES sop_versions(id),
    content_digest  TEXT NOT NULL CHECK (content_digest ~ '^[a-f0-9]{64}$'),
    created_at      TIMESTAMPTZ NOT NULL DEFAULT timezone('utc', now()),
    PRIMARY KEY(task_id, ordinal),
    UNIQUE(task_id, sop_version_id)
);

-- A snapshot may only be assembled in the transaction that created its task.
-- The task row's immutable creation timestamp and PostgreSQL xmin jointly
-- prove that identity without adding a second mutable sealing column.
CREATE OR REPLACE FUNCTION guard_task_sop_snapshot()
RETURNS trigger
LANGUAGE plpgsql
AS $$
DECLARE
    creation_time TIMESTAMPTZ;
    creation_txid XID8;
BEGIN
    IF TG_TABLE_NAME = 'tasks' THEN
        IF NEW.created_at IS DISTINCT FROM OLD.created_at THEN
            RAISE EXCEPTION 'task SOP snapshot creation timestamp is immutable' USING ERRCODE = '55000';
        END IF;
        RETURN NEW;
    END IF;

    IF TG_OP <> 'INSERT' THEN
        RAISE EXCEPTION 'task SOP snapshots are sealed' USING ERRCODE = '55000';
    END IF;

    SELECT created_at, xmin::text::xid8
    INTO creation_time, creation_txid
    FROM tasks
    WHERE id = NEW.task_id;
    IF creation_time IS DISTINCT FROM timezone('utc', transaction_timestamp())
       OR creation_txid IS DISTINCT FROM pg_current_xact_id() THEN
        RAISE EXCEPTION 'task SOP snapshots may only be inserted in the task creation transaction' USING ERRCODE = '55000';
    END IF;
    RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS tasks_sop_creation_identity_immutable ON tasks;
CREATE TRIGGER tasks_sop_creation_identity_immutable
BEFORE UPDATE OF created_at ON tasks
FOR EACH ROW EXECUTE FUNCTION guard_task_sop_snapshot();

DROP TRIGGER IF EXISTS task_sop_snapshots_sealed ON task_sop_snapshots;
CREATE TRIGGER task_sop_snapshots_sealed
BEFORE INSERT OR UPDATE OR DELETE ON task_sop_snapshots
FOR EACH ROW EXECUTE FUNCTION guard_task_sop_snapshot();

CREATE TABLE IF NOT EXISTS migration_0035_sophub_sop_registry_marker (
    id BOOLEAN PRIMARY KEY DEFAULT TRUE CHECK (id)
);
INSERT INTO migration_0035_sophub_sop_registry_marker(id)
VALUES(TRUE) ON CONFLICT DO NOTHING;
