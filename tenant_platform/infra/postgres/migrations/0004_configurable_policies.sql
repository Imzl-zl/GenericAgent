-- Migration 0004: Admin-configurable platform commands + tool policies.
--
-- Problem: commands were hardcoded in router.go (Go switch), tool policy was
-- a static JSON file. Changing either required recompiling the platform.
--
-- Design:
--   platform_commands — admin can enable/disable commands, change help text,
--                       switch a command between "intercept" (platform handles)
--                       and "passthrough" (forward to Worker as task).
--   tool_policies     — admin can create new policy versions, change allowed
--                       tools, without touching source code or JSON files.
--
-- Handler logic stays in Go code (each intercept command has a handler func);
-- the *registry* (which commands exist, are they enabled, what's their help
-- text) is database-driven. Adding a brand-new intercept command needs code,
-- but enabling/disabling/reclassifying existing commands is admin-only.

CREATE TABLE platform_commands (
    id              BIGSERIAL PRIMARY KEY,
    command         TEXT NOT NULL UNIQUE,
    -- "intercept": platform handles it (e.g. /stop cancels task in PostgreSQL)
    -- "passthrough": forward as task to Worker, GA decides if it's a command
    action          TEXT NOT NULL,
    handler         TEXT NOT NULL,
    help_text       TEXT,
    enabled         BOOLEAN NOT NULL DEFAULT true,
    sort_order      INT NOT NULL DEFAULT 0,
    updated_by      BIGINT,
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT (timezone('utc', now())),
    created_at      TIMESTAMPTZ NOT NULL DEFAULT (timezone('utc', now())),
    CONSTRAINT platform_commands_action_check CHECK (action IN ('intercept', 'passthrough')),
    CONSTRAINT platform_commands_command_nonempty CHECK (char_length(command) > 0),
    CONSTRAINT platform_commands_handler_nonempty CHECK (char_length(handler) > 0)
);

CREATE TABLE tool_policies (
    id              BIGSERIAL PRIMARY KEY,
    version         TEXT NOT NULL UNIQUE,
    allowed_tools   JSONB NOT NULL,
    description     TEXT,
    enabled         BOOLEAN NOT NULL DEFAULT true,
    created_by      BIGINT,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT (timezone('utc', now())),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT (timezone('utc', now())),
    CONSTRAINT tool_policies_version_nonempty CHECK (char_length(version) > 0)
);

-- Seed: platform commands (matches router.go handler keys)
INSERT INTO platform_commands (command, action, handler, help_text, sort_order) VALUES
    ('/help',     'intercept', 'help',       '显示帮助', 1),
    ('/status',   'intercept', 'status',     '查看任务状态', 2),
    ('/stop',     'intercept', 'stop',       '停止当前任务', 3),
    ('/new',      'intercept', 'new',        '新对话确认', 4),
    ('/llm',      'intercept', 'llm',        '查看模型信息', 5),
    ('/activate', 'intercept', 'activate',   '绑定微信账号', 6);

-- Seed: tool policy (matches contracts/policy/foundation.v1.json)
INSERT INTO tool_policies (version, allowed_tools, description) VALUES
    ('foundation.no-host-tools.v1', '["update_working_checkpoint"]'::jsonb, 'Foundation: no host tools, deny-by-default');
