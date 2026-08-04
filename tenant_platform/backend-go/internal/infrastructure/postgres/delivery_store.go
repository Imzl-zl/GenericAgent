package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/domain"
)

// ClaimPendingDeliveries locks up to limit deliverable outbox rows and marks
// them sending. It skips rows whose retry window (terminal_at + retryWindow)
// has already expired; those are left for the caller to dead-letter.
func (s *Store) ClaimPendingDeliveries(ctx context.Context, limit int, lease time.Duration, retryWindow time.Duration, now time.Time) ([]domain.Delivery, error) {
	if limit <= 0 {
		limit = 8
	}
	leaseUntil := now.Add(lease)
	rows, err := s.pool.Query(ctx, `
WITH eligible AS (
    SELECT d.delivery_id, d.attempt_count, t.terminal_at
    FROM task_deliveries d
    JOIN tasks t ON t.id = d.task_id
    WHERE d.status IN ('pending','sending')
      AND (d.attempt_lease_until IS NULL OR d.attempt_lease_until <= $1)
      AND (d.next_attempt_at IS NULL OR d.next_attempt_at <= $1)
      AND (
          t.terminal_at IS NULL
          OR $1 < t.terminal_at + $4::interval
      )
      -- round10 审查(B8): 同一 task 的 task_started 未完成(pending/sending)
      -- 时不得 claim 其他 delivery——否则并发发送会让完成消息先于"正在处理"
      -- 送达。task_started 自身不受限(否则永远无法发送); 终态事务已把
      -- pending 的 task_started 置 cancelled(task_helpers.go), 不会卡死。
      AND (
          d.delivery_type = 'task_started'
          OR NOT EXISTS (
              SELECT 1 FROM task_deliveries prior
              WHERE prior.task_id = d.task_id
                AND prior.delivery_type = 'task_started'
                AND prior.status IN ('pending','sending')
          )
      )
    ORDER BY d.created_at
    FOR UPDATE OF d SKIP LOCKED
    LIMIT $2
)
UPDATE task_deliveries d SET
    status = 'sending',
    attempt_count = e.attempt_count + 1,
    attempt_lease_until = $3,
    sent_at = $1,
    updated_at = $1
FROM eligible e
WHERE d.delivery_id = e.delivery_id
RETURNING d.delivery_id, d.task_id, d.delivery_type, d.status,
          COALESCE(d.payload_ref,''), COALESCE(d.payload_digest,''),
          COALESCE(d.error_code,''), COALESCE(d.error_message,''), d.attempt_count
`, now, limit, leaseUntil, retryWindow.String())
	if err != nil {
		return nil, fmt.Errorf("claim deliveries: %w", err)
	}
	defer rows.Close()
	return scanDeliveries(rows)
}

// ResetStaleSendingDeliveries returns expired sending rows to pending so a
// crashed or restarted platform does not leave deliveries stuck.
func (s *Store) ResetStaleSendingDeliveries(ctx context.Context, now time.Time) (int64, error) {
	tag, err := s.pool.Exec(ctx, `
UPDATE task_deliveries
SET status = 'pending', attempt_lease_until = NULL, updated_at = $1
WHERE status = 'sending' AND attempt_lease_until <= $1
`, now)
	if err != nil {
		return 0, fmt.Errorf("reset stale sending: %w", err)
	}
	return tag.RowsAffected(), nil
}

// DeadLetterExpiredDeliveries moves pending/sending rows whose retry window
// (terminal_at + retryWindow) has expired to dead_letter.
func (s *Store) DeadLetterExpiredDeliveries(ctx context.Context, retryWindow time.Duration, now time.Time) (int64, error) {
	tag, err := s.pool.Exec(ctx, `
UPDATE task_deliveries d SET
    status = 'dead_letter',
    terminal_at = $1,
    updated_at = $1
FROM tasks t
WHERE d.task_id = t.id
  AND d.status IN ('pending','sending')
  AND t.terminal_at IS NOT NULL
  AND $1 >= t.terminal_at + $2::interval
`, now, retryWindow.String())
	if err != nil {
		return 0, fmt.Errorf("dead letter expired deliveries: %w", err)
	}
	return tag.RowsAffected(), nil
}

// LoadDeliveryFiles 返回 task_complete delivery 绑定的输出文件快照
// (审查 R5-I3): 内容在任务成功事务时捕获, 发送时不再解析 workspace 路径。
func (s *Store) LoadDeliveryFiles(ctx context.Context, deliveryID string) ([]domain.DeliveryFile, error) {
	rows, err := s.pool.Query(ctx, `
SELECT marker, file_name, rel_path, content, digest, size_bytes
FROM task_delivery_files
WHERE delivery_id = $1
ORDER BY marker
`, deliveryID)
	if err != nil {
		return nil, fmt.Errorf("load delivery files: %w", err)
	}
	defer rows.Close()
	var out []domain.DeliveryFile
	for rows.Next() {
		var f domain.DeliveryFile
		if err := rows.Scan(&f.Marker, &f.FileName, &f.RelPath, &f.Content, &f.Digest, &f.SizeBytes); err != nil {
			return nil, fmt.Errorf("scan delivery file: %w", err)
		}
		out = append(out, f)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// DeleteExpiredDeliveryFiles 删除超过保留期的 delivery 文件快照
// (审查 R5-I3: 快照随 outbox 审计保留, 定期清理防无界增长)。
func (s *Store) DeleteExpiredDeliveryFiles(ctx context.Context, before time.Time) (int64, error) {
	tag, err := s.pool.Exec(ctx, `
DELETE FROM task_delivery_files WHERE created_at < $1
`, before.UTC())
	if err != nil {
		return 0, fmt.Errorf("delete expired delivery files: %w", err)
	}
	return tag.RowsAffected(), nil
}

// MarkDeliveryAcked records that the carrier accepted the message. Only
// pending/sending rows can transition to acked; if the row is already acked
// or dead_letter, the UPDATE matches zero rows and this is a no-op (idempotent).
func (s *Store) MarkDeliveryAcked(ctx context.Context, deliveryID string, ackedAt time.Time) error {
	_, err := s.pool.Exec(ctx, `
UPDATE task_deliveries
SET status = 'acked', acked_at = $2, updated_at = $2
WHERE delivery_id = $1 AND status IN ('pending','sending')
`, deliveryID, ackedAt)
	return err
}

// MarkDeliveryRetry returns a failed sending delivery to pending for a future
// attempt. Only rows still in 'sending' can retry; acked/dead_letter rows are
// left untouched.
func (s *Store) MarkDeliveryRetry(ctx context.Context, deliveryID string, nextAttemptAt time.Time, now time.Time) error {
	_, err := s.pool.Exec(ctx, `
UPDATE task_deliveries
SET status = 'pending', next_attempt_at = $2, attempt_lease_until = NULL, updated_at = $3
WHERE delivery_id = $1 AND status = 'sending'
`, deliveryID, nextAttemptAt, now)
	return err
}

// MarkDeliveryDeadLetter moves a delivery to the dead-letter state. Only
// pending/sending rows can be dead-lettered; already-acked rows are left
// untouched (carrier already accepted the message).
func (s *Store) MarkDeliveryDeadLetter(ctx context.Context, deliveryID string, errCode, errMessage string, terminalAt time.Time) error {
	_, err := s.pool.Exec(ctx, `
UPDATE task_deliveries
SET status = 'dead_letter', error_code = $2, error_message = $3,
    terminal_at = $4, updated_at = $4
WHERE delivery_id = $1 AND status IN ('pending','sending')
`, deliveryID, errCode, errMessage, terminalAt)
	return err
}

// GetDelivery loads a delivery by task and type.
func (s *Store) GetDelivery(ctx context.Context, taskID string, dt domain.DeliveryType) (domain.Delivery, error) {
	var d domain.Delivery
	err := s.pool.QueryRow(ctx, `
SELECT delivery_id, task_id, delivery_type, status,
       COALESCE(payload_ref,''), COALESCE(payload_digest,''),
       COALESCE(error_code,''), COALESCE(error_message,''), attempt_count
FROM task_deliveries WHERE task_id = $1 AND delivery_type = $2
`, taskID, string(dt)).Scan(
		&d.DeliveryID, &d.TaskID, &d.DeliveryType, &d.Status,
		&d.PayloadRef, &d.PayloadDigest, &d.ErrorCode, &d.ErrorMessage, &d.AttemptCount,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Delivery{}, fmt.Errorf("delivery not found")
	}
	return d, err
}

func scanDeliveries(rows pgx.Rows) ([]domain.Delivery, error) {
	var out []domain.Delivery
	for rows.Next() {
		var d domain.Delivery
		err := rows.Scan(
			&d.DeliveryID, &d.TaskID, &d.DeliveryType, &d.Status,
			&d.PayloadRef, &d.PayloadDigest, &d.ErrorCode, &d.ErrorMessage, &d.AttemptCount,
		)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}
