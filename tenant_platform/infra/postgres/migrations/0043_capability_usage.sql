-- 0043_capability_usage.sql
-- per-task capability 调用计量(审查 R4-I9): llm-proxy 按 JTI 原子计数,
-- 超过签发预算(max_turns)后拒绝请求, 防止 Runner 内代码绕过 Worker 的
-- RuntimePolicy 限制直接刷 LLM Proxy。行生命周期与 capability token 一致
-- (短 TTL); 终态撤销删除对应行见 revokeTaskCapabilityJTIs。
CREATE TABLE IF NOT EXISTS capability_usage (
    jti_hash   BYTEA PRIMARY KEY,
    max_calls  BIGINT NOT NULL CHECK (max_calls > 0),
    used_calls BIGINT NOT NULL DEFAULT 0 CHECK (used_calls >= 0),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT timezone('utc', now())
);

-- 审查: 迁移 marker 与 0042 模式一致——applyPendingMigrations 按 marker
-- 表判断已应用状态, 缺失会导致该文件在每次启动时被重复执行。
CREATE TABLE IF NOT EXISTS migration_0043_capability_usage_marker (
    id BOOLEAN PRIMARY KEY DEFAULT TRUE CHECK (id)
);
INSERT INTO migration_0043_capability_usage_marker(id)
VALUES (TRUE)
ON CONFLICT (id) DO NOTHING;
