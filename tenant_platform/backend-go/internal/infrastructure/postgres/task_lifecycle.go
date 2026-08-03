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

// MarkDispatchStarted records worker_instance_id and worker_dispatch_started_at before Worker RPC.
func (s *Store) MarkDispatchStarted(ctx context.Context, taskID, platformInstanceID, workerInstanceID string) (domain.Task, error) {
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
		if t.CancelRequestedAt != nil {
			return fmt.Errorf("task %s cancel already requested before dispatch", taskID)
		}
		row = tx.QueryRow(ctx, `
UPDATE tasks SET
  worker_instance_id = $2,
  worker_dispatch_started_at = timezone('utc', now()),
  updated_at = timezone('utc', now())
WHERE id = $1 AND status = 'starting' AND claim_owner = $3
  AND worker_dispatch_started_at IS NULL
RETURNING `+taskSelectColumns, taskID, workerInstanceID, platformInstanceID)
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
func (s *Store) MarkRunning(ctx context.Context, taskID, platformInstanceID string) (domain.Task, error) {
	var task domain.Task
	err := s.withTx(ctx, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `
UPDATE tasks SET status = 'running', updated_at = timezone('utc', now())
WHERE id = $1 AND claim_owner = $2 AND status = 'starting'
  AND worker_dispatch_started_at IS NOT NULL
  AND cancel_requested_at IS NULL
RETURNING `+taskSelectColumns, taskID, platformInstanceID)
		t, err := scanTask(row)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				// UPDATE matched nothing; re-read to surface the real reason
				// (cancelled, lease lost, state changed) instead of ErrNoRows.
				r2 := tx.QueryRow(ctx, `SELECT `+taskSelectColumns+` FROM tasks WHERE id = $1`, taskID)
				t2, err2 := scanTask(r2)
				if err2 != nil {
					return err
				}
				task = t2
				return nil
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
		if requesterUserID != 0 && t.RequesterID != requesterUserID {
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
			// 崩溃恢复撤销(审查): 中断任务的 capability JTI 在同一事务内写入
			// 撤销表, 使旧 token 立即失效——恢复路径不能依赖进程内重试(本实例
			// 崩溃期间已签发的 token 无人撤销, 旧 Runner 容器在 TTL 内仍可用)。
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
