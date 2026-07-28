-- 0027: index durable outbound delivery-part progress lookups.

CREATE INDEX IF NOT EXISTS messages_outbound_task_type_idx
    ON messages (task_id, message_type)
    WHERE direction = 'outbound' AND task_id IS NOT NULL;

CREATE TABLE IF NOT EXISTS migration_0027_outbound_delivery_progress_marker (
    applied_at TIMESTAMPTZ NOT NULL DEFAULT (timezone('utc', now()))
);
