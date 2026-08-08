-- Migration 0050: 为存量用户补齐 personal workspace。
--
-- 背景: 用户创建路径(CreateUser/CreateUserWithInvite)此前不建立
-- workspaces 行, 只有管理员 bootstrap 建; 普通用户审批通过后提交任务
-- 必因 workspace not found 失败(ROUTER_ERROR 500, poller 无限重试)。
-- 自本迁移起注册路径在同事务内幂等创建 personal:<uid> 行, 审批保持
-- 纯状态迁移。
--
-- 生命周期不变量: users 行 ⇔ workspaces 存在 session_key='personal:<uid>'
-- 行(与用户状态无关, 任务提交的 approved 门禁在 application 层)。
-- 因此本迁移为【所有】存量用户补行, 不只 approved——存量 pending 用户
-- 升级后审批才不会缺行。
--
-- volume_id 语义与 domain.WorkspaceDirHash 一致
-- (hex(sha256(session_key))), 满足 workspaces_null_volume_requires_loopback
-- 约束(bootstrap_marker 为 NULL 时 volume_id 必须非空)。

INSERT INTO workspaces (id, session_key, owner_user_id, kind, team_id, volume_id, bootstrap_marker)
SELECT gen_random_uuid(),
       'personal:' || u.id,
       u.id,
       'personal',
       NULL,
       encode(sha256(('personal:' || u.id)::bytea), 'hex'),
       NULL
FROM users u
WHERE NOT EXISTS (
      SELECT 1 FROM workspaces w WHERE w.session_key = 'personal:' || u.id
  );

CREATE TABLE IF NOT EXISTS migration_0050_user_personal_workspace_marker (
    id BOOLEAN PRIMARY KEY DEFAULT TRUE CHECK (id)
);
INSERT INTO migration_0050_user_personal_workspace_marker(id)
VALUES (TRUE)
ON CONFLICT (id) DO NOTHING;
