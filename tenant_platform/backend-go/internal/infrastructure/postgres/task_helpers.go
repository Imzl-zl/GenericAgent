package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/domain"
)

const taskSelectColumns = `
id, workspace_id::text, session_key, session_sequence, requester_user_id,
source, source_instance_id, message_id, message_idempotency_key,
prompt, persona_snapshot, tool_policy_version, prompt_bytes, persona_bytes,
status, COALESCE(claim_owner,''), claim_lease_until,
COALESCE(worker_instance_id,''), worker_dispatch_started_at, cancel_requested_at,
snapshot_id::text, COALESCE(snapshot_checksum,''), COALESCE(result_ref,''), COALESCE(result_digest,''),
COALESCE(terminal_error_code,''), COALESCE(terminal_error_message,''), COALESCE(terminal_error_trace_id,''),
created_at, updated_at, started_at, succeeded_at, terminal_at, last_activity_at, fresh_session
`

type scannable interface {
	Scan(dest ...any) error
}

func scanTask(row scannable) (domain.Task, error) {
	var t domain.Task
	var personaRaw []byte
	var leaseUntil *time.Time
	var snapshotID *string
	var dispatchAt, cancelAt, startedAt, succeededAt, terminalAt *time.Time
	err := row.Scan(
		&t.ID, &t.WorkspaceID, &t.SessionKey, &t.SessionSequence, &t.RequesterID,
		&t.Source, &t.SourceInstanceID, &t.MessageID, &t.MessageIdempotencyKey,
		&t.Prompt, &personaRaw, &t.ToolPolicyVersion, &t.PromptBytes, &t.PersonaBytes,
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
	if len(message) > MaxTerminalErrorBytes {
		message = message[:MaxTerminalErrorBytes]
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
	if err := insertDelivery(ctx, tx, tt.ID, deliveryType, resultRef, resultDigest, code, message, traceID); err != nil {
		return domain.Task{}, err
	}
	return tt, nil
}

func insertDelivery(ctx context.Context, tx pgx.Tx, taskID string, dt domain.DeliveryType, ref, digest, code, message, traceID string) error {
	id := domain.StableDeliveryID(taskID, dt)
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
	if strings.TrimSpace(cmd.Source) == "" || len(cmd.Source) > MaxSourceLen {
		return fmt.Errorf("source is required and must be <= %d", MaxSourceLen)
	}
	if !domain.IsValidSource(cmd.Source) {
		return fmt.Errorf("source must be one of %s|%s", domain.SourceWechat, domain.SourceWeb)
	}
	if strings.TrimSpace(cmd.SourceInstanceID) == "" || len(cmd.SourceInstanceID) > MaxSourceInstanceLen {
		return fmt.Errorf("source_instance_id is required and must be <= %d", MaxSourceInstanceLen)
	}
	if strings.TrimSpace(cmd.MessageID) == "" || len(cmd.MessageID) > MaxMessageIDLen {
		return fmt.Errorf("message_id is required and must be <= %d", MaxMessageIDLen)
	}
	if strings.TrimSpace(cmd.Prompt) == "" {
		return fmt.Errorf("prompt is required")
	}
	if strings.TrimSpace(cmd.ToolPolicyVersion) == "" || len(cmd.ToolPolicyVersion) > MaxToolPolicyVersionLen {
		return fmt.Errorf("tool_policy_version is required and must be <= %d", MaxToolPolicyVersionLen)
	}
	if cmd.PersonaSnapshot == nil {
		return fmt.Errorf("persona_snapshot is required")
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
