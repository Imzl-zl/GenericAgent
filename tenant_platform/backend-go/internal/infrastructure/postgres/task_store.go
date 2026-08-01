package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/domain"
)

// SubmitTask inserts a queued task or returns the existing dedupe row unchanged.
// PerUserQueueLimit, when > 0, is enforced inside this transaction (after the
// workspace row lock serializes concurrent submits for the same user) to
// prevent TOCTOU races.
func (s *Store) SubmitTask(ctx context.Context, cmd domain.SubmitTaskCommand) (domain.Task, error) {
	if err := validateSubmit(cmd); err != nil {
		return domain.Task{}, err
	}
	promptBytes := len([]byte(cmd.Prompt))
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
		var kind, teamID string
		var resetAt *time.Time
		err := tx.QueryRow(ctx, `
SELECT id, owner_user_id, kind, COALESCE(team_id::text, ''), reset_at FROM workspaces WHERE session_key = $1 FOR UPDATE
`, cmd.SessionKey).Scan(&workspaceID, &ownerID, &kind, &teamID, &resetAt)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("workspace not found for session_key %q", cmd.SessionKey)
			}
			return err
		}
		requester := ownerID
		if cmd.RequesterUserID != 0 {
			requester = cmd.RequesterUserID
		}
		if err := authorizeSubmitter(tx, ctx, kind, ownerID, teamID, requester); err != nil {
			return err
		}
		// /new was issued since the last committed snapshot: the next task
		// starts with cleared history and working state. Clear the marker in
		// the same workspace-locked tx so a concurrent submit cannot miss it.
		freshSession := false
		if resetAt != nil {
			freshSession = true
			if _, err := tx.Exec(ctx, `UPDATE workspaces SET reset_at = NULL WHERE session_key = $1`, cmd.SessionKey); err != nil {
				return err
			}
		}

		// Hard per-user queue cap inside the workspace lock. Concurrent
		// submits for the same user serialize on the workspace FOR UPDATE
		// above, so the count is consistent. The soft pre-check in
		// TaskService.SubmitTask handles the obvious case without entering a tx.
		if s.perUserQueueLimit > 0 && requester > 0 {
			queued, err := s.CountQueuedTasksByRequesterTx(ctx, tx, requester)
			if err != nil {
				return fmt.Errorf("count queued (tx): %w", err)
			}
			if queued >= s.perUserQueueLimit {
				return domain.ErrPerUserQueueFull
			}
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
  status, fresh_session
) VALUES (
  $1,$2,$3,$4,$5,
  $6,$7,$8,$9,
  $10,$11::jsonb,$12,$13,$14,
  'queued', $15
)
ON CONFLICT (source, source_instance_id, message_idempotency_key) DO NOTHING
RETURNING `+taskSelectColumns, taskID, workspaceID, cmd.SessionKey, nextSeq, requester,
			cmd.Source, cmd.SourceInstanceID, cmd.MessageID, idemKey,
			cmd.Prompt, string(personaRaw), cmd.ToolPolicyVersion, promptBytes, personaBytes,
			freshSession,
		)
		t, scanErr := scanTask(row)
		if scanErr == nil {
			task = t
			if err := insertEvent(ctx, tx, t.ID, "status_transition", 0, nil, nil, "", "queued", "", ""); err != nil {
				return err
			}
			// Create initial "task started" delivery for immediate user feedback.
			// This gives users a "processing..." notification instead of silent waiting.
			if err := insertDelivery(ctx, tx, t.ID, domain.DeliveryTaskStarted, "", "", "", "", ""); err != nil {
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
	if err != nil {
		return domain.Task{}, err
	}
	return t, nil
}

// ResetWorkspace marks the session for fresh start: the next submitted task
// will have fresh_session=true and the Worker will be restarted without
// loading the prior snapshot. Implements /new (spec §7).
func (s *Store) ResetWorkspace(ctx context.Context, sessionKey string) error {
	_, err := s.pool.Exec(ctx, `
UPDATE workspaces SET reset_at = timezone('utc', now())
WHERE session_key = $1
`, sessionKey)
	return err
}

// ListClaimableSessionKeys returns session keys with queued work and no active
// claim, ordered by cross-tenant round-robin fairness: each requester's oldest
// queued task gets a ROW_NUMBER, and sessions are ordered by (rn, oldest) so
// every requester rotates through instead of one user monopolizing the queue.
// When perUserRunningLimit > 0, sessions whose next-task requester already has
// >= perUserRunningLimit starting/running tasks are excluded.
func (s *Store) ListClaimableSessionKeys(ctx context.Context, limit, perUserRunningLimit int) ([]string, error) {
	if limit <= 0 {
		limit = 32
	}
	rows, err := s.pool.Query(ctx, `
SELECT session_key FROM (
  SELECT
    t.session_key,
    ROW_NUMBER() OVER (PARTITION BY t.requester_user_id ORDER BY MIN(t.created_at)) AS rn,
    MIN(t.created_at) AS oldest
  FROM tasks t
  WHERE t.status = 'queued'
  AND NOT EXISTS (
    SELECT 1 FROM tasks t2
    WHERE t2.session_key = t.session_key AND t2.status IN ('starting','running')
  )
  AND (
    $2 = 0 OR NOT EXISTS (
      SELECT 1 FROM tasks t3
      WHERE t3.requester_user_id = t.requester_user_id
      AND t3.status IN ('starting','running')
      HAVING COUNT(*) >= $2
    )
  )
  GROUP BY t.session_key, t.requester_user_id
) ranked
ORDER BY ranked.rn, ranked.oldest
LIMIT $1
`, limit, perUserRunningLimit)
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

// CountRunningTasks returns the global number of starting/running tasks.
// Used by the scheduler to enforce MaxRunningTasks before claiming.
func (s *Store) CountRunningTasks(ctx context.Context) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx, `
SELECT COUNT(*) FROM tasks WHERE status IN ('starting','running')
`).Scan(&n)
	if err != nil {
		return 0, err
	}
	return n, nil
}

// CountQueuedTasksByRequester returns the number of queued tasks owned by
// the given requester. Used by SubmitTask to enforce PerUserQueueLimit.
// Reads inside the caller's transaction when called via CountQueuedTasksByRequesterTx.
func (s *Store) CountQueuedTasksByRequester(ctx context.Context, requesterUserID int64) (int, error) {
	if requesterUserID <= 0 {
		return 0, fmt.Errorf("requester user id must be positive")
	}
	var n int
	err := s.pool.QueryRow(ctx, `
SELECT COUNT(*) FROM tasks WHERE requester_user_id = $1 AND status = 'queued'
`, requesterUserID).Scan(&n)
	if err != nil {
		return 0, err
	}
	return n, nil
}

// CountQueuedTasksByRequesterTx is the in-transaction variant used by
// SubmitTask to avoid TOCTOU races. The count is taken after the workspace
// row lock so concurrent submits for the same user serialize on the workspace.
func (s *Store) CountQueuedTasksByRequesterTx(ctx context.Context, tx pgx.Tx, requesterUserID int64) (int, error) {
	if requesterUserID <= 0 {
		return 0, fmt.Errorf("requester user id must be positive")
	}
	var n int
	err := tx.QueryRow(ctx, `
SELECT COUNT(*) FROM tasks WHERE requester_user_id = $1 AND status = 'queued'
`, requesterUserID).Scan(&n)
	if err != nil {
		return 0, err
	}
	return n, nil
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
