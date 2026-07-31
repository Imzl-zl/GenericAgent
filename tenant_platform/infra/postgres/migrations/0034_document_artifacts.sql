-- 0034_document_artifacts.sql
-- Persist bounded document outputs before single-use container cleanup.

CREATE TABLE IF NOT EXISTS document_artifacts (
    id            UUID PRIMARY KEY,
    job_id        UUID NOT NULL,
    command_id    TEXT NOT NULL,
    file_name     TEXT NOT NULL CHECK (
        char_length(file_name) BETWEEN 1 AND 255
        AND file_name = btrim(file_name)
        AND file_name !~ '[\\/]'
        AND file_name NOT IN ('.', '..')
    ),
    media_type    TEXT NOT NULL CHECK (char_length(media_type) BETWEEN 1 AND 128),
    content       BYTEA NOT NULL CHECK (octet_length(content) BETWEEN 1 AND 8388608),
    size_bytes    BIGINT NOT NULL CHECK (size_bytes BETWEEN 1 AND 8388608),
    sha256        TEXT NOT NULL CHECK (sha256 ~ '^[a-f0-9]{64}$'),
    created_at    TIMESTAMPTZ NOT NULL DEFAULT timezone('utc', now()),
    CONSTRAINT document_artifacts_command_fk
        FOREIGN KEY (job_id, command_id)
        REFERENCES document_commands(job_id, command_id)
        ON DELETE CASCADE,
    CONSTRAINT document_artifacts_size_matches_content
        CHECK (size_bytes = octet_length(content)),
    UNIQUE (job_id, command_id)
);

CREATE TABLE IF NOT EXISTS migration_0034_document_artifacts_marker (
    id BOOLEAN PRIMARY KEY DEFAULT TRUE CHECK (id)
);
INSERT INTO migration_0034_document_artifacts_marker(id)
VALUES(TRUE) ON CONFLICT DO NOTHING;
