-- 0042_task_capability_jtis.sql
-- 持久化每个已派发任务实际签发的 capability JTI 列表, 供崩溃恢复时撤销:
-- Platform 崩溃/被接管后, RecoverAfterRestart 中断的任务必须立即吊销其
-- 已签发的 LLM/Sophub capability, 否则旧 token 在 TTL(默认 1h)内仍可被
-- 旧 Runner 容器使用(审查: 撤销是持久工作流, 不只依赖进程内重试)。
ALTER TABLE tasks
    ADD COLUMN IF NOT EXISTS capability_jtis text[] NOT NULL DEFAULT '{}';

CREATE TABLE IF NOT EXISTS migration_0042_task_capability_jtis_marker (
    id BOOLEAN PRIMARY KEY DEFAULT TRUE CHECK (id)
);
INSERT INTO migration_0042_task_capability_jtis_marker(id)
VALUES (TRUE)
ON CONFLICT (id) DO NOTHING;
