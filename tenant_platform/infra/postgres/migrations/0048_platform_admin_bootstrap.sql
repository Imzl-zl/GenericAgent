-- Migration 0048: 生产平台管理员引导约束(方案 §7)。
--
-- 生产路径 cmd/platform 调用 EnsurePlatformAdminUser, 以
-- bootstrap_marker='platform-admin' 引导管理员 users + workspaces,
-- 且引导阶段 workspace 的 volume_id 为 NULL(共享卷由
-- WorkspaceCoordinator 在首次调度时写入)。
-- 0001_foundation 的 CHECK 约束只放行 'dev-loopback', 导致生产启动
-- bootstrap 必失败(SQLSTATE 23514)。本迁移放宽到两个 marker。

ALTER TABLE users DROP CONSTRAINT users_bootstrap_marker_check;
ALTER TABLE users ADD CONSTRAINT users_bootstrap_marker_check CHECK (
    bootstrap_marker IS NULL OR bootstrap_marker IN ('dev-loopback', 'platform-admin')
);

ALTER TABLE workspaces DROP CONSTRAINT workspaces_bootstrap_marker_check;
ALTER TABLE workspaces ADD CONSTRAINT workspaces_bootstrap_marker_check CHECK (
    bootstrap_marker IS NULL OR bootstrap_marker IN ('dev-loopback', 'platform-admin')
);

ALTER TABLE workspaces DROP CONSTRAINT workspaces_null_volume_requires_loopback;
ALTER TABLE workspaces ADD CONSTRAINT workspaces_null_volume_requires_loopback CHECK (
    volume_id IS NOT NULL OR bootstrap_marker IN ('dev-loopback', 'platform-admin')
);

CREATE TABLE IF NOT EXISTS migration_0048_platform_admin_bootstrap_marker (
    id BOOLEAN PRIMARY KEY DEFAULT TRUE CHECK (id)
);
INSERT INTO migration_0048_platform_admin_bootstrap_marker(id)
VALUES (TRUE)
ON CONFLICT (id) DO NOTHING;
