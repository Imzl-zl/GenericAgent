-- Migration 0046: Remove tool policy schema (审查 D1 去分级).
--
-- 动态工具策略(0004 tool_policies 表 / 0005 users.tool_policy_version 列)
-- 已停用: 工具能力统一由静态 policy manifest(foundation.v1.json) 决定,
-- 不再按用户分配或通过 DB 动态创建。tasks.tool_policy_version 列保留
-- (worker 执行链仍按任务快照解析静态档位)。

DROP TABLE IF EXISTS tool_policies;
ALTER TABLE users DROP COLUMN IF EXISTS tool_policy_version;
DROP INDEX IF EXISTS users_tool_policy_version_idx;

CREATE TABLE IF NOT EXISTS migration_0046_drop_tool_policy_marker (
    id BOOLEAN PRIMARY KEY DEFAULT TRUE CHECK (id)
);
INSERT INTO migration_0046_drop_tool_policy_marker(id)
VALUES (TRUE)
ON CONFLICT (id) DO NOTHING;
