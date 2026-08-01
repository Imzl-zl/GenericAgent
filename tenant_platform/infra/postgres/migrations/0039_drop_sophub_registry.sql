-- 0039_drop_sophub_registry.sql
-- 删除全局 SOP Registry 路径(方案 §5.2/§7):
-- "下载候选 -> 管理员审核 -> 全局 SOP Registry -> task snapshot 注入"整体删除,
-- SOP 改为每工作区 memory/sops/ 私有安装, 不再需要以下表。
-- sophub_bindings 保留: 部署管理员的 Sophub API Key 密文仍由 Platform 保存。

DROP TABLE IF EXISTS task_sop_snapshots CASCADE;
DROP TABLE IF EXISTS sop_versions CASCADE;
DROP TABLE IF EXISTS sop_entries CASCADE;
DROP TABLE IF EXISTS sop_candidates CASCADE;

-- 0035 创建的 tasks 表触发器与函数一并清理(先 drop trigger, 再 drop function)。
DROP TRIGGER IF EXISTS task_sop_snapshots_sealed ON task_sop_snapshots;
DROP TRIGGER IF EXISTS tasks_sop_creation_identity_immutable ON tasks;
DROP TRIGGER IF EXISTS sop_versions_append_only ON sop_versions;
DROP FUNCTION IF EXISTS guard_task_sop_snapshot();
DROP FUNCTION IF EXISTS reject_sop_version_mutation();

CREATE TABLE IF NOT EXISTS migration_0039_drop_sophub_registry_marker (
    id BOOLEAN PRIMARY KEY DEFAULT TRUE CHECK (id)
);
INSERT INTO migration_0039_drop_sophub_registry_marker(id)
VALUES (TRUE)
ON CONFLICT (id) DO NOTHING;
