-- 0044_task_delivery_files.sql
-- 成功任务声明的输出文件内容在成功事务内绑定到 task_complete outbox
-- (审查 R5-I3): 任务完成时(串行槽释放前)把 [FILE:...] 标记文件的安全
-- 快照写入本表, 异步 delivery 直接发送快照内容, 不再重新解析 workspace
-- 路径——否则同 Runner 下一条串行任务可能覆盖/删除同名输出, 交付错误内容。
-- 行生命周期: 随 task_deliveries 保留(审计"交付了什么"), 由 delivery
-- service 定期按保留期清理(DeleteExpiredDeliveryFiles)。
CREATE TABLE IF NOT EXISTS task_delivery_files (
    delivery_id TEXT NOT NULL,
    marker      TEXT NOT NULL,
    file_name   TEXT NOT NULL,
    rel_path    TEXT NOT NULL,
    content     BYTEA NOT NULL,
    digest      TEXT NOT NULL,
    size_bytes  BIGINT NOT NULL CHECK (size_bytes >= 0),
    created_at  TIMESTAMPTZ NOT NULL DEFAULT timezone('utc', now()),
    PRIMARY KEY (delivery_id, marker)
);
CREATE INDEX IF NOT EXISTS task_delivery_files_delivery_id_idx
    ON task_delivery_files (delivery_id);

-- 审查: 迁移 marker 与 0042/0043 模式一致——applyPendingMigrations 按
-- marker 表判断已应用状态, 缺失会导致该文件在每次启动时被重复执行。
CREATE TABLE IF NOT EXISTS migration_0044_task_delivery_files_marker (
    id BOOLEAN PRIMARY KEY DEFAULT TRUE CHECK (id)
);
INSERT INTO migration_0044_task_delivery_files_marker(id)
VALUES (TRUE)
ON CONFLICT (id) DO NOTHING;
