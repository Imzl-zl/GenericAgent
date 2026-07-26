-- 0021_tasks_requester_status_index.sql
-- Composite index for the hot path: SubmitTask + scheduler tick query
-- requester_user_id + status filter. Without this, large task tables degrade
-- to sequential scans on per-user queue/running lookups.
-- Partial index: only non-terminal statuses are queried in the hot path,
-- so we exclude terminal states to keep the index small.

CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_tasks_requester_status
    ON tasks (requester_user_id, status)
    WHERE status IN ('queued', 'starting', 'running');
