-- 0028: configurable Agent turn budget.
--
-- The prior production default of 6 turns was too small for multi-step tasks.
-- Persisting this value lets administrators tune future tasks without a
-- rebuild while the Worker and wall-clock limits still bound runaway loops.

INSERT INTO platform_runtime_settings (setting_key, int_value, updated_by)
VALUES ('agent_max_turns', 80, 0)
ON CONFLICT (setting_key) DO NOTHING;

CREATE TABLE IF NOT EXISTS migration_0028_agent_max_turns_setting_marker (
    applied_at TIMESTAMPTZ NOT NULL DEFAULT (timezone('utc', now()))
);
