-- Migration 0057: task_delivery_files.spool_path — 出站文件存储 spool 引用化。
--
-- 背景(2026-08-13 架构审查 B4/T5): task_delivery_files.content 存文件字节
-- (BYTEA, 单文件 ≤8MiB × 32 文件), 任务成功事务峰值内存/DB 压力大, 且
-- Phase C 视频(几十 MB)必然超限。改: 成功事务时把 [FILE:...] 输出文件
-- 流式复制到 delivery spool 共享卷(GA_DELIVERY_SPOOL_DIR, Platform rw /
-- Bot Poller ro 同卷), DB 只存 spool 相对路径 + 摘要——发送时直接读 spool。
--
-- 决策:
--   * spool_path 可空: 存量行(content 快照)继续兼容, 由既有 30d 保留期
--     自然过期; 新写入行 spool_path 非空、content 为空。
--   * 读取优先 spool_path; 空则回退 content(存量)。
--   * spool 文件清理与 DB 保留期一致(30d, mtime 清扫, delivery_service)。
--   * jsonb/DO 块条件执行(0052/0056 先例, 重放幂等)。
DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM information_schema.columns
        WHERE table_name = 'task_delivery_files' AND column_name = 'spool_path'
    ) THEN
        ALTER TABLE task_delivery_files ADD COLUMN spool_path TEXT;
    END IF;
END $$;
