-- 0041_runner_lease_stale_container.sql
-- runner_leases 增加 stale_container_id: 记录 lease 接管/重建时被替换的
-- 旧容器 ID, 供调用方定向清理(审查 C6)。接管事务不再清空 container_id
-- 前丢失旧容器身份, 新 Platform 启动 reconcile 可据此销毁旧容器。

ALTER TABLE runner_leases
    ADD COLUMN stale_container_id TEXT NOT NULL DEFAULT ''
    CHECK (octet_length(stale_container_id) <= 128);

CREATE TABLE IF NOT EXISTS migration_0041_runner_lease_stale_container_marker (
    id BOOLEAN PRIMARY KEY DEFAULT TRUE CHECK (id)
);
INSERT INTO migration_0041_runner_lease_stale_container_marker(id)
VALUES (TRUE)
ON CONFLICT (id) DO NOTHING;
