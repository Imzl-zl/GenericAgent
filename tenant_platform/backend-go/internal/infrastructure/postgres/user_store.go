package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/domain"
)

// CreateUser inserts a new user in status 'pending'. Username must be unique.
func (s *Store) CreateUser(ctx context.Context, username, passwordHash string) (domain.User, error) {
	if username == "" {
		return domain.User{}, fmt.Errorf("username is required")
	}
	var u domain.User
	err := s.withTx(ctx, func(tx pgx.Tx) error {
		// 审查: password_hash 为 NULL(空密码)时 COALESCE 成空串,
		// 否则 scanUser 扫 NULL 进 string 崩溃。
		return scanUser(tx.QueryRow(ctx, `
INSERT INTO users (username, status, password_hash)
VALUES ($1, 'pending', $2)
RETURNING id, username, COALESCE(password_hash,''), status, COALESCE(bootstrap_marker,''), created_at, approved_at
`, username, nullString(passwordHash)), &u)
	})
	return u, err
}

func nullString(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// GetUserByUsername returns a user by username.
func (s *Store) GetUserByUsername(ctx context.Context, username string) (domain.User, error) {
	if username == "" {
		return domain.User{}, fmt.Errorf("username is required")
	}
	var u domain.User
	row := s.pool.QueryRow(ctx, `
SELECT id, username, COALESCE(password_hash,''), status, COALESCE(bootstrap_marker,''), created_at, approved_at
FROM users WHERE username = $1
`, username)
	if err := scanUser(row, &u); err != nil {
		return domain.User{}, err
	}
	return u, nil
}

// ApproveUser transitions a user from 'pending' to 'approved' and records an
// audit event. Fails if the user is not in 'pending' state.
func (s *Store) ApproveUser(ctx context.Context, userID int64) (domain.User, error) {
	if userID <= 0 {
		return domain.User{}, fmt.Errorf("user id must be positive")
	}
	var u domain.User
	err := s.withTx(ctx, func(tx pgx.Tx) error {
		if err := scanUser(tx.QueryRow(ctx, `
SELECT id, username, COALESCE(password_hash,''), status, COALESCE(bootstrap_marker,''), created_at, approved_at
FROM users WHERE id = $1 FOR UPDATE
`, userID), &u); err != nil {
			return err
		}
		if u.Status != domain.UserPending {
			return fmt.Errorf("user %d is in state %s, cannot approve", userID, u.Status)
		}
		now := time.Now().UTC()
		if err := scanUser(tx.QueryRow(ctx, `
UPDATE users SET status = 'approved', approved_at = $2, updated_at = $2
WHERE id = $1 AND status = 'pending'
RETURNING id, username, COALESCE(password_hash,''), status, COALESCE(bootstrap_marker,''), created_at, approved_at
`, userID, now), &u); err != nil {
			return err
		}
		return s.AppendAuditEventTx(ctx, tx, domain.AuditEvent{
			ActorUserID: userID,
			Action:      domain.AuditUserApproved,
			TargetType:  "user",
			TargetID:    fmt.Sprintf("%d", userID),
		})
	})
	return u, err
}

// BlockUser transitions a user to 'blocked', cancels their queued tasks, marks
// running tasks for cancellation, and records audit events — all in one
// transaction (spec §5.3). Returns the affected running/starting tasks that
// need async worker cancellation.
func (s *Store) BlockUser(ctx context.Context, userID int64) (domain.User, []domain.Task, error) {
	if userID <= 0 {
		return domain.User{}, nil, fmt.Errorf("user id must be positive")
	}
	var u domain.User
	var affected []domain.Task
	err := s.withTx(ctx, func(tx pgx.Tx) error {
		if err := scanUser(tx.QueryRow(ctx, `
SELECT id, username, COALESCE(password_hash,''), status, COALESCE(bootstrap_marker,''), created_at, approved_at
FROM users WHERE id = $1 FOR UPDATE
`, userID), &u); err != nil {
			return err
		}
		if u.Status == domain.UserBlocked {
			return fmt.Errorf("user %d is already blocked", userID)
		}
		now := time.Now().UTC()
		if err := scanUser(tx.QueryRow(ctx, `
UPDATE users SET status = 'blocked', updated_at = $2
WHERE id = $1
RETURNING id, username, COALESCE(password_hash,''), status, COALESCE(bootstrap_marker,''), created_at, approved_at
`, userID, now), &u); err != nil {
			return err
		}
		// 直接终态化 queued 与未派发 starting 任务(round10 审查 B5): 未派发
		// starting 若只写 cancel_requested_at, dispatch 会因
		// "WorkerDispatchStartedAt==nil" 直接返回, 任务永久卡在 starting
		// 占住串行槽; queued 若用裸 UPDATE 也不取消 pending task_started
		// delivery(用户会收到"正在处理"却无后续)。统一走 finalizeTerminal:
		// 撤销已签发 JTI、写 status_transition、取消 task_started、清 claim。
		if err := cancelBlockedUserTasks(ctx, tx, userID); err != nil {
			return err
		}
		// Mark running/starting(已派发)任务 for cooperative cancellation。
		rows, err := tx.Query(ctx, `
UPDATE tasks
SET cancel_requested_at = $2, updated_at = $2
WHERE requester_user_id = $1 AND status IN `+activeTaskStatusesSQL+` AND cancel_requested_at IS NULL
RETURNING `+taskSelectColumns, userID, now)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			t, err := scanTask(rows)
			if err != nil {
				return err
			}
			affected = append(affected, t)
		}
		if err := rows.Err(); err != nil {
			return err
		}
		// 审查 D3(用户生命周期): 封禁必须立即撤销该用户全部登录会话——
		// 否则 blocked 用户可持旧 token 继续调用用户控制面 API。与状态
		// 变更同一事务提交, 失败即整体回滚(封禁不生效)。
		if _, err := tx.Exec(ctx, `DELETE FROM user_sessions WHERE user_id = $1`, userID); err != nil {
			return fmt.Errorf("revoke user sessions: %w", err)
		}
		return nil
	})
	if err != nil {
		return domain.User{}, nil, err
	}
	// Audit is appended outside the tx so a failed audit does not roll back
	// the safety-critical block. A failed audit is logged but not fatal.
	_ = s.AppendAuditEvent(ctx, domain.AuditEvent{
		ActorUserID: userID,
		Action:      domain.AuditUserBlocked,
		TargetType:  "user",
		TargetID:    fmt.Sprintf("%d", userID),
	})
	return u, affected, nil
}

// cancelBlockedUserTasks 终态化用户所有 queued 与未派发 starting 任务
// (round10 审查 B5): 复用 RemoveMember 的 cancelRemovedMemberTasks 语义
// (finalizeTerminal: 撤销 JTI、写事件、取消 pending task_started、清 claim),
// 跨该用户全部 workspace。已派发(starting + dispatch 已开始)任务由调用方
// 写 durable cancel_requested_at, 由 scheduler tick 驱动 Worker 取消。
func cancelBlockedUserTasks(ctx context.Context, tx pgx.Tx, userID int64) error {
	rows, err := tx.Query(ctx, `
SELECT `+taskSelectColumns+` FROM tasks
WHERE requester_user_id = $1
  AND ((status = 'queued') OR
       (status = 'starting' AND worker_dispatch_started_at IS NULL AND cancel_requested_at IS NULL))
FOR UPDATE
`, userID)
	if err != nil {
		return err
	}
	defer rows.Close()
	var list []domain.Task
	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			return err
		}
		list = append(list, t)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for _, t := range list {
		if _, err := finalizeTerminal(ctx, tx, t, domain.TaskCancelled, domain.DeliveryTaskCancelled,
			"TASK_CANCELLED", "user blocked", "", "", ""); err != nil {
			return err
		}
	}
	return nil
}

// ListPendingUsers returns all users with status 'pending'.
func (s *Store) ListPendingUsers(ctx context.Context) ([]domain.User, error) {
	rows, err := s.pool.Query(ctx, `
SELECT id, username, COALESCE(password_hash,''), status, COALESCE(bootstrap_marker,''), created_at, approved_at
FROM users WHERE status = 'pending' ORDER BY created_at
`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var users []domain.User
	for rows.Next() {
		var u domain.User
		if err := scanUser(rows, &u); err != nil {
			return nil, err
		}
		users = append(users, u)
	}
	return users, rows.Err()
}

// CountPendingUsers returns the count of users with status 'pending'.
func (s *Store) CountPendingUsers(ctx context.Context) (int, error) {
	var count int
	err := s.pool.QueryRow(ctx, `SELECT COUNT(*) FROM users WHERE status = 'pending'`).Scan(&count)
	return count, err
}

// CountApprovedUsers returns the count of users with status 'approved'.
func (s *Store) CountApprovedUsers(ctx context.Context) (int, error) {
	var count int
	err := s.pool.QueryRow(ctx, `SELECT COUNT(*) FROM users WHERE status = 'approved'`).Scan(&count)
	return count, err
}

func scanUser(row pgx.Row, u *domain.User) error {
	// 审查: password_hash 可空(admin 创建的测试账号无密码)——NULL 必须
	// 扫成空串而非报错, 否则 CreateUser("", "") 直接崩溃。
	return row.Scan(&u.ID, &u.Username, &u.PasswordHash, &u.Status, &u.BootstrapMarker, &u.CreatedAt, &u.ApprovedAt)
}

// EnsureUserExists returns nil if the user exists; otherwise pgx.ErrNoRows.
func (s *Store) EnsureUserExists(ctx context.Context, userID int64) error {
	var dummy int
	err := s.pool.QueryRow(ctx, `SELECT 1 FROM users WHERE id = $1`, userID).Scan(&dummy)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("user %d not found", userID)
	}
	return err
}

// FindRunningTaskBySession returns the single starting/running task for the
// given session_key, or pgx.ErrNoRows if none. Relies on the
// one_running_task_per_session unique index.
func (s *Store) FindRunningTaskBySession(ctx context.Context, sessionKey string) (domain.Task, error) {
	if sessionKey == "" {
		return domain.Task{}, fmt.Errorf("session key is required")
	}
	row := s.pool.QueryRow(ctx, `
SELECT `+taskSelectColumns+` FROM tasks
WHERE session_key = $1 AND status IN `+activeTaskStatusesSQL+`
`, sessionKey)
	return scanTask(row)
}
