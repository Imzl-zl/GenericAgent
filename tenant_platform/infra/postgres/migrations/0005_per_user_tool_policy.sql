-- Migration 0005: Per-user tool policy assignment.
--
-- Problem: tool_policy_version was global (one policy for all users). Admins
-- couldn't grant different capabilities to different users (e.g. free users
-- get no-host-tools, premium users get code_run).
--
-- Design: each user gets their own tool_policy_version. The router resolves
-- the user's policy when handling messages and passes it to the task. Admins
-- can change a user's policy at runtime via PUT /v1/admin/users/{id}/tool-policy.
--
-- Default: 'foundation.no-host-tools.v1' (deny-by-default, safest).

ALTER TABLE users ADD COLUMN tool_policy_version TEXT NOT NULL DEFAULT 'foundation.no-host-tools.v1';

-- Index for fast per-user policy lookup.
CREATE INDEX users_tool_policy_version_idx ON users (tool_policy_version);

-- Add updated_at to users if not exists (needed for policy change audit).
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM information_schema.columns
                   WHERE table_name = 'users' AND column_name = 'updated_at') THEN
        ALTER TABLE users ADD COLUMN updated_at TIMESTAMPTZ NOT NULL DEFAULT (timezone('utc', now()));
    END IF;
END $$;
