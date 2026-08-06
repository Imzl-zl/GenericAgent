package postgres

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/domain"
	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/infrastructure/llmproxy"
)

// runningTaskLimitLockKey 是全局 running-task 上限 advisory lock 常量键
// (审查 D4): 所有 Platform 实例在同一 Postgres 上串行化计数+claim。
const runningTaskLimitLockKey = 0x4752544C // "GRTL"

// ClaimNextTask claims the oldest queued row for session when no starting/running exists.
func (s *Store) ClaimNextTask(ctx context.Context, sessionKey, platformInstanceID string, claimLease time.Duration) (domain.Task, bool, error) {
	if strings.TrimSpace(platformInstanceID) == "" {
		return domain.Task{}, false, fmt.Errorf("platform instance id is required")
	}
	if claimLease <= 0 {
		return domain.Task{}, false, fmt.Errorf("claim lease must be positive")
	}
	var task domain.Task
	var claimed bool
	err := s.withTx(ctx, func(tx pgx.Tx) error {
		// 审查 D4: 全局 running-task 上限的原子门禁。advisory lock 把所有
		// Platform 实例的 claim 容量检查串行化(仅 limit>0 时启用), 计数+
		// claim 在同一事务内完成——两个实例同时观察到 limit-1 时只有一个
		// 能通过, 不会超卖。锁在 workspace 行锁之前获取, 顺序固定, 不会
		// 形成锁序环。scheduler 侧的 MaxRunningTasks 预检查保留作快速
		// 拒绝, 此事务内检查才是跨实例硬门禁。
		if s.runningTaskLimit > 0 {
			if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, runningTaskLimitLockKey); err != nil {
				return fmt.Errorf("acquire running task limit lock: %w", err)
			}
			var running int
			if err := tx.QueryRow(ctx, `
SELECT COUNT(*) FROM tasks WHERE status IN ('starting','running')
`).Scan(&running); err != nil {
				return err
			}
			if running >= s.runningTaskLimit {
				return nil // 上限已满, 不 claim(claimed=false)
			}
		}
		// Session lock via workspace row.
		var workspaceID string
		err := tx.QueryRow(ctx, `
SELECT id FROM workspaces WHERE session_key = $1 FOR UPDATE
`, sessionKey).Scan(&workspaceID)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return nil
			}
			return err
		}
		var busy int
		if err := tx.QueryRow(ctx, `
SELECT COUNT(*) FROM tasks
WHERE session_key = $1 AND status IN ('starting','running')
`, sessionKey).Scan(&busy); err != nil {
			return err
		}
		if busy > 0 {
			return nil
		}
		leaseMicros := claimLease.Microseconds()
		if leaseMicros <= 0 {
			leaseMicros = 1
		}
		row := tx.QueryRow(ctx, `
UPDATE tasks SET
  status = 'starting',
  claim_owner = $2,
  claimed_at = timezone('utc', now()),
	  claim_lease_until = timezone('utc', now()) + $3 * interval '1 microsecond',
  updated_at = timezone('utc', now()),
  started_at = COALESCE(started_at, timezone('utc', now()))
WHERE id = (
  SELECT id FROM tasks
  WHERE session_key = $1 AND status = 'queued'
  ORDER BY session_sequence ASC
  FOR UPDATE SKIP LOCKED
  LIMIT 1
)
RETURNING `+taskSelectColumns, sessionKey, platformInstanceID, leaseMicros)
		t, err := scanTask(row)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		if err := insertNextEvent(ctx, tx, t.ID, "status_transition", nil, nil, "queued", "starting", "", ""); err != nil {
			return err
		}
		task = t
		claimed = true
		return nil
	})
	return task, claimed, err
}

// HeartbeatClaim extends claim_lease_until for a current-owner starting/running task.
// Returns application.ErrLeaseExpired when 0 rows matched (the caller no longer
// owns the task — lease expired or was stolen by RecoverAfterRestart). Other
// errors indicate DB connectivity issues and should be retried next tick.
func (s *Store) HeartbeatClaim(ctx context.Context, taskID, platformInstanceID string, claimLease time.Duration) error {
	if claimLease <= 0 {
		return fmt.Errorf("claim lease must be positive")
	}
	leaseMicros := claimLease.Microseconds()
	if leaseMicros <= 0 {
		leaseMicros = 1
	}
	tag, err := s.pool.Exec(ctx, `
UPDATE tasks SET
  claim_lease_until = timezone('utc', now()) + $3 * interval '1 microsecond',
  updated_at = timezone('utc', now())
WHERE id = $1 AND claim_owner = $2 AND status IN ('starting','running')
  AND claim_lease_until > timezone('utc', now())
`, taskID, platformInstanceID, leaseMicros)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return domain.ErrLeaseExpired
	}
	return nil
}

// RequeueTask 把本实例 claim 的 starting 任务退回 queued 并清空 claim 字段。
// 幂等: 非 starting 或 claim_owner 不匹配时返回 nil(任务已被其他路径处理)。
func (s *Store) RequeueTask(ctx context.Context, taskID, platformInstanceID string) error {
	if strings.TrimSpace(platformInstanceID) == "" {
		return fmt.Errorf("platform instance id is required")
	}
	return s.withTx(ctx, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `
SELECT `+taskSelectColumns+` FROM tasks WHERE id = $1 FOR UPDATE
`, taskID)
		t, err := scanTask(row)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return nil
			}
			return err
		}
		if t.Status != domain.TaskStarting || t.ClaimOwner != platformInstanceID {
			return nil
		}
		if _, err := tx.Exec(ctx, `
UPDATE tasks SET
  status = 'queued',
  claim_owner = NULL,
  claimed_at = NULL,
  claim_lease_until = NULL,
  updated_at = timezone('utc', now())
WHERE id = $1
`, taskID); err != nil {
			return err
		}
		return insertNextEvent(ctx, tx, taskID, "status_transition", nil, nil, "starting", "queued", "", "")
	})
}

// MarkDispatchStarted records worker_instance_id and worker_dispatch_started_at
// before Worker RPC. freshSession 是 dispatch 实时判定的 /new 消费标记
// (审查 F2: 与 reset_at 的实时状态一致, 不再使用提交时的陈旧快照)。
func (s *Store) MarkDispatchStarted(ctx context.Context, taskID, platformInstanceID, workerInstanceID string, freshSession bool) (domain.Task, error) {
	var task domain.Task
	err := s.withTx(ctx, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `
SELECT `+taskSelectColumns+` FROM tasks WHERE id = $1 FOR UPDATE
`, taskID)
		t, err := scanTask(row)
		if err != nil {
			return err
		}
		if t.Status != domain.TaskStarting {
			return fmt.Errorf("task %s not starting (status=%s)", taskID, t.Status)
		}
		if t.ClaimOwner != platformInstanceID {
			return fmt.Errorf("task %s claim owner mismatch", taskID)
		}
		// round9 审查: 已派发但 claim lease 已过期(进程暂停/心跳丢失后恢复)
		// 的任务不得继续派发——旧 owner 必须先经 RecoverAfterRestart/新 owner
		// 接管, 否则可能与接管者重叠执行。
		if t.ClaimLeaseUntil.IsZero() || !t.ClaimLeaseUntil.After(time.Now().UTC()) {
			return fmt.Errorf("task %s claim lease expired before dispatch", taskID)
		}
		if t.CancelRequestedAt != nil {
			return fmt.Errorf("task %s cancel already requested before dispatch", taskID)
		}
		row = tx.QueryRow(ctx, `
UPDATE tasks SET
  worker_instance_id = $2,
  worker_dispatch_started_at = timezone('utc', now()),
  fresh_session = $4,
  updated_at = timezone('utc', now())
WHERE id = $1 AND status = 'starting' AND claim_owner = $3
  AND worker_dispatch_started_at IS NULL
  AND claim_lease_until > timezone('utc', now())
RETURNING `+taskSelectColumns, taskID, workerInstanceID, platformInstanceID, freshSession)
		t, err = scanTask(row)
		if err != nil {
			return err
		}
		if err := insertNextEvent(ctx, tx, t.ID, "dispatch", nil, nil, "", "", workerInstanceID, ""); err != nil {
			return err
		}
		task = t
		return nil
	})
	return task, err
}

// MarkRunning advances starting -> running after Worker acceptance. If the
// row no longer matches (cancelled, lease lost, or already terminal), the
// UPDATE returns zero rows; we re-read the current state so callers can
// distinguish "task was cancelled" from "task not found".
// round9 审查: lease 已过期/owner 已变/状态异常(非终态、未取消)必须返回
// 错误——调度器据此中止派发并销毁 Worker, 否则旧 owner 会继续 ExecuteTask
// 与接管者重叠执行。
func (s *Store) MarkRunning(ctx context.Context, taskID, platformInstanceID string) (domain.Task, error) {
	var task domain.Task
	err := s.withTx(ctx, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `
UPDATE tasks SET status = 'running', updated_at = timezone('utc', now())
WHERE id = $1 AND claim_owner = $2 AND status = 'starting'
  AND worker_dispatch_started_at IS NOT NULL
  AND cancel_requested_at IS NULL
  AND claim_lease_until > timezone('utc', now())
RETURNING `+taskSelectColumns, taskID, platformInstanceID)
		t, err := scanTask(row)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				// UPDATE matched nothing; re-read to surface the real reason
				// (cancelled, lease lost, state changed) instead of ErrNoRows.
				r2 := tx.QueryRow(ctx, `SELECT `+taskSelectColumns+` FROM tasks WHERE id = $1`, taskID)
				t2, err2 := scanTask(r2)
				if err2 != nil {
					return err2
				}
				if t2.Status.IsTerminal() || t2.CancelRequestedAt != nil {
					// 已终态或已请求取消: 属预期竞争, 返回当前行让调用方决定。
					task = t2
					return nil
				}
				return fmt.Errorf("task %s no longer dispatchable under %s (status=%s, lease expired or owner changed)",
					taskID, platformInstanceID, t2.Status)
			}
			return err
		}
		if err := insertNextEvent(ctx, tx, t.ID, "status_transition", nil, nil, "starting", "running", t.WorkerInstanceID, ""); err != nil {
			return err
		}
		task = t
		return nil
	})
	return task, err
}

// RecordChunkEvent stores bounded chunk metadata only (no text).
// Also updates last_activity_at for idle/deadlock detection (reaper).
// No-op when the task is already terminal: avoids inserting chunk events and
// refreshing last_activity_at on cancelled/completed tasks.
func (s *Store) RecordChunkEvent(ctx context.Context, taskID string, byteCount int, digest string) error {
	return s.withTx(ctx, func(tx pgx.Tx) error {
		var lockedTaskID string
		if err := tx.QueryRow(ctx, `SELECT id FROM tasks WHERE id=$1 AND status IN ('starting','running') FOR UPDATE`, taskID).Scan(&lockedTaskID); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return nil
			}
			return err
		}
		bc := byteCount
		if err := insertNextEvent(ctx, tx, lockedTaskID, "chunk", &bc, strPtr(digest), "", "", "", ""); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `UPDATE tasks SET last_activity_at = timezone('utc', now()), updated_at = timezone('utc', now()) WHERE id = $1`, lockedTaskID)
		return err
	})
}

// RecordHeartbeat updates last_activity_at without writing a chunk event.
// Called when Worker drain_display_queue polls empty (LLM still thinking).
func (s *Store) RecordHeartbeat(ctx context.Context, taskID string) error {
	_, err := s.pool.Exec(ctx, `UPDATE tasks SET last_activity_at = timezone('utc', now()) WHERE id = $1 AND status IN ('starting','running')`, taskID)
	return err
}

// CancelTask applies durable cancel decision under row lock.
// Returns the task and whether a Worker cancel RPC should be issued.
func (s *Store) CancelTask(ctx context.Context, taskID string, requesterUserID int64) (domain.Task, bool, error) {
	var task domain.Task
	var needWorker bool
	err := s.withTx(ctx, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `
SELECT `+taskSelectColumns+` FROM tasks WHERE id = $1 FOR UPDATE
`, taskID)
		t, err := scanTask(row)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("task not found: %s", taskID)
			}
			return err
		}
		// 审查 I-4: 取消者必须为任务归属者(RequesterID), requester 必传不可
		// 绕过(原 requesterUserID != 0 条件允许 0 值跳过校验)。
		if requesterUserID <= 0 || t.RequesterID != requesterUserID {
			return fmt.Errorf("requester %d cannot cancel task owned by %d", requesterUserID, t.RequesterID)
		}
		if t.Status.IsTerminal() {
			task = t
			return nil
		}
		switch t.Status {
		case domain.TaskQueued:
			tt, err := finalizeTerminal(ctx, tx, t, domain.TaskCancelled, domain.DeliveryTaskCancelled, "TASK_CANCELLED", "task cancelled before dispatch", "", "", "")
			if err != nil {
				return err
			}
			task = tt
			return nil
		case domain.TaskStarting:
			if t.WorkerDispatchStartedAt == nil {
				tt, err := finalizeTerminal(ctx, tx, t, domain.TaskCancelled, domain.DeliveryTaskCancelled, "TASK_CANCELLED", "task cancelled before worker dispatch", "", "", "")
				if err != nil {
					return err
				}
				task = tt
				return nil
			}
			// Already dispatching: record cancel request once.
			if t.CancelRequestedAt == nil {
				row = tx.QueryRow(ctx, `
UPDATE tasks SET cancel_requested_at = timezone('utc', now()), updated_at = timezone('utc', now())
WHERE id = $1 AND cancel_requested_at IS NULL
RETURNING `+taskSelectColumns, taskID)
				t, err = scanTask(row)
				if err != nil {
					return err
				}
				if err := insertNextEvent(ctx, tx, t.ID, "cancel_request", nil, nil, string(t.Status), string(t.Status), t.WorkerInstanceID, "TASK_CANCELLED"); err != nil {
					return err
				}
			}
			task = t
			needWorker = true
			return nil
		case domain.TaskRunning:
			if t.CancelRequestedAt == nil {
				row = tx.QueryRow(ctx, `
UPDATE tasks SET cancel_requested_at = timezone('utc', now()), updated_at = timezone('utc', now())
WHERE id = $1 AND cancel_requested_at IS NULL
RETURNING `+taskSelectColumns, taskID)
				t, err = scanTask(row)
				if err != nil {
					return err
				}
				if err := insertNextEvent(ctx, tx, t.ID, "cancel_request", nil, nil, string(t.Status), string(t.Status), t.WorkerInstanceID, "TASK_CANCELLED"); err != nil {
					return err
				}
			}
			task = t
			needWorker = true
			return nil
		default:
			return fmt.Errorf("unexpected status %s", t.Status)
		}
	})
	return task, needWorker, err
}

// RecoverAfterRestart interrupts expired starting/running rows whose claim
// lease has lapsed. Both foreign owners (multi-instance failover) and self
// (single-instance restart) are recovered: after a restart the prior lease
// is stale even if claim_owner matches this instance, so excluding self would
// leave those rows stuck forever.
func (s *Store) RecoverAfterRestart(ctx context.Context, platformInstanceID string) (int, error) {
	if strings.TrimSpace(platformInstanceID) == "" {
		return 0, fmt.Errorf("platform instance id is required")
	}
	var n int
	err := s.withTx(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
SELECT `+taskSelectColumns+` FROM tasks
WHERE status IN ('starting','running')
  AND claim_owner IS NOT NULL
  AND claim_lease_until IS NOT NULL
  AND claim_lease_until < timezone('utc', now())
FOR UPDATE
`)
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
			if t.Status == domain.TaskStarting && t.WorkerDispatchStartedAt == nil {
				// 未派发(claim 后、MarkDispatchStarted 前崩溃/lease 过期): 任务
				// 从未交给 Worker 执行。即使已签发 capability(签发先于
				// MarkDispatchStarted, 审查 F1), 也先撤销 JTI 再退回 queued,
				// 由下一轮 tick 重新 claim——容量满载等瞬时窗口不得把任务
				// 误判为 interrupted(审查 F4: 满载保持 queued)。
				if err := revokeTaskCapabilityJTIs(ctx, tx, t.ID); err != nil {
					return err
				}
				if err := requeueExpiredTask(ctx, tx, t); err != nil {
					return err
				}
				n++
				continue
			}
			// 已派发/运行中: 崩溃恢复撤销(审查): 中断任务的 capability JTI 在
			// 同一事务内写入撤销表, 使旧 token 立即失效——恢复路径不能依赖
			// 进程内重试(本实例崩溃期间已签发的 token 无人撤销, 旧 Runner
			// 容器在 TTL 内仍可用)。
			if err := revokeTaskCapabilityJTIs(ctx, tx, t.ID); err != nil {
				return err
			}
			if _, err := finalizeTerminal(ctx, tx, t, domain.TaskInterrupted, domain.DeliveryTaskInterrupted,
				"TASK_INTERRUPTED", "prior claim owner lease expired", "", "", ""); err != nil {
				return err
			}
			n++
		}
		return nil
	})
	return n, err
}

// requeueExpiredTask 把 lease 过期且未派发的 starting 任务退回 queued 并
// 清空 claim 字段(审查 F4)。调用方必须已持任务行锁(RecoverAfterRestart
// 的事务内)。
func requeueExpiredTask(ctx context.Context, tx pgx.Tx, t domain.Task) error {
	if _, err := tx.Exec(ctx, `
UPDATE tasks SET
  status = 'queued',
  claim_owner = NULL,
  claimed_at = NULL,
  claim_lease_until = NULL,
  updated_at = timezone('utc', now())
WHERE id = $1 AND status = 'starting'
`, t.ID); err != nil {
		return fmt.Errorf("requeue expired task %s: %w", t.ID, err)
	}
	return insertNextEvent(ctx, tx, t.ID, "status_transition", nil, nil, "starting", "queued", t.WorkerInstanceID, "")
}

// revokeTaskCapabilityJTIs 在调用方事务内把任务的 capability JTI 全部写入
// 撤销表(幂等: ON CONFLICT 取最晚过期)。无 JTI(loopback/未签发)时无操作。
func revokeTaskCapabilityJTIs(ctx context.Context, tx pgx.Tx, taskID string) error {
	var jtis []string
	if err := tx.QueryRow(ctx, `SELECT capability_jtis FROM tasks WHERE id = $1`, taskID).Scan(&jtis); err != nil {
		return fmt.Errorf("load task capability jtis: %w", err)
	}
	expiresAt := time.Now().UTC().Add(recoveryRevocationTTL)
	for _, jti := range jtis {
		if jti == "" {
			continue
		}
		digest := llmproxy.HashJTI(jti)
		if _, err := tx.Exec(ctx, `
INSERT INTO llm_capability_revocations (jti_hash, expires_at)
VALUES ($1, $2)
ON CONFLICT (jti_hash) DO UPDATE
SET expires_at = GREATEST(llm_capability_revocations.expires_at, EXCLUDED.expires_at)
`, digest[:], expiresAt); err != nil {
			return fmt.Errorf("revoke capability jti on recovery: %w", err)
		}
		// 计量行随撤销一并清理(审查 R4-I9): 防止 capability_usage 无界增长。
		if _, err := tx.Exec(ctx, `DELETE FROM capability_usage WHERE jti_hash = $1`, digest[:]); err != nil {
			return fmt.Errorf("delete capability usage on recovery: %w", err)
		}
	}
	return nil
}
