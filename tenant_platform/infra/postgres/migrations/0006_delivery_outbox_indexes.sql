-- Slice 3b: delivery outbox polling indexes.
CREATE INDEX IF NOT EXISTS task_deliveries_status_lease_idx
    ON task_deliveries (status, attempt_lease_until, created_at)
    WHERE status IN ('pending', 'sending');

CREATE INDEX IF NOT EXISTS task_deliveries_task_status_idx
    ON task_deliveries (task_id, status);
