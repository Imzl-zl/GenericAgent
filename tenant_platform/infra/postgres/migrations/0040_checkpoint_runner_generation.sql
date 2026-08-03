-- 0040_checkpoint_runner_generation.sql
-- workspace_snapshots 增加 runner_generation: 记录签发 checkpoint lease 时
-- 的 Runner lease generation(方案 §7 generation fencing)。Prepare 写入,
-- Commit/CompleteSucceeded 校验, 防止旧 generation Runner 在 lease 被接管
-- 后仍提交恢复点(审查 I7)。

ALTER TABLE workspace_snapshots
    ADD COLUMN runner_generation BIGINT;

UPDATE workspace_snapshots
SET runner_generation = 1
WHERE runner_generation IS NULL;

ALTER TABLE workspace_snapshots
    ALTER COLUMN runner_generation SET NOT NULL;

ALTER TABLE workspace_snapshots
    ADD CONSTRAINT workspace_snapshots_runner_generation_pos
    CHECK (runner_generation > 0);

CREATE TABLE IF NOT EXISTS migration_0040_checkpoint_runner_generation_marker (
    id BOOLEAN PRIMARY KEY DEFAULT TRUE CHECK (id)
);
INSERT INTO migration_0040_checkpoint_runner_generation_marker(id)
VALUES (TRUE)
ON CONFLICT (id) DO NOTHING;
