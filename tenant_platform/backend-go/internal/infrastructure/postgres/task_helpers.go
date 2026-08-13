package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/domain"
)

const taskSelectColumns = `
id, workspace_id::text, session_key, session_sequence, requester_user_id,
source, source_instance_id, message_id, message_idempotency_key, conversation_key,
conversation_type, stream_final_at,
prompt, persona_snapshot, tool_policy_version, prompt_bytes, persona_bytes,
COALESCE(media::text,'[]'),
status, COALESCE(claim_owner,''), claim_lease_until,
COALESCE(worker_instance_id,''), worker_dispatch_started_at, cancel_requested_at,
snapshot_id::text, COALESCE(snapshot_checksum,''), COALESCE(result_ref,''), COALESCE(result_digest,''),
COALESCE(terminal_error_code,''), COALESCE(terminal_error_message,''), COALESCE(terminal_error_trace_id,''),
created_at, updated_at, started_at, succeeded_at, terminal_at, last_activity_at, fresh_session
`

// activeTaskStatusesSQL 是"任务处于活跃执行状态"的状态值列表(审查 C1 收敛):
// 容量门禁/claim 判定/终态转移/对账等多处共用, 新增活跃状态或调整语义
// 只改此处。状态枚举真值见 domain.TaskStatus 常量。多表查询(如 tasks 自
// 连接、runner_leases 联查)必须显式限定表别名, 否则 status 列歧义。
const activeTaskStatusesSQL = "('" + string(domain.TaskStarting) + "','" + string(domain.TaskRunning) + "')"

// activeClaimLeaseSQL 是 claim lease 未过期的谓词片段(审查 C1 收敛;
// round9: 时钟统一用 DB 时钟, 与 claim_lease_until 同源)。
const activeClaimLeaseSQL = "claim_lease_until > timezone('utc', now())"

type scannable interface {
	Scan(dest ...any) error
}

func scanTask(row scannable) (domain.Task, error) {
	var t domain.Task
	var personaRaw []byte
	var mediaRaw []byte
	var leaseUntil *time.Time
	var snapshotID *string
	var dispatchAt, cancelAt, startedAt, succeededAt, terminalAt *time.Time
	err := row.Scan(
		&t.ID, &t.WorkspaceID, &t.SessionKey, &t.SessionSequence, &t.RequesterID,
		&t.Source, &t.SourceInstanceID, &t.MessageID, &t.MessageIdempotencyKey,
		&t.ConversationKey, &t.ConversationType, &t.StreamFinalAt,
		&t.Prompt, &personaRaw, &t.ToolPolicyVersion, &t.PromptBytes, &t.PersonaBytes,
		&mediaRaw,
		&t.Status, &t.ClaimOwner, &leaseUntil,
		&t.WorkerInstanceID, &dispatchAt, &cancelAt,
		&snapshotID, &t.SnapshotChecksum, &t.ResultRef, &t.ResultDigest,
		&t.TerminalErrorCode, &t.TerminalErrorMessage, &t.TerminalErrorTraceID,
		&t.CreatedAt, &t.UpdatedAt, &startedAt, &succeededAt, &terminalAt, &t.LastActivityAt, &t.FreshSession,
	)
	if err != nil {
		return domain.Task{}, err
	}
	if leaseUntil != nil {
		t.ClaimLeaseUntil = leaseUntil.UTC()
	}
	if snapshotID != nil {
		t.SnapshotID = *snapshotID
	}
	t.WorkerDispatchStartedAt = dispatchAt
	t.CancelRequestedAt = cancelAt
	t.StartedAt = startedAt
	t.SucceededAt = succeededAt
	t.TerminalAt = terminalAt
	if len(personaRaw) > 0 {
		if err := json.Unmarshal(personaRaw, &t.PersonaSnapshot); err != nil {
			return domain.Task{}, fmt.Errorf("persona_snapshot: %w", err)
		}
	}
	if len(mediaRaw) > 0 && string(mediaRaw) != "[]" {
		if err := json.Unmarshal(mediaRaw, &t.Media); err != nil {
			return domain.Task{}, fmt.Errorf("task media: %w", err)
		}
	}
	if t.PersonaSnapshot == nil {
		t.PersonaSnapshot = []string{}
	}
	return t, nil
}

func finalizeTerminal(
	ctx context.Context,
	tx pgx.Tx,
	t domain.Task,
	status domain.TaskStatus,
	deliveryType domain.DeliveryType,
	code, message, resultRef, resultDigest, traceID string,
) (domain.Task, error) {
	if !status.IsTerminal() {
		return domain.Task{}, fmt.Errorf("not terminal: %s", status)
	}
	if len(message) > domain.MaxTerminalErrorBytes {
		message = message[:domain.MaxTerminalErrorBytes]
	}
	row := tx.QueryRow(ctx, `
UPDATE tasks SET
  status = $2,
  claim_owner = NULL,
  claim_lease_until = NULL,
  claimed_at = NULL,
  terminal_error_code = NULLIF($3,''),
  terminal_error_message = NULLIF($4,''),
  terminal_error_trace_id = NULLIF($5,''),
  result_ref = COALESCE(NULLIF($6,''), result_ref),
  result_digest = COALESCE(NULLIF($7,''), result_digest),
  terminal_at = timezone('utc', now()),
  updated_at = timezone('utc', now())
WHERE id = $1
RETURNING `+taskSelectColumns, t.ID, string(status), code, message, traceID, resultRef, resultDigest)
	tt, err := scanTask(row)
	if err != nil {
		return domain.Task{}, err
	}
	if err := insertNextEvent(ctx, tx, tt.ID, "status_transition", nil, nil, string(t.Status), string(status), tt.WorkerInstanceID, code); err != nil {
		return domain.Task{}, err
	}
	// 审查 R5-M2: 终态事务取消尚未发送的 task_started(pending), 防止重试
	// 恢复后用户先见完成消息、后见"正在处理"。sending 中的无法撤回(发送
	// 中消息顺序正常), 保持现状。cancelled 不参与 claim/重试/死信。
	if _, err := tx.Exec(ctx, `
UPDATE task_deliveries SET status = 'cancelled', updated_at = timezone('utc', now())
WHERE task_id = $1 AND delivery_type = 'task_started' AND status = 'pending'
`, tt.ID); err != nil {
		return domain.Task{}, err
	}
	if err := insertDelivery(ctx, tx, tt.ID, deliveryType, resultRef, resultDigest, code, message, traceID); err != nil {
		return domain.Task{}, err
	}
	// 审查 R4-C3: 终态事务内同步撤销任务的 capability JTI(与任务状态变更
	// 原子), 不依赖事务后的进程内重试——Platform 在终态提交后立即崩溃时
	// 旧 token 也已失效。幂等: 撤销表 ON CONFLICT 取最晚过期。
	if err := revokeTaskCapabilityJTIs(ctx, tx, tt.ID); err != nil {
		return domain.Task{}, err
	}
	return tt, nil
}

func insertDelivery(ctx context.Context, tx pgx.Tx, taskID string, dt domain.DeliveryType, ref, digest, code, message, traceID string) error {
	id := domain.StableDeliveryID(taskID, dt)
	// task_started and task_complete don't have error fields per schema constraint
	if dt == domain.DeliveryTaskStarted || dt == domain.DeliveryTaskComplete {
		_, err := tx.Exec(ctx, `
INSERT INTO task_deliveries (
  delivery_id, task_id, delivery_type, status,
  payload_ref, payload_digest
) VALUES ($1,$2,$3,'pending',$4,$5)
ON CONFLICT (task_id, delivery_type) DO NOTHING
`, id, taskID, string(dt), nullIfEmpty(ref), nullIfEmpty(digest))
		return err
	}
	// error deliveries (failed/cancelled/interrupted) require error fields
	_, err := tx.Exec(ctx, `
INSERT INTO task_deliveries (
  delivery_id, task_id, delivery_type, status,
  payload_ref, payload_digest, error_code, error_message, error_trace_id
) VALUES ($1,$2,$3,'pending',$4,$5,$6,$7,$8)
ON CONFLICT (task_id, delivery_type) DO NOTHING
`, id, taskID, string(dt), nullIfEmpty(ref), nullIfEmpty(digest), nullIfEmpty(code), nullIfEmpty(message), nullIfEmpty(traceID))
	return err
}

func insertEvent(ctx context.Context, tx pgx.Tx, taskID, eventType string, seq int64, byteCount *int, digest *string, fromStatus, toStatus, worker, errCode string) error {
	_, err := tx.Exec(ctx, `
INSERT INTO task_events (
  task_id, event_type, sequence_no, byte_count, digest, from_status, to_status, worker_instance, error_code
) VALUES ($1,$2,$3,$4,$5,NULLIF($6,''),NULLIF($7,''),NULLIF($8,''),NULLIF($9,''))
`, taskID, eventType, seq, byteCount, digest, fromStatus, toStatus, worker, errCode)
	return err
}

// insertNextEvent appends a task_event using the atomic per-task counter
// last_event_sequence_no (migration 0020). The UPDATE ... RETURNING in the
// CTE takes a row-level lock on the tasks row, serializing concurrent event
// inserts within the same transaction and across transactions. This replaces
// the old COALESCE(MAX(sequence_no),0)+1 pattern which required every caller
// to hold a FOR UPDATE lock and scanned task_events on every insert.
func insertNextEvent(ctx context.Context, tx pgx.Tx, taskID, eventType string, byteCount *int, digest *string, fromStatus, toStatus, worker, errCode string) error {
	_, err := tx.Exec(ctx, `
WITH next_seq AS (
  UPDATE tasks SET last_event_sequence_no = last_event_sequence_no + 1
  WHERE id = $1
  RETURNING last_event_sequence_no
)
INSERT INTO task_events (
  task_id, event_type, sequence_no, byte_count, digest, from_status, to_status, worker_instance, error_code, created_at
)
SELECT $1, $2, next_seq.last_event_sequence_no, $3, $4, NULLIF($5,''), NULLIF($6,''), NULLIF($7,''), NULLIF($8,''), timezone('utc', now())
FROM next_seq
`, taskID, eventType, byteCount, digest, fromStatus, toStatus, worker, errCode)
	return err
}

func validateSubmit(cmd domain.SubmitTaskCommand) error {
	if strings.TrimSpace(cmd.SessionKey) == "" {
		return fmt.Errorf("session_key is required")
	}
	if strings.TrimSpace(cmd.Source) == "" || len(cmd.Source) > domain.MaxSourceLen {
		return fmt.Errorf("source is required and must be <= %d", domain.MaxSourceLen)
	}
	if !domain.IsValidSource(cmd.Source) {
		return fmt.Errorf("source must be one of %s|%s", domain.SourceWechat, domain.SourceWeb)
	}
	if strings.TrimSpace(cmd.SourceInstanceID) == "" || len(cmd.SourceInstanceID) > domain.MaxSourceInstanceLen {
		return fmt.Errorf("source_instance_id is required and must be <= %d", domain.MaxSourceInstanceLen)
	}
	if strings.TrimSpace(cmd.MessageID) == "" || len(cmd.MessageID) > domain.MaxMessageIDLen {
		return fmt.Errorf("message_id is required and must be <= %d", domain.MaxMessageIDLen)
	}
	if strings.TrimSpace(cmd.Prompt) == "" {
		return fmt.Errorf("prompt is required")
	}
	if strings.TrimSpace(cmd.ToolPolicyVersion) == "" || len(cmd.ToolPolicyVersion) > domain.MaxToolPolicyVersionLen {
		return fmt.Errorf("tool_policy_version is required and must be <= %d", domain.MaxToolPolicyVersionLen)
	}
	if cmd.PersonaSnapshot == nil {
		return fmt.Errorf("persona_snapshot is required")
	}
	if err := validateTaskMedia(cmd.Media); err != nil {
		return err
	}
	return nil
}

// validateTaskMedia 校验任务入站媒体清单(2026-08-13 多模态链路):
// relative_path 必须相对会话沙箱根且不含路径穿越, 大小非负, 条数有上限
// (防恶意超长清单撑爆 TaskEnvelope/GA 首轮 payload)。
func validateTaskMedia(media []domain.TaskMedia) error {
	if len(media) > domain.MaxTaskMediaCount {
		return fmt.Errorf("task media exceeds max count (%d > %d)", len(media), domain.MaxTaskMediaCount)
	}
	for i, m := range media {
		if strings.TrimSpace(m.RelativePath) == "" {
			return fmt.Errorf("media[%d] relative_path is required", i)
		}
		clean := path.Clean(m.RelativePath)
		if clean != m.RelativePath || strings.HasPrefix(clean, "../") || strings.HasPrefix(clean, "/") {
			return fmt.Errorf("media[%d] relative_path %q must be a clean relative path", i, m.RelativePath)
		}
		if m.SizeBytes < 0 {
			return fmt.Errorf("media[%d] size_bytes must be non-negative", i)
		}
		// 2026-08-13 审查 D5: content_type 可空, 非空时仅限长度(流入
		// TaskEnvelope/提示上下文, 超长拒绝)。
		if len(m.ContentType) > 255 {
			return fmt.Errorf("media[%d] content_type exceeds 255 bytes", i)
		}
	}
	return nil
}

func encodePersona(p []string) ([]byte, int, error) {
	if p == nil {
		p = []string{}
	}
	raw, err := json.Marshal(p)
	if err != nil {
		return nil, 0, err
	}
	return raw, len(raw), nil
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func strPtr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// IsUniqueViolation reports a PostgreSQL unique_violation.
func IsUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}
