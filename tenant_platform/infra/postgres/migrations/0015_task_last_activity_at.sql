-- 0015: tasks.last_activity_at for idle/deadlock detection
--
-- Purpose:
--   Detect "Worker alive but deadlocked" (LLM HTTP call hung, GIL deadlock,
--   infinite loop) — the one scenario gRPC stream errors + heartbeat lease
--   loss cannot catch. Pattern is the industry-standard idle detection
--   (Temporal HeartbeatTimeout, Kubernetes liveness probe).
--
-- Mechanism:
--   * Updated on every chunk event via RecordChunkEvent (hot path, microcost).
--   * Updated on every Worker heartbeat event (drain_display_queue empty poll,
--     every 30s while LLM is thinking).
--   * reaper tick scans for status='running' AND last_activity_at < now()-idle
--     and finalizes those tasks (releases slot, prevents queue starvation).
--
-- Idle threshold is configured via TASK_IDLE_TIMEOUT_SECONDS env (default 300s
-- = 5 min, well above normal LLM thinking gaps).
--
-- Partial index only covers running rows (10-20 in P0), so maintenance cost
-- is negligible.

ALTER TABLE tasks
    ADD COLUMN IF NOT EXISTS last_activity_at TIMESTAMPTZ NOT NULL DEFAULT (timezone('utc', now()));

-- Backfill existing running tasks (rare on cold start) so reaper does not
-- immediately trip on rows with NULL-ish defaults.
UPDATE tasks
   SET last_activity_at = timezone('utc', now())
 WHERE status = 'running'
   AND last_activity_at IS NULL;

-- Partial index: only running rows. Keeps the index tiny (10-20 rows in P0)
-- and the reaper query microsecond-fast.
CREATE INDEX IF NOT EXISTS tasks_running_last_activity_idx
    ON tasks (last_activity_at)
    WHERE status = 'running';

CREATE TABLE IF NOT EXISTS migration_0015_task_last_activity_at_marker (
    applied_at TIMESTAMPTZ NOT NULL DEFAULT (timezone('utc', now()))
);
