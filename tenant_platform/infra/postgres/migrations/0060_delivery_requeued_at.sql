-- Migration 0060: task_deliveries.requeued_at(admin 死信重投的窗口锚点扩展)。
--
-- 背景(2026-08-14 独立审查 + 子代理复审 P1): admin 重投端点把死信行置回
-- pending, 但 delivery 重试窗口的锚点是 tasks.terminal_at + retryWindow——
-- 事故发现通常在任务完成数小时后(08-14 场景即如此), 重投行会在下一个
-- tick 的 DeadLetterExpiredDeliveries(先于 claim 执行)里被原样打回死信,
-- 端点"假成功"。修复: requeue 时记录 requeued_at, 窗口锚点取
-- GREATEST(tasks.terminal_at, task_deliveries.requeued_at)——管理重投开启
-- 新的 30min 窗口, 且不污染用户可见的 tasks.terminal_at(任务完成时间)。
--
-- requeued_at 可空: NULL = 从未管理重投(存量行), 锚点退化为 terminal_at。
-- 迁移只加可空列, 无存量数据影响。幂等: DO 块条件执行(0052/0053 先例,
-- 重放迁移测试要求重复应用不报错)。

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM information_schema.columns
        WHERE table_name = 'task_deliveries' AND column_name = 'requeued_at'
    ) THEN
        ALTER TABLE task_deliveries ADD COLUMN requeued_at TIMESTAMPTZ;
    END IF;
END $$;
