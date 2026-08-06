-- Migration 0047: Delivery attempt fencing token (审查 F2).
--
-- claim 时为每个 attempt 生成随机 token; Ack/Retry/DeadLetter 必须携带
-- 该 token 才能生效。防止旧 attempt 超时被重置后, 新 attempt 接管期间
-- 旧执行者仍可按 delivery_id+status 覆盖/提前终态化新 attempt。

ALTER TABLE task_deliveries ADD COLUMN attempt_token TEXT NOT NULL DEFAULT '';

CREATE TABLE IF NOT EXISTS migration_0047_delivery_attempt_token_marker (
    id BOOLEAN PRIMARY KEY DEFAULT TRUE CHECK (id)
);
INSERT INTO migration_0047_delivery_attempt_token_marker(id)
VALUES (TRUE)
ON CONFLICT (id) DO NOTHING;
