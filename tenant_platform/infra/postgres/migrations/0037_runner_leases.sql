-- 0037_runner_leases.sql
-- 持久 Runner lease 控制面记录(spec §3: 串行调度与 generation fencing)。
-- 同一 runner_key 最多一个活跃 lease;generation 单调递增,每次 Runner
-- 重建递增;创建/复用/回收/孤儿清理都以 generation fencing。

CREATE TABLE IF NOT EXISTS runner_leases (
    runner_key       TEXT PRIMARY KEY CHECK (octet_length(runner_key) BETWEEN 3 AND 256),
    owner            TEXT NOT NULL CHECK (octet_length(owner) BETWEEN 1 AND 128),
    generation       BIGINT NOT NULL CHECK (generation > 0),
    container_id     TEXT NOT NULL DEFAULT '' CHECK (octet_length(container_id) <= 128),
    control_endpoint TEXT NOT NULL DEFAULT '' CHECK (octet_length(control_endpoint) <= 512),
    status           TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active')),
    expires_at       TIMESTAMPTZ NOT NULL,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT timezone('utc', now()),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT timezone('utc', now())
);

CREATE INDEX IF NOT EXISTS runner_leases_expiry_idx ON runner_leases (expires_at);

CREATE TABLE IF NOT EXISTS migration_0037_runner_leases_marker (
    id BOOLEAN PRIMARY KEY DEFAULT TRUE CHECK (id)
);
INSERT INTO migration_0037_runner_leases_marker(id)
VALUES (TRUE)
ON CONFLICT (id) DO NOTHING;
