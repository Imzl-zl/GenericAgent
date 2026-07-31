-- 0033_document_job_finish.sql
-- Close command submission before successful document-job finalization.

ALTER TABLE document_jobs
    ADD COLUMN IF NOT EXISTS commands_closed_at TIMESTAMPTZ;

DO $$
BEGIN
    IF EXISTS (
        SELECT 1
        FROM document_jobs j
        JOIN document_commands c ON c.job_id = j.id
        WHERE j.status = 'succeeded' AND c.status <> 'succeeded'
    ) THEN
        RAISE EXCEPTION 'cannot migrate document jobs: historical succeeded job has non-succeeded command';
    END IF;
END $$;

-- Jobs that succeeded before this migration had an implicitly closed command set.
UPDATE document_jobs
SET commands_closed_at = COALESCE(commands_closed_at, terminal_at, updated_at)
WHERE status = 'succeeded' AND commands_closed_at IS NULL;

ALTER TABLE document_jobs
    DROP CONSTRAINT IF EXISTS document_jobs_succeeded_commands_closed_check;
ALTER TABLE document_jobs
    ADD CONSTRAINT document_jobs_succeeded_commands_closed_check CHECK (
        status <> 'succeeded' OR commands_closed_at IS NOT NULL
    );

-- Prewarm creation is a durable, unbound intent. A ready instance is also
-- unbound, while allocated/running instances must belong to exactly one job.
ALTER TABLE document_instances
    DROP CONSTRAINT IF EXISTS document_instances_binding_check;
ALTER TABLE document_instances
    ADD CONSTRAINT document_instances_binding_check CHECK (
        (status = 'creating' AND allocated_job_id IS NULL AND ready_at IS NULL AND allocated_at IS NULL)
        OR
        (status = 'ready' AND allocated_job_id IS NULL AND ready_at IS NOT NULL AND allocated_at IS NULL)
        OR
        (status IN ('allocated','running') AND allocated_job_id IS NOT NULL AND allocated_at IS NOT NULL)
        OR
        (status IN ('destroying','destroyed','lost'))
    );

CREATE TABLE IF NOT EXISTS migration_0033_document_job_finish_marker (
    id BOOLEAN PRIMARY KEY DEFAULT TRUE CHECK (id)
);
INSERT INTO migration_0033_document_job_finish_marker(id)
VALUES(TRUE) ON CONFLICT DO NOTHING;
