package postgres

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/domain"
)

// insertPersonalWorkspaceTx 是 personal workspace 行的唯一 SQL 实现。
// 所有创建 personal workspace 的路径(用户注册 CreateUser/CreateUserWithInvite、
// 管理员 bootstrap ensureContextUser)都必须经由此 helper, 不得另写 INSERT。
//
// 生命周期不变量(迁移 0050 补存量, 注册路径保增量):
//
//	users 行存在 ⇔ workspaces 存在 session_key='personal:<uid>' 行
//
// 与用户状态(pending/approved/blocked)无关——任务提交的 approved 门禁在
// application 层(validateSessionAccess), workspace 行本身不授予能力。
// 由此审批(ApproveUser)保持纯状态迁移, 任何"创建用户的路径"自动获得
// workspace, 不存在"审批通过却无工作区"的窗口。
//
// volume_id 语义: 非 bootstrap 用户取 domain.WorkspaceDirHash(session_key),
// 与运行时共享卷路径(workspaces/<hash>)一致, 且满足
// workspaces_null_volume_requires_loopback 约束; bootstrap 用户为 NULL。
// ON CONFLICT (session_key) DO NOTHING 保证幂等(重复注册/并发安全)。
func insertPersonalWorkspaceTx(ctx context.Context, tx pgx.Tx, userID int64, volumeID, bootstrapMarker *string) (string, error) {
	wsKey := fmt.Sprintf("personal:%d", userID)
	wsID := uuid.New()
	if _, err := tx.Exec(ctx, `
INSERT INTO workspaces (id, session_key, owner_user_id, kind, team_id, volume_id, bootstrap_marker)
VALUES ($1, $2, $3, 'personal', NULL, $4, $5)
ON CONFLICT (session_key) DO NOTHING
`, wsID, wsKey, userID, volumeID, bootstrapMarker); err != nil {
		return "", fmt.Errorf("insert personal workspace %s: %w", wsKey, err)
	}
	return wsKey, nil
}

// personalWorkspaceVolumeID 返回普通用户 workspace 的 volume_id
// (domain.WorkspaceDirHash 的 hex 值, 与运行时路径同源)。
func personalWorkspaceVolumeID(sessionKey string) (string, error) {
	hash, err := domain.WorkspaceDirHash(sessionKey)
	if err != nil {
		return "", fmt.Errorf("workspace dir hash %q: %w", sessionKey, err)
	}
	return hash, nil
}

// WorkspaceIDByKeyTx 在给定事务内按 session_key 返回 workspace id。
// 供 ensureContextUser 在 insertPersonalWorkspaceTx(幂等 DO NOTHING)
// 之后取回新行 id。
func (s *Store) WorkspaceIDByKeyTx(ctx context.Context, tx pgx.Tx, sessionKey string) (uuid.UUID, error) {
	var id uuid.UUID
	if err := tx.QueryRow(ctx, `SELECT id FROM workspaces WHERE session_key = $1`, sessionKey).Scan(&id); err != nil {
		return uuid.UUID{}, fmt.Errorf("workspace id for %q: %w", sessionKey, err)
	}
	return id, nil
}
