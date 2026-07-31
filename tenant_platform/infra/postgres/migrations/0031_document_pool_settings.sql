-- 0031_document_pool_settings.sql
-- Atomically versioned administrator settings for the document execution pool.
-- A singleton row keeps every runtime consumer on one coherent snapshot.

CREATE TABLE IF NOT EXISTS document_pool_settings (
    singleton                   BOOLEAN PRIMARY KEY DEFAULT TRUE CHECK (singleton),
    enabled                     BOOLEAN NOT NULL DEFAULT FALSE,
    max_active                  INTEGER NOT NULL DEFAULT 1 CHECK (max_active >= 0),
    min_ready                   INTEGER NOT NULL DEFAULT 0 CHECK (min_ready >= 0 AND min_ready <= max_active),
    job_idle_ttl_seconds        INTEGER NOT NULL DEFAULT 600 CHECK (job_idle_ttl_seconds > 0),
    ready_idle_ttl_seconds      INTEGER NOT NULL DEFAULT 300 CHECK (ready_idle_ttl_seconds > 0),
    global_queue_limit          INTEGER NOT NULL DEFAULT 100 CHECK (global_queue_limit > 0),
    per_tenant_queue_limit      INTEGER NOT NULL DEFAULT 20 CHECK (per_tenant_queue_limit > 0 AND per_tenant_queue_limit <= global_queue_limit),
    per_tenant_active_limit     INTEGER NOT NULL DEFAULT 1 CHECK (per_tenant_active_limit >= 0 AND per_tenant_active_limit <= max_active),
    job_timeout_seconds         INTEGER NOT NULL DEFAULT 3600 CHECK (job_timeout_seconds > 0),
    command_timeout_seconds     INTEGER NOT NULL DEFAULT 300 CHECK (command_timeout_seconds > 0 AND command_timeout_seconds <= job_timeout_seconds),
    version                     BIGINT NOT NULL DEFAULT 1 CHECK (version > 0),
    updated_by                  BIGINT NOT NULL DEFAULT 0,
    updated_at                  TIMESTAMPTZ NOT NULL DEFAULT now(),
    reason                      TEXT NOT NULL DEFAULT 'initial migration' CHECK (char_length(reason) BETWEEN 1 AND 500)
);

INSERT INTO document_pool_settings(singleton)
VALUES (TRUE)
ON CONFLICT (singleton) DO NOTHING;

CREATE TABLE IF NOT EXISTS migration_0031_document_pool_settings_marker (
    id BOOLEAN PRIMARY KEY DEFAULT TRUE CHECK (id)
);
INSERT INTO migration_0031_document_pool_settings_marker(id)
VALUES (TRUE) ON CONFLICT DO NOTHING;
