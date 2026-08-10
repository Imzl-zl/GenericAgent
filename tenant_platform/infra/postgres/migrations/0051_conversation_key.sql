-- Migration 0051: 对话单元分桶（conversation_key）。
--
-- 背景: 同 workspace 内对话上下文按"对话单元"分桶隔离
-- (IM_CHANNEL_ARCHITECTURE.zh-CN.md §3) —— 微信个人自用单桶、
-- 可群聊渠道每个群/私聊一桶。空字符串 = 存量单桶(微信), 行为不变。
--
-- 设计: 恢复/提交的"选桶"在 Platform 控制面(CurrentRestorePoint/
-- PrepareCheckpoint), worker 只读写给定 ref —— 本迁移只给 tasks 与
-- workspace_snapshots 加列, 不动 worker 契约语义。
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS conversation_key TEXT NOT NULL DEFAULT '';

ALTER TABLE workspace_snapshots ADD COLUMN IF NOT EXISTS conversation_key TEXT NOT NULL DEFAULT '';

-- 按桶查最近 committed 恢复点: generation 为 workspace 级单调递增,
-- 桶内取 MAX(generation) 的 committed 行即该桶最近恢复点。
CREATE INDEX IF NOT EXISTS workspace_snapshots_bucket_idx
    ON workspace_snapshots (workspace_id, conversation_key, state, generation DESC);

CREATE INDEX IF NOT EXISTS tasks_bucket_idx ON tasks (workspace_id, conversation_key);

CREATE TABLE IF NOT EXISTS migration_0051_conversation_key_marker (
    id BOOLEAN PRIMARY KEY DEFAULT TRUE CHECK (id)
);
INSERT INTO migration_0051_conversation_key_marker(id)
VALUES (TRUE)
ON CONFLICT (id) DO NOTHING;
