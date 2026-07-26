package postgres

import (
	"context"
	"fmt"
	"math"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/domain"
)

// PrepareCheckpoint inserts workspace_snapshots(state=writing) with generation token.
// StagingRefFunc computes the token-scoped staging reference inside the same
// transaction that inserts the workspace_snapshots row, so the DB-stored ref
// and the ref returned to the caller can never diverge (plan Task 5: token and
// staging_ref must be created atomically).
type StagingRefFunc func(snapshotID, token string, generation int64) string

func (s *Store) PrepareCheckpoint(ctx context.Context, taskID, platformInstanceID string, stagingRefFor StagingRefFunc, maxBundleBytes uint64) (snapshotID, token string, generation int64, err error) {
	if maxBundleBytes == 0 || maxBundleBytes > uint64(math.MaxInt64) {
		return "", "", 0, fmt.Errorf("max bundle bytes must be between 1 and %d", int64(math.MaxInt64))
	}
	if stagingRefFor == nil {
		return "", "", 0, fmt.Errorf("stagingRefFor callback is required")
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
		// Resolve the token-scoped staging ref before INSERT so the row stores
		// the final ref in the same transaction (no out-of-band rewrite).
		stagingRef := stagingRefFor(sid.String(), tok, gen)
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
		if t.CancelRequestedAt != nil && t.WorkerDispatchStartedAt != nil {
			status = domain.TaskInterrupted
			deliveryType = domain.DeliveryTaskInterrupted
			code = "TASK_INTERRUPTED"
			message = "task interrupted after accepted cancellation"
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
