-- 0025: runtime-tunable platform settings.
--
-- First use-case: inbound IM coalescing window so admins can tune how long
-- the webhook/router waits to merge same-send text/media bursts into one task.

CREATE TABLE platform_runtime_settings (
    setting_key TEXT PRIMARY KEY,
    int_value   INT NOT NULL,
    updated_by  BIGINT,
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT (timezone('utc', now())),
    created_at  TIMESTAMPTZ NOT NULL DEFAULT (timezone('utc', now())),
    CONSTRAINT platform_runtime_settings_key_nonempty CHECK (char_length(setting_key) > 0),
    CONSTRAINT platform_runtime_settings_int_nonnegative CHECK (int_value >= 0)
);

INSERT INTO platform_runtime_settings (setting_key, int_value, updated_by)
VALUES ('im_inbound_coalesce_window_ms', 4000, 0)
ON CONFLICT (setting_key) DO NOTHING;

CREATE TABLE IF NOT EXISTS migration_0025_platform_runtime_settings_marker (
    applied_at TIMESTAMPTZ NOT NULL DEFAULT (timezone('utc', now()))
);
