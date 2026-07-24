// Package postgres implements the platform task store against PostgreSQL.
package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/domain"
)

// Application-enforced limits (not volatile DB now() checks).
const (
	MaxPromptBytes          = 64 * 1024
	MaxPersonaBytes         = 16 * 1024
	MaxTerminalErrorBytes   = 4 * 1024
	MaxToolPolicyVersionLen = 128
	MaxSourceLen            = 64
	MaxSourceInstanceLen    = 128
	MaxMessageIDLen         = 256
)

// DevelopmentContext is the approved loopback user/workspace pair.
type DevelopmentContext struct {
	UserID      int64
	Username    string
	WorkspaceID string
	SessionKey  string
}

// Store is the PostgreSQL-backed task store.
type Store struct {
	pool *pgxpool.Pool
}

// NewStore wraps a pgx pool.
func NewStore(pool *pgxpool.Pool) (*Store, error) {
	if pool == nil {
		return nil, fmt.Errorf("pool is nil")
	}
	return &Store{pool: pool}, nil
}

// Pool exposes the underlying pool for tests.
func (s *Store) Pool() *pgxpool.Pool { return s.pool }

// EnsureDevelopmentContext inserts or refreshes only bootstrap_marker='dev-loopback' rows.
// If the requested ID exists with another marker or non-approved status, it fails visibly.
func (s *Store) EnsureDevelopmentContext(ctx context.Context, userID int64, username string) (DevelopmentContext, error) {
	if userID <= 0 {
		return DevelopmentContext{}, fmt.Errorf("dev user id must be positive")
	}
	if strings.TrimSpace(username) == "" {
		username = fmt.Sprintf("dev-user-%d", userID)
	}
	sessionKey := fmt.Sprintf("personal:%d", userID)
	var out DevelopmentContext
	err := s.withTx(ctx, func(tx pgx.Tx) error {
		var existingID int64
		var status, marker *string
		err := tx.QueryRow(ctx, `
SELECT id, status, bootstrap_marker FROM users WHERE id = $1 FOR UPDATE
`, userID).Scan(&existingID, &status, &marker)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if err == nil {
			if status == nil || *status != "approved" {
				return fmt.Errorf("user %d exists with non-approved status; refusing bootstrap promotion", userID)
			}
			if marker == nil || *marker != "dev-loopback" {
				return fmt.Errorf("user %d exists without bootstrap_marker=dev-loopback; refusing bootstrap", userID)
			}
			if _, err := tx.Exec(ctx, `
UPDATE users SET username = $2, approved_at = COALESCE(approved_at, timezone('utc', now()))
WHERE id = $1 AND bootstrap_marker = 'dev-loopback'
`, userID, username); err != nil {
				return err
			}
		} else {
			if _, err := tx.Exec(ctx, `
INSERT INTO users (id, username, status, bootstrap_marker, approved_at)
VALUES ($1, $2, 'approved', 'dev-loopback', timezone('utc', now()))
`, userID, username); err != nil {
				return err
			}
		}

		var wsID uuid.UUID
		var wsMarker *string
		var vol *string
		err = tx.QueryRow(ctx, `
SELECT id, bootstrap_marker, volume_id FROM workspaces WHERE session_key = $1 FOR UPDATE
`, sessionKey).Scan(&wsID, &wsMarker, &vol)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if err == nil {
			if wsMarker == nil || *wsMarker != "dev-loopback" {
				return fmt.Errorf("workspace %s exists without bootstrap_marker=dev-loopback", sessionKey)
			}
			if _, err := tx.Exec(ctx, `
UPDATE workspaces SET owner_user_id = $2, kind = 'personal', team_id = NULL
WHERE id = $1 AND bootstrap_marker = 'dev-loopback'
`, wsID, userID); err != nil {
				return err
			}
		} else {
			wsID = uuid.New()
			if _, err := tx.Exec(ctx, `
INSERT INTO workspaces (id, session_key, owner_user_id, kind, team_id, volume_id, bootstrap_marker)
VALUES ($1, $2, $3, 'personal', NULL, NULL, 'dev-loopback')
`, wsID, sessionKey, userID); err != nil {
				return err
			}
		}
		out = DevelopmentContext{
			UserID:      userID,
			Username:    username,
			WorkspaceID: wsID.String(),
			SessionKey:  sessionKey,
		}
		return nil
	})
	return out, err
}

// SubmitTask inserts a queued task or returns the existing dedupe row unchanged.
func (s *Store) SubmitTask(ctx context.Context, cmd domain.SubmitTaskCommand) (domain.Task, error) {
	if err := validateSubmit(cmd); err != nil {
		return domain.Task{}, err
	}
	promptBytes := utf8.RuneCountInString(cmd.Prompt) // use byte size for storage limit
	promptBytes = len([]byte(cmd.Prompt))
	personaRaw, personaBytes, err := encodePersona(cmd.PersonaSnapshot)
	if err != nil {
		return domain.Task{}, err
	}
	if promptBytes > MaxPromptBytes {
		return domain.Task{}, fmt.Errorf("prompt exceeds max bytes (%d > %d)", promptBytes, MaxPromptBytes)
	}
	if personaBytes > MaxPersonaBytes {
		return domain.Task{}, fmt.Errorf("persona_snapshot exceeds max bytes (%d > %d)", personaBytes, MaxPersonaBytes)
	}

	var task domain.Task
	err = s.withTx(ctx, func(tx pgx.Tx) error {
		var workspaceID uuid.UUID
		var ownerID int64
		err := tx.QueryRow(ctx, `
SELECT id, owner_user_id FROM workspaces WHERE session_key = $1 FOR UPDATE
`, cmd.SessionKey).Scan(&workspaceID, &ownerID)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("workspace not found for session_key %q", cmd.SessionKey)
			}
			return err
		}
		if cmd.RequesterUserID != 0 && cmd.RequesterUserID != ownerID {
			// Foundation: requester must own the personal workspace.
			return fmt.Errorf("requester %d is not owner of session %s", cmd.RequesterUserID, cmd.SessionKey)
		}
		requester := ownerID
		if cmd.RequesterUserID != 0 {
			requester = cmd.RequesterUserID
		}

		var nextSeq int64
		if err := tx.QueryRow(ctx, `
SELECT COALESCE(MAX(session_sequence), 0) + 1 FROM tasks WHERE session_key = $1
`, cmd.SessionKey).Scan(&nextSeq); err != nil {
			return err
		}

		taskID := uuid.NewString()
		idemKey := cmd.MessageID
		row := tx.QueryRow(ctx, `
INSERT INTO tasks (
  id, workspace_id, session_key, session_sequence, requester_user_id,
  source, source_instance_id, message_id, message_idempotency_key,
  prompt, persona_snapshot, tool_policy_version, prompt_bytes, persona_bytes,
  status
) VALUES (
  $1,$2,$3,$4,$5,
  $6,$7,$8,$9,
  $10,$11::jsonb,$12,$13,$14,
  'queued'
)
ON CONFLICT (source, source_instance_id, message_idempotency_key) DO NOTHING
RETURNING `+taskSelectColumns, taskID, workspaceID, cmd.SessionKey, nextSeq, requester,
			cmd.Source, cmd.SourceInstanceID, cmd.MessageID, idemKey,
			cmd.Prompt, string(personaRaw), cmd.ToolPolicyVersion, promptBytes, personaBytes,
		)
		t, scanErr := scanTask(row)
		if scanErr == nil {
			task = t
			if err := insertEvent(ctx, tx, t.ID, "status_transition", 0, nil, nil, "", "queued", "", ""); err != nil {
				return err
			}
			return nil
		}
		if !errors.Is(scanErr, pgx.ErrNoRows) {
			return scanErr
		}
		// Conflict: return existing row FOR UPDATE unchanged.
		row = tx.QueryRow(ctx, `
SELECT `+taskSelectColumns+` FROM tasks
WHERE source = $1 AND source_instance_id = $2 AND message_idempotency_key = $3
FOR UPDATE
`, cmd.Source, cmd.SourceInstanceID, idemKey)
		t, err = scanTask(row)
		if err != nil {
			return err
		}
		task = t
		return nil
	})
	return task, err
}

// GetTask loads a task by id.
func (s *Store) GetTask(ctx context.Context, taskID string) (domain.Task, error) {
	row := s.pool.QueryRow(ctx, `SELECT `+taskSelectColumns+` FROM tasks WHERE id = $1`, taskID)
	t, err := scanTask(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Task{}, fmt.Errorf("task not found: %s", taskID)
	}
	return t, err
}

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
		var workspaceID uuid.UUID
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
`, taskID, platformInstanceID, leaseMicros)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("heartbeat missed for task %s owner %s", taskID, platformInstanceID)
	}
	return nil
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

// MarkRunning advances starting -> running after Worker acceptance.
func (s *Store) MarkRunning(ctx context.Context, taskID, platformInstanceID string) (domain.Task, error) {
	var task domain.Task
	err := s.withTx(ctx, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `
UPDATE tasks SET status = 'running', updated_at = timezone('utc', now())
WHERE id = $1 AND claim_owner = $2 AND status = 'starting'
  AND worker_dispatch_started_at IS NOT NULL
RETURNING `+taskSelectColumns, taskID, platformInstanceID)
		t, err := scanTask(row)
		if err != nil {
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
func (s *Store) RecordChunkEvent(ctx context.Context, taskID string, byteCount int, digest string) error {
	return s.withTx(ctx, func(tx pgx.Tx) error {
		var lockedTaskID string
		if err := tx.QueryRow(ctx, `SELECT id FROM tasks WHERE id=$1 FOR UPDATE`, taskID).Scan(&lockedTaskID); err != nil {
			return err
		}
		bc := byteCount
		return insertNextEvent(ctx, tx, lockedTaskID, "chunk", &bc, strPtr(digest), "", "", "", "")
	})
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

// RecoverAfterRestart interrupts expired foreign-owner starting/running rows.
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
  AND claim_owner <> $1
  AND claim_lease_until IS NOT NULL
  AND claim_lease_until < timezone('utc', now())
FOR UPDATE
`, platformInstanceID)
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

// PrepareCheckpoint inserts workspace_snapshots(state=writing) with generation token.
func (s *Store) PrepareCheckpoint(ctx context.Context, taskID, platformInstanceID, stagingRef string, maxBundleBytes uint64) (snapshotID, token string, generation int64, err error) {
	if maxBundleBytes == 0 || maxBundleBytes > uint64(math.MaxInt64) {
		return "", "", 0, fmt.Errorf("max bundle bytes must be between 1 and %d", int64(math.MaxInt64))
	}
	err = s.withTx(ctx, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `SELECT `+taskSelectColumns+` FROM tasks WHERE id = $1 FOR UPDATE`, taskID)
		t, err := scanTask(row)
		if err != nil {
			return err
		}
		if t.ClaimOwner != platformInstanceID {
			return fmt.Errorf("prepare checkpoint: claim owner mismatch")
		}
		if t.Status != domain.TaskStarting && t.Status != domain.TaskRunning {
			return fmt.Errorf("prepare checkpoint: task status %s", t.Status)
		}
		var gen int64
		if err := tx.QueryRow(ctx, `
SELECT COALESCE(MAX(generation), 0) + 1 FROM workspace_snapshots WHERE workspace_id = $1::uuid
`, t.WorkspaceID).Scan(&gen); err != nil {
			return err
		}
		sid := uuid.New()
		tok := fmt.Sprintf("ckpt-%s-g%d-%s", taskID, gen, uuid.NewString())
		leaseUntil := time.Now().UTC().Add(2 * time.Minute)
		if _, err := tx.Exec(ctx, `
INSERT INTO workspace_snapshots (
  id, workspace_id, task_id, schema_version, state, generation,
  lease_owner, lease_until, token, staging_ref, max_bundle_bytes
) VALUES (
  $1, $2::uuid, $3, 'genericagent.snapshot.v1', 'writing', $4,
  $5, $6, $7, $8, $9
)
`, sid, t.WorkspaceID, t.ID, gen, platformInstanceID, leaseUntil, tok, stagingRef, int64(maxBundleBytes)); err != nil {
			return err
		}
		snapshotID = sid.String()
		token = tok
		generation = gen
		return nil
	})
	return snapshotID, token, generation, err
}

// LoadSnapshotToken returns writing snapshot metadata for token validation.
func (s *Store) LoadSnapshotToken(ctx context.Context, snapshotID, token string) (workspaceID, taskID, stagingRef, leaseOwner string, leaseUntil time.Time, generation int64, maxBundleBytes uint64, state string, err error) {
	var maxBundle int64
	err = s.pool.QueryRow(ctx, `
SELECT workspace_id::text, task_id, COALESCE(staging_ref,''), COALESCE(lease_owner,''),
       COALESCE(lease_until, timezone('utc', now())), generation, max_bundle_bytes, state
FROM workspace_snapshots WHERE id = $1::uuid AND token = $2
`, snapshotID, token).Scan(&workspaceID, &taskID, &stagingRef, &leaseOwner, &leaseUntil, &generation, &maxBundle, &state)
	if err == nil {
		maxBundleBytes = uint64(maxBundle)
	}
	return
}

// CompleteSucceeded commits snapshot + task + delivery in one transaction.
func (s *Store) CompleteSucceeded(ctx context.Context, taskID, platformInstanceID, snapshotID, fileRef, checksum, resultRef, resultDigest string, resultBytes int) (domain.Task, error) {
	var task domain.Task
	err := s.withTx(ctx, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `SELECT `+taskSelectColumns+` FROM tasks WHERE id = $1 FOR UPDATE`, taskID)
		t, err := scanTask(row)
		if err != nil {
			return err
		}
		if t.ClaimOwner != platformInstanceID {
			return fmt.Errorf("complete: claim owner mismatch")
		}
		if t.Status.IsTerminal() {
			return fmt.Errorf("complete: already terminal %s", t.Status)
		}
		if t.CancelRequestedAt != nil {
			if _, err := tx.Exec(ctx, `
UPDATE workspace_snapshots SET
  state = 'quarantined',
  lease_owner = NULL,
  lease_until = NULL,
  staging_ref = NULL
WHERE id = $1::uuid AND task_id = $2 AND state = 'writing'
`, snapshotID, taskID); err != nil {
				return err
			}
			tt, err := finalizeTerminal(ctx, tx, t, domain.TaskInterrupted, domain.DeliveryTaskInterrupted,
				"TASK_INTERRUPTED", "task interrupted after accepted cancellation", "", "", "")
		if err != nil {
				return err
			}
			task = tt
			return nil
		}
		tag, err := tx.Exec(ctx, `
UPDATE workspace_snapshots SET
  state = 'committed',
  file_ref = $2,
  checksum = $3,
  result_ref = $4,
  result_digest = $5,
  result_bytes = $6,
  committed_at = timezone('utc', now()),
  lease_owner = NULL,
  lease_until = NULL,
  staging_ref = NULL
WHERE id = $1::uuid AND task_id = $7 AND state = 'writing' AND token IS NOT NULL
`, snapshotID, fileRef, checksum, resultRef, resultDigest, resultBytes, taskID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return fmt.Errorf("complete: snapshot %s not writable", snapshotID)
		}
		if _, err := tx.Exec(ctx, `
UPDATE workspaces SET current_snapshot_id = $2::uuid WHERE id = $1::uuid
`, t.WorkspaceID, snapshotID); err != nil {
			return err
		}
		row = tx.QueryRow(ctx, `
UPDATE tasks SET
  status = 'succeeded',
  claim_owner = NULL,
  claim_lease_until = NULL,
  claimed_at = NULL,
  snapshot_id = $2::uuid,
  snapshot_checksum = $3,
  result_ref = $4,
  result_digest = $5,
  succeeded_at = timezone('utc', now()),
  terminal_at = timezone('utc', now()),
  updated_at = timezone('utc', now())
WHERE id = $1
RETURNING `+taskSelectColumns, taskID, snapshotID, checksum, resultRef, resultDigest)
		tt, err := scanTask(row)
		if err != nil {
			return err
		}
		if err := insertNextEvent(ctx, tx, tt.ID, "status_transition", nil, strPtr(resultDigest), string(t.Status), "succeeded", tt.WorkerInstanceID, ""); err != nil {
			return err
		}
		if err := insertDelivery(ctx, tx, tt.ID, domain.DeliveryTaskComplete, resultRef, resultDigest, "", "", ""); err != nil {
			return err
		}
		task = tt
		return nil
	})
	return task, err
}

// CompleteFailedTerminal commits failed/cancelled/interrupted without success checkpoint.
func (s *Store) CompleteFailedTerminal(ctx context.Context, taskID string, status domain.TaskStatus, deliveryType domain.DeliveryType, code, message, traceID string) (domain.Task, error) {
	var task domain.Task
	err := s.withTx(ctx, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `SELECT `+taskSelectColumns+` FROM tasks WHERE id = $1 FOR UPDATE`, taskID)
		t, err := scanTask(row)
		if err != nil {
			return err
		}
		if t.Status.IsTerminal() {
			task = t
			return nil
		}
		tt, err := finalizeTerminal(ctx, tx, t, status, deliveryType, code, message, "", "", traceID)
		if err != nil {
			return err
		}
		task = tt
		return nil
	})
	return task, err
}

// ListClaimableSessionKeys returns session keys with queued work and no active claim.
func (s *Store) ListClaimableSessionKeys(ctx context.Context, limit int) ([]string, error) {
	if limit <= 0 {
		limit = 32
	}
	rows, err := s.pool.Query(ctx, `
SELECT DISTINCT t.session_key
FROM tasks t
WHERE t.status = 'queued'
  AND NOT EXISTS (
    SELECT 1 FROM tasks r
    WHERE r.session_key = t.session_key AND r.status IN ('starting','running')
  )
ORDER BY t.session_key
LIMIT $1
`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var sk string
		if err := rows.Scan(&sk); err != nil {
			return nil, err
		}
		out = append(out, sk)
	}
	return out, rows.Err()
}

// ListOwnedActiveTasks returns starting/running tasks for this platform instance.
func (s *Store) ListOwnedActiveTasks(ctx context.Context, platformInstanceID string) ([]domain.Task, error) {
	rows, err := s.pool.Query(ctx, `
SELECT `+taskSelectColumns+` FROM tasks
WHERE claim_owner = $1 AND status IN ('starting','running')
ORDER BY claimed_at ASC NULLS LAST
`, platformInstanceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.Task
	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
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

// --- internals ---

const taskSelectColumns = `
id, workspace_id::text, session_key, session_sequence, requester_user_id,
source, source_instance_id, message_id, message_idempotency_key,
prompt, persona_snapshot, tool_policy_version, prompt_bytes, persona_bytes,
status, COALESCE(claim_owner,''), claim_lease_until,
COALESCE(worker_instance_id,''), worker_dispatch_started_at, cancel_requested_at,
snapshot_id::text, COALESCE(snapshot_checksum,''), COALESCE(result_ref,''), COALESCE(result_digest,''),
COALESCE(terminal_error_code,''), COALESCE(terminal_error_message,''), COALESCE(terminal_error_trace_id,''),
created_at, updated_at, started_at, succeeded_at, terminal_at
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
		&t.CreatedAt, &t.UpdatedAt, &startedAt, &succeededAt, &terminalAt,
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

func insertNextEvent(ctx context.Context, tx pgx.Tx, taskID, eventType string, byteCount *int, digest *string, fromStatus, toStatus, worker, errCode string) error {
	var sequence int64
	if err := tx.QueryRow(ctx, `
SELECT COALESCE(MAX(sequence_no), -1) + 1 FROM task_events WHERE task_id = $1
`, taskID).Scan(&sequence); err != nil {
		return err
	}
	return insertEvent(ctx, tx, taskID, eventType, sequence, byteCount, digest, fromStatus, toStatus, worker, errCode)
}

func (s *Store) withTx(ctx context.Context, fn func(pgx.Tx) error) error {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func validateSubmit(cmd domain.SubmitTaskCommand) error {
	if strings.TrimSpace(cmd.SessionKey) == "" {
		return fmt.Errorf("session_key is required")
	}
	if strings.TrimSpace(cmd.Source) == "" || len(cmd.Source) > MaxSourceLen {
		return fmt.Errorf("source is required and must be <= %d", MaxSourceLen)
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
