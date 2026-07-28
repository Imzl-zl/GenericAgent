-- 0026: expose only user-safe IM commands.
--
-- /llm mutates model selection in native GA frontends and must remain under
-- platform/operator control. /activate belonged to the removed legacy binding
-- flow. /abort is the safe GA WeChat alias for /stop.

UPDATE platform_commands
SET enabled = false,
    updated_at = timezone('utc', now())
WHERE command IN ('/llm', '/activate');

INSERT INTO platform_commands (command, action, handler, help_text, sort_order, enabled)
VALUES ('/abort', 'intercept', 'stop', '停止当前任务（/stop 别名）', 4, true)
ON CONFLICT (command) DO UPDATE
SET action = EXCLUDED.action,
    handler = EXCLUDED.handler,
    help_text = EXCLUDED.help_text,
    sort_order = EXCLUDED.sort_order,
    enabled = true,
    updated_at = timezone('utc', now());

CREATE TABLE IF NOT EXISTS migration_0026_safe_user_commands_marker (
    applied_at TIMESTAMPTZ NOT NULL DEFAULT (timezone('utc', now()))
);
