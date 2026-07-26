-- 0022_task_started_delivery.sql
-- Add task_started delivery type for immediate user feedback

-- Drop and recreate type constraint to include task_started
ALTER TABLE task_deliveries DROP CONSTRAINT task_deliveries_type_check;
ALTER TABLE task_deliveries ADD CONSTRAINT task_deliveries_type_check
  CHECK (delivery_type = ANY (ARRAY[
    'task_started'::text,
    'task_complete'::text,
    'task_failed'::text,
    'task_cancelled'::text,
    'task_interrupted'::text
  ]));

-- Adjust error_payload constraint: task_started and task_complete don't require error_code
ALTER TABLE task_deliveries DROP CONSTRAINT task_deliveries_error_payload;
ALTER TABLE task_deliveries ADD CONSTRAINT task_deliveries_error_payload
  CHECK (
    delivery_type IN ('task_started', 'task_complete')
    OR (error_code IS NOT NULL AND char_length(error_code) > 0)
  );
