-- Migration 0017: WeChat ClawBot @username relay (P1 slice 2)
--
-- Adds the relay_preferences table for per-user relay opt-out, and seeds the
-- /relay_off and /relay_on platform commands. The relay feature lets user A
-- send "@<username> <text>" to their bot; the router intercepts it (no LLM),
-- resolves the recipient's bot, and forwards the message via iLink.
--
-- Design notes:
--   - Opt-out lives in a dedicated table (not a users column) to keep the
--     users hot path untouched. Default is opted-in (opt_out=FALSE).
--   - relay is bidirectional and symmetric: B replies with "@<sender> ..."
--     and the same intercept handles the reverse direction.
--   - Inbound messages are already audited via messages table (migration
--     0013). Outbound relay messages reuse InsertOutboundMessage for audit.

CREATE TABLE IF NOT EXISTS relay_preferences (
    user_id     BIGINT PRIMARY KEY REFERENCES users (id) ON DELETE CASCADE,
    opt_out     BOOLEAN NOT NULL DEFAULT FALSE,
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT (timezone('utc', now()))
);

-- Seed relay control commands. Idempotent across re-applies.
INSERT INTO platform_commands (command, action, handler, help_text, sort_order) VALUES
    ('/relay_off', 'intercept', 'relay_off', '关闭接收 @用户名 转发消息', 20),
    ('/relay_on',  'intercept', 'relay_on',  '开启接收 @用户名 转发消息', 21)
ON CONFLICT (command) DO NOTHING;
