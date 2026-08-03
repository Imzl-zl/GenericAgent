-- 0045_delivery_cancelled.sql
-- 审查 R5-M2: task_started 发送失败重试期间, 终态 delivery 可能先发出;
-- 恢复后用户会先收到完成消息、后收到"正在处理"。终态事务取消尚未发送的
-- task_started(pending → cancelled), 避免乱序。cancelled 是终态, 不参与
-- delivery claim/重试/死信。
ALTER TABLE task_deliveries DROP CONSTRAINT task_deliveries_status_check;
ALTER TABLE task_deliveries ADD CONSTRAINT task_deliveries_status_check
    CHECK (status IN ('pending', 'sending', 'acked', 'dead_letter', 'cancelled'));

CREATE TABLE IF NOT EXISTS migration_0045_delivery_cancelled_marker (
    id BOOLEAN PRIMARY KEY DEFAULT TRUE CHECK (id)
);
INSERT INTO migration_0045_delivery_cancelled_marker(id)
VALUES (TRUE)
ON CONFLICT (id) DO NOTHING;
