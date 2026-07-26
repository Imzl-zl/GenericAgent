-- 0018_session_reset.sql
-- /new 命令支持：标记 session 重置，下一个 task 以 fresh_session=true 跳过快照恢复。
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS fresh_session bool NOT NULL DEFAULT false;
ALTER TABLE workspaces ADD COLUMN IF NOT EXISTS reset_at timestamptz;

-- 部分索引：仅索引有 reset 标记的 workspace，SubmitTask 快速检查。
CREATE INDEX IF NOT EXISTS idx_workspaces_reset_at ON workspaces (session_key) WHERE reset_at IS NOT NULL;

CREATE TABLE IF NOT EXISTS migration_0018_session_reset_marker (
    applied_at TIMESTAMPTZ NOT NULL DEFAULT (timezone('utc', now()))
);
