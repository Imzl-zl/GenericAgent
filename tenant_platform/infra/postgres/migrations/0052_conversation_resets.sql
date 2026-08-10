-- Migration 0052: /new 桶级化（conversation_resets）。
--
-- 背景: 对话单元分桶(IM_CHANNEL_ARCHITECTURE §3)后, /new 语义应为
-- "清当前对话单元桶"——微信 /new 不得连带清掉其他桶(如飞书群)的下一个
-- 任务恢复。旧实现 workspaces.reset_at 是 workspace 级单标记。
--
-- 新模型: conversation_resets 按 (workspace_id, conversation_key) 一行,
-- 语义与旧 reset_at 一致(标记保留到该桶 fresh 任务成功终态, 失败/取消
-- 不消费)。
CREATE TABLE IF NOT EXISTS conversation_resets (
    workspace_id UUID NOT NULL REFERENCES workspaces (id),
    conversation_key TEXT NOT NULL DEFAULT '',
    reset_at TIMESTAMPTZ NOT NULL DEFAULT (timezone('utc', now())),
    PRIMARY KEY (workspace_id, conversation_key)
);

-- 存量 reset 标记迁移(旧实现只有默认桶语义 → conversation_key='')
-- 条件执行: 本迁移已应用后 workspaces.reset_at 列被删, EnsureSchema 重放
-- 场景(如测试删 marker)下直接 SELECT reset_at 会因列不存在而失败。
DO $$
BEGIN
  IF EXISTS (
    SELECT 1 FROM information_schema.columns
    WHERE table_schema = current_schema()
      AND table_name = 'workspaces'
      AND column_name = 'reset_at'
  ) THEN
    INSERT INTO conversation_resets (workspace_id, conversation_key, reset_at)
    SELECT id, '', reset_at FROM workspaces WHERE reset_at IS NOT NULL
    ON CONFLICT (workspace_id, conversation_key) DO NOTHING;
  END IF;
END
$$;

-- 旧列与索引退役(0018 的 workspace 级语义)
DROP INDEX IF EXISTS idx_workspaces_reset_at;
ALTER TABLE workspaces DROP COLUMN IF EXISTS reset_at;

CREATE TABLE IF NOT EXISTS migration_0052_conversation_resets_marker (
    id BOOLEAN PRIMARY KEY DEFAULT TRUE CHECK (id)
);
INSERT INTO migration_0052_conversation_resets_marker(id)
VALUES (TRUE)
ON CONFLICT (id) DO NOTHING;
