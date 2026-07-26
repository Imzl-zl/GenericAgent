-- Migration 0020: Add last_event_sequence_no counter to tasks table.
--
-- Replaces the COALESCE(MAX(sequence_no),0)+1 pattern in insertNextEvent with
-- an atomic per-task counter. The UPDATE ... RETURNING on the task row itself
-- provides row-level serialization, eliminating the need for a separate
-- SELECT MAX scan and removing the fragility where correctness depended on
-- every caller holding a FOR UPDATE lock before inserting an event.
--
-- The counter is initialized to the current MAX(sequence_no) per task so
-- existing tasks continue from their correct next sequence number. New tasks
-- start at 0; the first insertNextEvent call increments to 1 (matching the
-- previous COALESCE(MAX(NULL),0)+1 = 1 semantics).
--
-- Marker table: migration_0020_task_event_sequence_counter_marker

ALTER TABLE tasks ADD COLUMN last_event_sequence_no BIGINT NOT NULL DEFAULT 0;

UPDATE tasks SET last_event_sequence_no = COALESCE(
    (SELECT MAX(sequence_no) FROM task_events WHERE task_events.task_id = tasks.id),
    0
);

CREATE TABLE IF NOT EXISTS migration_0020_task_event_sequence_counter_marker (
    applied_at TIMESTAMPTZ NOT NULL DEFAULT (timezone('utc', now()))
);
