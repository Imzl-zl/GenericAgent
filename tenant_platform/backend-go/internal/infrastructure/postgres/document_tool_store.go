package postgres

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/domain"
)

const documentToolKeyPrefix = "document-task:"

func documentToolJobKey(taskID string) string {
	return documentToolKeyPrefix + strings.TrimSpace(taskID)
}

func documentToolCommandID(taskID, requestID string) string {
	digest := sha256.Sum256([]byte(strings.TrimSpace(taskID) + "\x00" + strings.TrimSpace(requestID)))
	return "gateway:" + hex.EncodeToString(digest[:])
}

func (s *Store) SubmitDocumentToolCommand(
	ctx context.Context,
	cmd domain.SubmitDocumentToolCommand,
) (domain.DocumentToolSubmission, error) {
	if err := validateDocumentToolScope(cmd.Scope); err != nil {
		return domain.DocumentToolSubmission{}, err
	}
	cmd.RequestID = strings.TrimSpace(cmd.RequestID)
	if cmd.RequestID == "" || len(cmd.RequestID) > 256 {
		return domain.DocumentToolSubmission{}, fmt.Errorf("document tool request id is required and must be <= 256 bytes")
	}
	if cmd.Operation.SchemaVersion != 1 || strings.TrimSpace(cmd.Operation.Operation) == "" || len(cmd.Operation.Operation) > 128 {
		return domain.DocumentToolSubmission{}, fmt.Errorf("operation requires schema_version 1 and a name <= 128 bytes")
	}
	if len(cmd.Operation.Parameters) == 0 || !json.Valid(cmd.Operation.Parameters) {
		return domain.DocumentToolSubmission{}, fmt.Errorf("operation parameters must be valid JSON")
	}
	encodedOperation, err := json.Marshal(cmd.Operation)
	if err != nil {
		return domain.DocumentToolSubmission{}, fmt.Errorf("encode document operation: %w", err)
	}
	commandPayload, commandHash, err := canonicalDocumentPayload(encodedOperation)
	if err != nil {
		return domain.DocumentToolSubmission{}, err
	}

	var submission domain.DocumentToolSubmission
	err = s.withTx(ctx, func(tx pgx.Tx) error {
		var enabled bool
		var globalLimit, workspaceLimit int
		if err := tx.QueryRow(ctx, `
SELECT enabled,global_queue_limit,per_tenant_queue_limit
FROM document_pool_settings WHERE singleton=TRUE FOR UPDATE
`).Scan(&enabled, &globalLimit, &workspaceLimit); err != nil {
			return fmt.Errorf("lock document pool settings: %w", err)
		}
		task, err := lockDocumentToolTask(ctx, tx, cmd.Scope, true)
		if err != nil {
			return err
		}
		job, err := getOrCreateDocumentToolJob(ctx, tx, task, enabled, globalLimit, workspaceLimit)
		if err != nil {
			return err
		}
		command, err := submitDocumentToolCommandTx(
			ctx, tx, job, task.RequesterID,
			documentToolCommandID(task.ID, cmd.RequestID), commandPayload, commandHash,
		)
		if err != nil {
			return err
		}
		submission = domain.DocumentToolSubmission{Job: job, Command: command}
		return nil
	})
	return submission, err
}

func (s *Store) CloseDocumentToolJob(
	ctx context.Context,
	scope domain.DocumentToolTaskScope,
) (domain.DocumentJob, error) {
	if err := validateDocumentToolScope(scope); err != nil {
		return domain.DocumentJob{}, err
	}
	var job domain.DocumentJob
	err := s.withTx(ctx, func(tx pgx.Tx) error {
		task, err := lockDocumentToolTask(ctx, tx, scope, false)
		if err != nil {
			return err
		}
		current, err := lockDocumentToolJob(ctx, tx, task)
		if err != nil {
			return err
		}
		if current.CommandsClosedAt == nil && current.Status != domain.DocumentJobQueued && current.Status != domain.DocumentJobStarting && current.Status != domain.DocumentJobRunning && !current.Status.IsTerminal() {
			return fmt.Errorf("%w: cannot close commands while job is %s", domain.ErrDocumentJobState, current.Status)
		}
		stored, err := scanDocumentJob(tx.QueryRow(ctx, `
WITH timestamp AS (SELECT clock_timestamp() AS value)
UPDATE document_jobs
SET commands_closed_at=COALESCE(commands_closed_at,timestamp.value),
 last_activity_at=CASE WHEN commands_closed_at IS NULL THEN timestamp.value ELSE last_activity_at END,
 updated_at=CASE WHEN commands_closed_at IS NULL THEN timestamp.value ELSE updated_at END
FROM timestamp
WHERE id=$1 RETURNING `+documentJobColumns, current.ID))
		if err != nil {
			return err
		}
		job = stored
		return nil
	})
	return job, err
}

func (s *Store) GetDocumentToolStatus(
	ctx context.Context,
	scope domain.DocumentToolTaskScope,
	requestID string,
) (domain.DocumentToolStatus, error) {
	if err := validateDocumentToolScope(scope); err != nil {
		return domain.DocumentToolStatus{}, err
	}
	requestID = strings.TrimSpace(requestID)
	if len(requestID) > 256 {
		return domain.DocumentToolStatus{}, fmt.Errorf("document tool request id must be <= 256 bytes")
	}
	var status domain.DocumentToolStatus
	err := s.withTx(ctx, func(tx pgx.Tx) error {
		task, err := lockDocumentToolTask(ctx, tx, scope, true)
		if err != nil {
			return err
		}
		job, err := lockDocumentToolJob(ctx, tx, task)
		if err != nil {
			return err
		}
		status.Job = job
		if requestID == "" {
			return nil
		}
		command, err := scanDocumentCommand(tx.QueryRow(ctx, `
SELECT `+documentCommandColumns+` FROM document_commands
WHERE job_id=$1 AND command_id=$2
`, job.ID, documentToolCommandID(task.ID, requestID)))
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrDocumentCommandNotFound
		}
		if err != nil {
			return err
		}
		status.Command = &command
		return nil
	})
	return status, err
}

func (s *Store) GetDocumentToolArtifact(
	ctx context.Context,
	scope domain.DocumentToolTaskScope,
	requestID string,
) (domain.DocumentArtifact, error) {
	if err := validateDocumentToolScope(scope); err != nil {
		return domain.DocumentArtifact{}, err
	}
	requestID = strings.TrimSpace(requestID)
	if requestID == "" || len(requestID) > 256 {
		return domain.DocumentArtifact{}, fmt.Errorf("document tool request id is required and must be <= 256 bytes")
	}
	var artifact domain.DocumentArtifact
	err := s.withTx(ctx, func(tx pgx.Tx) error {
		task, err := lockDocumentToolTask(ctx, tx, scope, true)
		if err != nil {
			return err
		}
		job, err := lockDocumentToolJob(ctx, tx, task)
		if err != nil {
			return err
		}
		stored, err := scanDocumentArtifact(tx.QueryRow(ctx, `
SELECT a.id::text,a.job_id::text,a.command_id,a.file_name,a.media_type,
 a.content,a.size_bytes,a.sha256,a.created_at
FROM document_artifacts a
JOIN document_commands c ON c.job_id=a.job_id AND c.command_id=a.command_id
WHERE a.job_id=$1 AND a.command_id=$2 AND c.status='succeeded'
`, job.ID, documentToolCommandID(task.ID, requestID)))
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrDocumentArtifactNotFound
		}
		if err != nil {
			return err
		}
		artifact = stored
		return nil
	})
	return artifact, err
}

func validateDocumentToolScope(scope domain.DocumentToolTaskScope) error {
	if _, err := uuid.Parse(strings.TrimSpace(scope.TaskID)); err != nil {
		return fmt.Errorf("document tool task id must be a UUID: %w", err)
	}
	if strings.TrimSpace(scope.SessionKey) == "" {
		return fmt.Errorf("document tool session key is required")
	}
	if _, err := uuid.Parse(strings.TrimSpace(scope.WorkspaceID)); err != nil {
		return fmt.Errorf("document tool workspace id must be a UUID: %w", err)
	}
	return nil
}

func lockDocumentToolTask(
	ctx context.Context,
	tx pgx.Tx,
	scope domain.DocumentToolTaskScope,
	requireActive bool,
) (domain.Task, error) {
	observed, err := scanTask(tx.QueryRow(ctx, `
SELECT `+taskSelectColumns+` FROM tasks WHERE id=$1
`, scope.TaskID))
	if err != nil {
		return domain.Task{}, err
	}
	if observed.SessionKey != strings.TrimSpace(scope.SessionKey) || observed.WorkspaceID != strings.TrimSpace(scope.WorkspaceID) {
		return domain.Task{}, domain.ErrDocumentUnauthorized
	}
	if err := lockDocumentToolMembership(ctx, tx, observed); err != nil {
		return domain.Task{}, err
	}
	task, err := scanTask(tx.QueryRow(ctx, `
SELECT `+taskSelectColumns+` FROM tasks WHERE id=$1 FOR UPDATE
`, scope.TaskID))
	if err != nil {
		return domain.Task{}, err
	}
	if task.SessionKey != observed.SessionKey || task.WorkspaceID != observed.WorkspaceID || task.RequesterID != observed.RequesterID {
		return domain.Task{}, domain.ErrDocumentUnauthorized
	}
	if !requireActive {
		return task, nil
	}
	if task.Status != domain.TaskRunning || task.CancelRequestedAt != nil || task.ClaimLeaseUntil.IsZero() {
		return domain.Task{}, domain.ErrDocumentTaskInactive
	}
	var leaseValid bool
	if err := tx.QueryRow(ctx, `SELECT claim_lease_until > timezone('utc',now()) FROM tasks WHERE id=$1`, task.ID).Scan(&leaseValid); err != nil {
		return domain.Task{}, err
	}
	if !leaseValid {
		return domain.Task{}, domain.ErrDocumentTaskInactive
	}
	return task, nil
}

func lockDocumentToolMembership(ctx context.Context, tx pgx.Tx, task domain.Task) error {
	var ownerID int64
	var kind, teamID string
	if err := tx.QueryRow(ctx, `
SELECT owner_user_id,kind,COALESCE(team_id::text,'')
FROM workspaces WHERE id=$1
`, task.WorkspaceID).Scan(&ownerID, &kind, &teamID); err != nil {
		return err
	}
	switch kind {
	case "personal":
		if task.RequesterID != ownerID {
			return domain.ErrDocumentUnauthorized
		}
		return nil
	case "team":
		if teamID == "" {
			return domain.ErrDocumentUnauthorized
		}
		if task.RequesterID == ownerID {
			return nil
		}
		var status string
		if err := tx.QueryRow(ctx, `
SELECT status FROM team_members
WHERE team_id=$1::uuid AND user_id=$2 FOR SHARE
`, teamID, task.RequesterID).Scan(&status); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.ErrDocumentUnauthorized
			}
			return err
		}
		if status != string(domain.MemberApproved) {
			return domain.ErrDocumentUnauthorized
		}
		return nil
	default:
		return domain.ErrDocumentUnauthorized
	}
}

func getOrCreateDocumentToolJob(
	ctx context.Context,
	tx pgx.Tx,
	task domain.Task,
	enabled bool,
	globalLimit, workspaceLimit int,
) (domain.DocumentJob, error) {
	idempotencyKey := documentToolJobKey(task.ID)
	jobPayload, jobHash, err := canonicalDocumentPayload(json.RawMessage(fmt.Sprintf(
		`{"schema_version":1,"task_id":%q}`, task.ID,
	)))
	if err != nil {
		return domain.DocumentJob{}, err
	}
	existing, existingHash, err := getDocumentJobByIdempotencyTx(ctx, tx, uuid.MustParse(task.WorkspaceID), idempotencyKey)
	if err == nil {
		if existingHash != jobHash || existing.RequesterUserID != task.RequesterID {
			return domain.DocumentJob{}, domain.ErrDocumentIdempotencyConflict
		}
		return existing, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return domain.DocumentJob{}, err
	}
	if !enabled {
		return domain.DocumentJob{}, domain.ErrDocumentPoolDisabled
	}

	var globalQueued, workspaceQueued int
	if err := tx.QueryRow(ctx, `
SELECT count(*),count(*) FILTER (WHERE workspace_id=$1)
FROM document_jobs WHERE status='queued'
`, task.WorkspaceID).Scan(&globalQueued, &workspaceQueued); err != nil {
		return domain.DocumentJob{}, err
	}
	if globalQueued >= globalLimit {
		return domain.DocumentJob{}, domain.ErrDocumentGlobalQueueFull
	}
	if workspaceQueued >= workspaceLimit {
		return domain.DocumentJob{}, domain.ErrDocumentWorkspaceQueueFull
	}
	return scanDocumentJob(tx.QueryRow(ctx, `
INSERT INTO document_jobs(
 id,workspace_id,requester_user_id,idempotency_key,payload,payload_hash,status
) VALUES($1,$2,$3,$4,$5,$6,'queued')
RETURNING `+documentJobColumns,
		uuid.NewString(), task.WorkspaceID, task.RequesterID, idempotencyKey, jobPayload, jobHash))
}

func submitDocumentToolCommandTx(
	ctx context.Context,
	tx pgx.Tx,
	job domain.DocumentJob,
	requester int64,
	commandID string,
	payload []byte,
	payloadHash string,
) (domain.DocumentCommand, error) {
	current, err := scanDocumentJob(tx.QueryRow(ctx, `
SELECT `+documentJobColumns+` FROM document_jobs WHERE id=$1 FOR UPDATE
`, job.ID))
	if err != nil {
		return domain.DocumentCommand{}, err
	}
	if current.RequesterUserID != requester {
		return domain.DocumentCommand{}, domain.ErrDocumentUnauthorized
	}
	existingHash := ""
	existing, err := scanDocumentCommandWithHash(tx.QueryRow(ctx, `
SELECT `+documentCommandColumns+`,payload_hash FROM document_commands
WHERE job_id=$1 AND command_id=$2 FOR UPDATE
`, current.ID, commandID), &existingHash)
	if err == nil {
		if existingHash != payloadHash {
			return domain.DocumentCommand{}, domain.ErrDocumentIdempotencyConflict
		}
		return existing, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return domain.DocumentCommand{}, err
	}
	if current.CommandsClosedAt != nil {
		return domain.DocumentCommand{}, domain.ErrDocumentCommandsClosed
	}
	if current.Status != domain.DocumentJobQueued && current.Status != domain.DocumentJobStarting && current.Status != domain.DocumentJobRunning {
		return domain.DocumentCommand{}, fmt.Errorf("%w: cannot submit command while job is %s", domain.ErrDocumentJobState, current.Status)
	}
	stored, err := scanDocumentCommand(tx.QueryRow(ctx, `
INSERT INTO document_commands(id,job_id,command_id,payload,payload_hash,status)
VALUES($1,$2,$3,$4,$5,'pending') RETURNING `+documentCommandColumns,
		uuid.NewString(), current.ID, commandID, payload, payloadHash))
	if err != nil {
		return domain.DocumentCommand{}, err
	}
	if _, err := tx.Exec(ctx, `
UPDATE document_jobs SET last_activity_at=timezone('utc',now()),updated_at=timezone('utc',now())
WHERE id=$1 AND status IN ('starting','running')
`, current.ID); err != nil {
		return domain.DocumentCommand{}, err
	}
	return stored, nil
}

func lockDocumentToolJob(
	ctx context.Context,
	tx pgx.Tx,
	task domain.Task,
) (domain.DocumentJob, error) {
	job, err := scanDocumentJob(tx.QueryRow(ctx, `
SELECT `+documentJobColumns+` FROM document_jobs
WHERE workspace_id=$1 AND idempotency_key=$2 FOR UPDATE
`, task.WorkspaceID, documentToolJobKey(task.ID)))
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.DocumentJob{}, domain.ErrDocumentJobNotFound
	}
	if err != nil {
		return domain.DocumentJob{}, err
	}
	if job.RequesterUserID != task.RequesterID {
		return domain.DocumentJob{}, domain.ErrDocumentUnauthorized
	}
	return job, nil
}
