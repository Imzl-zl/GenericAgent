package postgres

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/domain"
)

const documentJobColumns = `
id::text, workspace_id::text, requester_user_id, idempotency_key, payload,
status, COALESCE(instance_id::text,''), COALESCE(claim_owner,''), generation,
claim_lease_until, claimed_at, last_activity_at,
COALESCE(terminal_error_code,''), COALESCE(terminal_error_message,''),
created_at, updated_at, started_at, terminal_at, commands_closed_at`

const documentInstanceColumns = `
id::text, instance_name, slot_path, status, COALESCE(allocated_job_id::text,''),
created_at, updated_at, ready_at, allocated_at, destroy_at`

const documentCommandColumns = `
id::text, job_id::text, command_id, payload, status, generation,
created_at, updated_at, started_at, completed_at`

func (s *Store) GetDocumentPoolStatus(ctx context.Context) (domain.DocumentPoolStatus, error) {
	var status domain.DocumentPoolStatus
	err := s.pool.QueryRow(ctx, `
SELECT
 (SELECT count(*) FROM document_jobs WHERE status='queued'),
 (SELECT count(*) FROM document_jobs WHERE status='starting'),
 (SELECT count(*) FROM document_jobs WHERE status='running'),
 (SELECT count(*) FROM document_instances WHERE status='creating'),
 (SELECT count(*) FROM document_instances WHERE status='ready'),
 (SELECT count(*) FROM document_instances WHERE status='allocated'),
 (SELECT count(*) FROM document_instances WHERE status='running'),
 (SELECT count(*) FROM document_instances WHERE status='destroying'),
 (SELECT count(*) FROM document_instances WHERE status='lost'),
 (SELECT count(*) FROM document_commands WHERE status='pending'),
 (SELECT count(*) FROM document_commands WHERE status='executing'),
 (SELECT min(created_at) FROM document_jobs WHERE status='queued'),
 statement_timestamp()
`).Scan(
		&status.JobsQueued, &status.JobsStarting, &status.JobsRunning,
		&status.InstancesCreating, &status.InstancesReady, &status.InstancesAllocated,
		&status.InstancesRunning, &status.InstancesDestroying, &status.InstancesLost,
		&status.CommandsPending, &status.CommandsExecuting,
		&status.OldestQueuedAt, &status.ObservedAt,
	)
	if err != nil {
		return domain.DocumentPoolStatus{}, fmt.Errorf("get document pool status: %w", err)
	}
	return status, nil
}

func (s *Store) SubmitDocumentJob(ctx context.Context, cmd domain.SubmitDocumentJobCommand) (domain.DocumentJob, error) {
	workspaceID, err := uuid.Parse(strings.TrimSpace(cmd.WorkspaceID))
	if err != nil {
		return domain.DocumentJob{}, fmt.Errorf("workspace id must be a UUID: %w", err)
	}
	if cmd.RequesterUserID <= 0 {
		return domain.DocumentJob{}, fmt.Errorf("requester user id must be positive")
	}
	if strings.TrimSpace(cmd.IdempotencyKey) == "" || len(cmd.IdempotencyKey) > 256 {
		return domain.DocumentJob{}, fmt.Errorf("idempotency key is required and must be <= 256 bytes")
	}
	payload, payloadHash, err := canonicalDocumentPayload(cmd.Payload)
	if err != nil {
		return domain.DocumentJob{}, err
	}

	var job domain.DocumentJob
	err = s.withTx(ctx, func(tx pgx.Tx) error {
		var globalLimit, workspaceLimit int
		if err := tx.QueryRow(ctx, `
SELECT global_queue_limit, per_tenant_queue_limit
FROM document_pool_settings WHERE singleton = TRUE FOR UPDATE
`).Scan(&globalLimit, &workspaceLimit); err != nil {
			return fmt.Errorf("lock document pool settings: %w", err)
		}

		var ownerID int64
		var kind, teamID string
		if err := tx.QueryRow(ctx, `
SELECT owner_user_id, kind, COALESCE(team_id::text,'')
FROM workspaces WHERE id=$1 FOR UPDATE
`, workspaceID).Scan(&ownerID, &kind, &teamID); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("workspace not found: %s", workspaceID)
			}
			return err
		}
		if err := authorizeSubmitter(tx, ctx, kind, ownerID, teamID, cmd.RequesterUserID); err != nil {
			return err
		}

		existing, existingHash, err := getDocumentJobByIdempotencyTx(ctx, tx, workspaceID, cmd.IdempotencyKey)
		if err == nil {
			if existingHash != payloadHash || existing.RequesterUserID != cmd.RequesterUserID {
				return domain.ErrDocumentIdempotencyConflict
			}
			job = existing
			return nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}

		var globalQueued, workspaceQueued int
		if err := tx.QueryRow(ctx, `
SELECT count(*), count(*) FILTER (WHERE workspace_id=$1)
FROM document_jobs WHERE status='queued'
`, workspaceID).Scan(&globalQueued, &workspaceQueued); err != nil {
			return err
		}
		if globalQueued >= globalLimit {
			return domain.ErrDocumentGlobalQueueFull
		}
		if workspaceQueued >= workspaceLimit {
			return domain.ErrDocumentWorkspaceQueueFull
		}

		row := tx.QueryRow(ctx, `
INSERT INTO document_jobs(
 id,workspace_id,requester_user_id,idempotency_key,payload,payload_hash,status
) VALUES($1,$2,$3,$4,$5,$6,'queued')
RETURNING `+documentJobColumns,
			uuid.NewString(), workspaceID, cmd.RequesterUserID, cmd.IdempotencyKey, payload, payloadHash)
		stored, err := scanDocumentJob(row)
		if err != nil {
			return err
		}
		job = stored
		return nil
	})
	return job, err
}

func (s *Store) CreateReadyDocumentInstance(ctx context.Context, instanceName, slotPath string) (domain.DocumentInstance, error) {
	instanceName = strings.TrimSpace(instanceName)
	slotPath = strings.TrimSpace(slotPath)
	if instanceName == "" || slotPath == "" {
		return domain.DocumentInstance{}, fmt.Errorf("instance name and slot path are required")
	}
	row := s.pool.QueryRow(ctx, `
INSERT INTO document_instances(id,instance_name,slot_path,status,ready_at)
VALUES($1,$2,$3,'ready',timezone('utc',now()))
RETURNING `+documentInstanceColumns, uuid.NewString(), instanceName, slotPath)
	return scanDocumentInstance(row)
}

// ReserveDocumentInstance records a durable creating intent before any runtime
// side effect. The settings lock serializes reconcilers so they cannot exceed
// either the warm target or the mutable/deployment hard capacity.
func (s *Store) ReserveDocumentInstance(ctx context.Context, instanceName, slotPath string) (domain.DocumentInstance, bool, error) {
	instanceName = strings.TrimSpace(instanceName)
	slotPath = strings.TrimSpace(slotPath)
	if instanceName == "" || slotPath == "" {
		return domain.DocumentInstance{}, false, fmt.Errorf("instance name and slot path are required")
	}
	var instance domain.DocumentInstance
	var reserved bool
	err := s.withTx(ctx, func(tx pgx.Tx) error {
		var enabled bool
		var minReady, maxActive int
		if err := tx.QueryRow(ctx, `
SELECT enabled,min_ready,max_active
FROM document_pool_settings WHERE singleton=TRUE FOR UPDATE
`).Scan(&enabled, &minReady, &maxActive); err != nil {
			return fmt.Errorf("lock document pool settings: %w", err)
		}
		if maxActive > s.documentPoolDeploymentMaxActive {
			return fmt.Errorf(
				"persisted document pool settings violate deployment policy: max_active %d exceeds deployment maximum %d",
				maxActive, s.documentPoolDeploymentMaxActive,
			)
		}
		if !enabled || minReady == 0 || maxActive == 0 {
			return nil
		}
		var warm, live int
		if err := tx.QueryRow(ctx, `
SELECT count(*) FILTER (WHERE status IN ('creating','ready')),
       count(*) FILTER (WHERE status IN ('creating','ready','allocated','running','destroying'))
FROM document_instances
`).Scan(&warm, &live); err != nil {
			return err
		}
		if warm >= minReady || live >= maxActive || live >= s.documentPoolDeploymentMaxActive {
			return nil
		}
		stored, err := scanDocumentInstance(tx.QueryRow(ctx, `
INSERT INTO document_instances(id,instance_name,slot_path,status)
VALUES($1,$2,$3,'creating')
RETURNING `+documentInstanceColumns, uuid.NewString(), instanceName, slotPath))
		if err != nil {
			return err
		}
		instance, reserved = stored, true
		return nil
	})
	return instance, reserved, err
}

func (s *Store) MarkDocumentInstanceReady(ctx context.Context, instanceID string) (domain.DocumentInstance, error) {
	instance, err := scanDocumentInstance(s.pool.QueryRow(ctx, `
UPDATE document_instances
SET status='ready',ready_at=timezone('utc',now()),updated_at=timezone('utc',now())
WHERE id=$1 AND status='creating' AND allocated_job_id IS NULL
RETURNING `+documentInstanceColumns, instanceID))
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.DocumentInstance{}, domain.ErrDocumentJobState
	}
	return instance, err
}

func (s *Store) MarkDocumentInstanceDestroying(ctx context.Context, instanceID string) (domain.DocumentInstance, error) {
	instance, err := scanDocumentInstance(s.pool.QueryRow(ctx, `
UPDATE document_instances
SET status='destroying',destroy_at=COALESCE(destroy_at,timezone('utc',now())),updated_at=timezone('utc',now())
WHERE id=$1 AND (
 (status IN ('creating','ready') AND allocated_job_id IS NULL)
 OR status='destroying'
)
RETURNING `+documentInstanceColumns, instanceID))
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.DocumentInstance{}, domain.ErrDocumentJobState
	}
	return instance, err
}

// RecoverCreatingDocumentInstances turns every uncertain runtime creation into
// cleanup intent. Runtime deletion remains a separate, retryable operation.
func (s *Store) RecoverCreatingDocumentInstances(ctx context.Context) (int, error) {
	tag, err := s.pool.Exec(ctx, `
UPDATE document_instances
SET status='destroying',destroy_at=COALESCE(destroy_at,timezone('utc',now())),updated_at=timezone('utc',now())
WHERE status='creating'
`)
	return int(tag.RowsAffected()), err
}

// ReserveExcessReadyForDestroy atomically selects ready instances for cleanup.
// Disabled pools drain immediately; enabled pools retain min_ready and only
// clean excess instances older than ready_idle_ttl_seconds.
func (s *Store) ReserveExcessReadyForDestroy(ctx context.Context) (int, error) {
	var reserved int
	err := s.withTx(ctx, func(tx pgx.Tx) error {
		var enabled bool
		var minReady, readyIdleTTL int
		if err := tx.QueryRow(ctx, `
SELECT enabled,min_ready,ready_idle_ttl_seconds
FROM document_pool_settings WHERE singleton=TRUE FOR UPDATE
`).Scan(&enabled, &minReady, &readyIdleTTL); err != nil {
			return fmt.Errorf("lock document pool settings: %w", err)
		}
		var ready int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM document_instances WHERE status='ready'`).Scan(&ready); err != nil {
			return err
		}
		excess := ready
		if enabled {
			excess = ready - minReady
		}
		if excess <= 0 {
			return nil
		}
		condition := ""
		args := []any{excess}
		if enabled {
			condition = "AND ready_at < timezone('utc',now())-$2*interval '1 second'"
			args = append(args, readyIdleTTL)
		}
		return tx.QueryRow(ctx, `
WITH selected AS (
 SELECT id FROM document_instances
 WHERE status='ready' `+condition+`
 ORDER BY ready_at,created_at,id
 FOR UPDATE SKIP LOCKED LIMIT $1
), updated AS (
 UPDATE document_instances i
 SET status='destroying',destroy_at=COALESCE(i.destroy_at,timezone('utc',now())),updated_at=timezone('utc',now())
 FROM selected WHERE i.id=selected.id AND i.status='ready'
 RETURNING 1
)
SELECT count(*) FROM updated
`, args...).Scan(&reserved)
	})
	return reserved, err
}

func (s *Store) ListDestroyingDocumentInstances(ctx context.Context) ([]domain.DocumentInstance, error) {
	rows, err := s.pool.Query(ctx, `
SELECT `+documentInstanceColumns+` FROM document_instances
WHERE status='destroying' ORDER BY updated_at,id
`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var instances []domain.DocumentInstance
	for rows.Next() {
		instance, err := scanDocumentInstance(rows)
		if err != nil {
			return nil, err
		}
		instances = append(instances, instance)
	}
	return instances, rows.Err()
}

func (s *Store) MarkDocumentInstanceDestroyed(ctx context.Context, instanceID string) (domain.DocumentInstance, error) {
	instance, err := scanDocumentInstance(s.pool.QueryRow(ctx, `
UPDATE document_instances
SET status='destroyed',updated_at=timezone('utc',now())
WHERE id=$1 AND status='destroying'
RETURNING `+documentInstanceColumns, instanceID))
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.DocumentInstance{}, domain.ErrDocumentJobState
	}
	return instance, err
}

// ClaimNextDocumentJob locks the singleton settings row, checks both active
// limits, chooses a workspace fairly, and binds one never-used ready instance.
// Every step occurs in this transaction; callers never perform count-then-claim.
func (s *Store) ClaimNextDocumentJob(ctx context.Context, owner string, lease time.Duration) (domain.DocumentClaim, bool, error) {
	owner = strings.TrimSpace(owner)
	if owner == "" {
		return domain.DocumentClaim{}, false, fmt.Errorf("owner is required")
	}
	if lease <= 0 {
		return domain.DocumentClaim{}, false, fmt.Errorf("lease must be positive")
	}
	leaseMicros := lease.Microseconds()
	if leaseMicros < 1 {
		leaseMicros = 1
	}

	var claim domain.DocumentClaim
	var claimed bool
	err := s.withTx(ctx, func(tx pgx.Tx) error {
		var enabled bool
		var globalActiveLimit, workspaceActiveLimit int
		if err := tx.QueryRow(ctx, `
SELECT enabled,max_active,per_tenant_active_limit
FROM document_pool_settings WHERE singleton=TRUE FOR UPDATE
`).Scan(&enabled, &globalActiveLimit, &workspaceActiveLimit); err != nil {
			return fmt.Errorf("lock document pool settings: %w", err)
		}
		if globalActiveLimit > s.documentPoolDeploymentMaxActive {
			return fmt.Errorf(
				"persisted document pool settings violate deployment policy: max_active %d exceeds deployment maximum %d",
				globalActiveLimit,
				s.documentPoolDeploymentMaxActive,
			)
		}
		if !enabled || globalActiveLimit == 0 || workspaceActiveLimit == 0 {
			return nil
		}
		var active int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM document_jobs WHERE status IN ('starting','running')`).Scan(&active); err != nil {
			return err
		}
		if active >= globalActiveLimit {
			return nil
		}

		var jobID string
		if err := tx.QueryRow(ctx, `
SELECT j.id::text
FROM document_jobs j
WHERE j.status='queued'
  AND (SELECT count(*) FROM document_jobs active
       WHERE active.workspace_id=j.workspace_id AND active.status IN ('starting','running')) < $1
ORDER BY COALESCE((SELECT max(history.claimed_at) FROM document_jobs history
                   WHERE history.workspace_id=j.workspace_id), '-infinity'::timestamptz),
         j.created_at, j.id
FOR UPDATE OF j SKIP LOCKED
LIMIT 1
`, workspaceActiveLimit).Scan(&jobID); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return nil
			}
			return err
		}

		var instanceID string
		if err := tx.QueryRow(ctx, `
SELECT id::text FROM document_instances
WHERE status='ready' AND allocated_job_id IS NULL
ORDER BY created_at,id
FOR UPDATE SKIP LOCKED LIMIT 1
`).Scan(&instanceID); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return nil
			}
			return err
		}

		instance, err := scanDocumentInstance(tx.QueryRow(ctx, `
UPDATE document_instances SET status='allocated',allocated_job_id=$2,
 allocated_at=timezone('utc',now()),updated_at=timezone('utc',now())
WHERE id=$1 AND status='ready' AND allocated_job_id IS NULL
RETURNING `+documentInstanceColumns, instanceID, jobID))
		if err != nil {
			return err
		}
		job, err := scanDocumentJob(tx.QueryRow(ctx, `
UPDATE document_jobs SET status='starting',instance_id=$2,claim_owner=$3,
 generation=generation+1,claim_lease_until=timezone('utc',now())+$4*interval '1 microsecond',
 claimed_at=timezone('utc',now()),last_activity_at=timezone('utc',now()),
 started_at=COALESCE(started_at,timezone('utc',now())),updated_at=timezone('utc',now())
WHERE id=$1 AND status='queued'
RETURNING `+documentJobColumns, jobID, instanceID, owner, leaseMicros))
		if err != nil {
			return err
		}
		claim = domain.DocumentClaim{Job: job, Instance: instance}
		claimed = true
		return nil
	})
	return claim, claimed, err
}

func (s *Store) HeartbeatDocumentJob(ctx context.Context, jobID, owner string, generation int64, lease time.Duration) error {
	if lease <= 0 {
		return fmt.Errorf("lease must be positive")
	}
	leaseMicros := lease.Microseconds()
	if leaseMicros < 1 {
		leaseMicros = 1
	}
	tag, err := s.pool.Exec(ctx, `
UPDATE document_jobs SET claim_lease_until=timezone('utc',now())+$4*interval '1 microsecond',
 updated_at=timezone('utc',now())
WHERE id=$1 AND claim_owner=$2 AND generation=$3 AND status IN ('starting','running')
 AND claim_lease_until > timezone('utc',now())
`, jobID, owner, generation, leaseMicros)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return domain.ErrDocumentFenceLost
	}
	return nil
}

func (s *Store) MarkDocumentJobAndInstanceRunning(ctx context.Context, jobID, owner string, generation int64) (domain.DocumentJob, error) {
	var job domain.DocumentJob
	err := s.withTx(ctx, func(tx pgx.Tx) error {
		status, err := lockActiveDocumentJobFence(ctx, tx, jobID, owner, generation)
		if err != nil {
			return err
		}
		if status != domain.DocumentJobStarting {
			return domain.ErrDocumentFenceLost
		}
		tag, err := tx.Exec(ctx, `
UPDATE document_instances i
SET status='running',updated_at=timezone('utc',now())
FROM document_jobs j
WHERE j.id=$1 AND i.id=j.instance_id AND i.allocated_job_id=j.id AND i.status='allocated'
`, jobID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return domain.ErrDocumentFenceLost
		}
		stored, err := scanDocumentJob(tx.QueryRow(ctx, `
UPDATE document_jobs SET status='running',last_activity_at=timezone('utc',now()),updated_at=timezone('utc',now())
WHERE id=$1 AND claim_owner=$2 AND generation=$3 AND status='starting'
 AND claim_lease_until > timezone('utc',now())
RETURNING `+documentJobColumns, jobID, owner, generation))
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrDocumentFenceLost
		}
		if err != nil {
			return err
		}
		job = stored
		return nil
	})
	return job, err
}

func (s *Store) SubmitDocumentCommand(ctx context.Context, cmd domain.SubmitDocumentCommand) (domain.DocumentCommand, error) {
	if _, err := uuid.Parse(strings.TrimSpace(cmd.JobID)); err != nil {
		return domain.DocumentCommand{}, fmt.Errorf("job id must be a UUID: %w", err)
	}
	if cmd.RequesterUserID <= 0 {
		return domain.DocumentCommand{}, fmt.Errorf("requester user id must be positive")
	}
	if strings.TrimSpace(cmd.CommandID) == "" || len(cmd.CommandID) > 256 {
		return domain.DocumentCommand{}, fmt.Errorf("command id is required and must be <= 256 bytes")
	}
	if cmd.Operation.SchemaVersion != 1 || strings.TrimSpace(cmd.Operation.Operation) == "" || len(cmd.Operation.Operation) > 128 {
		return domain.DocumentCommand{}, fmt.Errorf("operation requires schema_version 1 and a name <= 128 bytes")
	}
	if len(cmd.Operation.Parameters) == 0 || !json.Valid(cmd.Operation.Parameters) {
		return domain.DocumentCommand{}, fmt.Errorf("operation parameters must be valid JSON")
	}
	encoded, err := json.Marshal(cmd.Operation)
	if err != nil {
		return domain.DocumentCommand{}, fmt.Errorf("encode document operation: %w", err)
	}
	payload, payloadHash, err := canonicalDocumentPayload(encoded)
	if err != nil {
		return domain.DocumentCommand{}, err
	}
	var command domain.DocumentCommand
	err = s.withTx(ctx, func(tx pgx.Tx) error {
		jobStatus, commandsClosedAt, err := lockDocumentJobForRequester(ctx, tx, cmd.JobID, cmd.RequesterUserID)
		if err != nil {
			return err
		}
		var existingHash string
		existing, err := scanDocumentCommandWithHash(tx.QueryRow(ctx, `
SELECT `+documentCommandColumns+`,payload_hash FROM document_commands
WHERE job_id=$1 AND command_id=$2 FOR UPDATE
`, cmd.JobID, cmd.CommandID), &existingHash)
		if err == nil {
			if existingHash != payloadHash {
				return domain.ErrDocumentIdempotencyConflict
			}
			command = existing
			return nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if commandsClosedAt != nil {
			return domain.ErrDocumentCommandsClosed
		}
		if jobStatus != domain.DocumentJobQueued && jobStatus != domain.DocumentJobStarting && jobStatus != domain.DocumentJobRunning {
			return fmt.Errorf("%w: cannot submit command while job is %s", domain.ErrDocumentJobState, jobStatus)
		}
		stored, err := scanDocumentCommand(tx.QueryRow(ctx, `
INSERT INTO document_commands(id,job_id,command_id,payload,payload_hash,status)
VALUES($1,$2,$3,$4,$5,'pending') RETURNING `+documentCommandColumns,
			uuid.NewString(), cmd.JobID, cmd.CommandID, payload, payloadHash))
		if err != nil {
			return err
		}
		command = stored
		_, err = tx.Exec(ctx, `
UPDATE document_jobs SET last_activity_at=timezone('utc',now()),updated_at=timezone('utc',now())
WHERE id=$1 AND status IN ('starting','running')
`, cmd.JobID)
		return err
	})
	return command, err
}

func lockDocumentJobForRequester(ctx context.Context, tx pgx.Tx, jobID string, requester int64) (domain.DocumentJobStatus, *time.Time, error) {
	var status domain.DocumentJobStatus
	var commandsClosedAt *time.Time
	var ownerID int64
	var kind, teamID string
	err := tx.QueryRow(ctx, `
SELECT j.status,j.commands_closed_at,w.owner_user_id,w.kind,COALESCE(w.team_id::text,'')
FROM document_jobs j JOIN workspaces w ON w.id=j.workspace_id
WHERE j.id=$1 FOR UPDATE OF j
`, jobID).Scan(&status, &commandsClosedAt, &ownerID, &kind, &teamID)
	if err != nil {
		return "", nil, err
	}
	if err := authorizeSubmitter(tx, ctx, kind, ownerID, teamID, requester); err != nil {
		return "", nil, fmt.Errorf("%w: %v", domain.ErrDocumentUnauthorized, err)
	}
	return status, commandsClosedAt, nil
}

func (s *Store) CloseDocumentJobCommands(ctx context.Context, jobID string, requester int64) (domain.DocumentJob, error) {
	if _, err := uuid.Parse(strings.TrimSpace(jobID)); err != nil {
		return domain.DocumentJob{}, fmt.Errorf("job id must be a UUID: %w", err)
	}
	if requester <= 0 {
		return domain.DocumentJob{}, fmt.Errorf("requester user id must be positive")
	}
	var job domain.DocumentJob
	err := s.withTx(ctx, func(tx pgx.Tx) error {
		status, closedAt, err := lockDocumentJobForRequester(ctx, tx, jobID, requester)
		if err != nil {
			return err
		}
		if closedAt == nil && status.IsTerminal() {
			return fmt.Errorf("%w: cannot close commands while job is %s", domain.ErrDocumentJobState, status)
		}
		if closedAt == nil && status != domain.DocumentJobQueued && status != domain.DocumentJobStarting && status != domain.DocumentJobRunning {
			return fmt.Errorf("%w: cannot close commands while job is %s", domain.ErrDocumentJobState, status)
		}
		stored, err := scanDocumentJob(tx.QueryRow(ctx, `
WITH timestamp AS (SELECT clock_timestamp() AS value)
UPDATE document_jobs
SET commands_closed_at=COALESCE(commands_closed_at,timestamp.value),
 last_activity_at=CASE WHEN commands_closed_at IS NULL THEN timestamp.value ELSE last_activity_at END,
 updated_at=CASE WHEN commands_closed_at IS NULL THEN timestamp.value ELSE updated_at END
FROM timestamp
WHERE id=$1 RETURNING `+documentJobColumns, jobID))
		if err != nil {
			return err
		}
		job = stored
		return nil
	})
	return job, err
}

func (s *Store) ClaimNextDocumentCommand(ctx context.Context, jobID, owner string, generation int64) (domain.DocumentCommand, bool, error) {
	var command domain.DocumentCommand
	var claimed bool
	err := s.withTx(ctx, func(tx pgx.Tx) error {
		status, err := lockActiveDocumentJobFence(ctx, tx, jobID, owner, generation)
		if err != nil {
			return err
		}
		if status != domain.DocumentJobRunning {
			return domain.ErrDocumentFenceLost
		}
		var commandID string
		if err := tx.QueryRow(ctx, `
SELECT command_id FROM document_commands
WHERE job_id=$1 AND status='pending'
ORDER BY created_at,id FOR UPDATE SKIP LOCKED LIMIT 1
`, jobID).Scan(&commandID); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return nil
			}
			return err
		}
		stored, err := scanDocumentCommand(tx.QueryRow(ctx, `
UPDATE document_commands SET status='executing',generation=$3,
 started_at=timezone('utc',now()),updated_at=timezone('utc',now())
WHERE job_id=$1 AND command_id=$2 AND status='pending'
RETURNING `+documentCommandColumns, jobID, commandID, generation))
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE document_jobs SET last_activity_at=timezone('utc',now()),updated_at=timezone('utc',now()) WHERE id=$1`, jobID); err != nil {
			return err
		}
		command, claimed = stored, true
		return nil
	})
	return command, claimed, err
}

func (s *Store) StartDocumentCommand(ctx context.Context, jobID, commandID, owner string, generation int64) (domain.DocumentCommand, error) {
	var command domain.DocumentCommand
	err := s.withTx(ctx, func(tx pgx.Tx) error {
		jobStatus, err := lockActiveDocumentJobFence(ctx, tx, jobID, owner, generation)
		if err != nil {
			return err
		}
		if jobStatus != domain.DocumentJobRunning {
			return domain.ErrDocumentFenceLost
		}
		stored, err := scanDocumentCommand(tx.QueryRow(ctx, `
UPDATE document_commands SET status='executing',generation=$3,
 started_at=timezone('utc',now()),updated_at=timezone('utc',now())
WHERE job_id=$1 AND command_id=$2 AND status='pending'
 AND id=(SELECT id FROM document_commands
         WHERE job_id=$1 AND status='pending' ORDER BY created_at,id LIMIT 1)
RETURNING `+documentCommandColumns, jobID, commandID, generation))
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrDocumentFenceLost
		}
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
UPDATE document_jobs SET last_activity_at=timezone('utc',now()),updated_at=timezone('utc',now()) WHERE id=$1
`, jobID); err != nil {
			return err
		}
		command = stored
		return nil
	})
	return command, err
}

func (s *Store) CompleteDocumentCommand(ctx context.Context, jobID, commandID, owner string, generation int64, status domain.DocumentCommandStatus) (domain.DocumentCommand, error) {
	if status != domain.DocumentCommandSucceeded && status != domain.DocumentCommandFailed {
		return domain.DocumentCommand{}, fmt.Errorf("document command completion status must be succeeded or failed")
	}
	var command domain.DocumentCommand
	err := s.withTx(ctx, func(tx pgx.Tx) error {
		jobStatus, err := lockActiveDocumentJobFence(ctx, tx, jobID, owner, generation)
		if err != nil {
			return err
		}
		if jobStatus != domain.DocumentJobRunning {
			return domain.ErrDocumentFenceLost
		}
		stored, err := scanDocumentCommand(tx.QueryRow(ctx, `
UPDATE document_commands SET status=$4,completed_at=timezone('utc',now()),updated_at=timezone('utc',now())
WHERE job_id=$1 AND command_id=$2 AND status='executing' AND generation=$3
RETURNING `+documentCommandColumns, jobID, commandID, generation, string(status)))
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrDocumentFenceLost
		}
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
UPDATE document_jobs SET last_activity_at=timezone('utc',now()),updated_at=timezone('utc',now()) WHERE id=$1
`, jobID); err != nil {
			return err
		}
		command = stored
		return nil
	})
	return command, err
}

func lockActiveDocumentJobFence(ctx context.Context, tx pgx.Tx, jobID, owner string, generation int64) (domain.DocumentJobStatus, error) {
	var status domain.DocumentJobStatus
	err := tx.QueryRow(ctx, `
SELECT status FROM document_jobs
WHERE id=$1 AND claim_owner=$2 AND generation=$3 AND status IN ('starting','running')
 AND claim_lease_until > timezone('utc',now())
FOR UPDATE
`, jobID, owner, generation).Scan(&status)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", domain.ErrDocumentFenceLost
	}
	return status, err
}

func (s *Store) FinalizeDocumentJob(ctx context.Context, jobID, owner string, generation int64, status domain.DocumentJobStatus, errorCode, errorMessage string) (domain.DocumentJob, error) {
	if !status.IsTerminal() {
		return domain.DocumentJob{}, fmt.Errorf("document job status is not terminal: %s", status)
	}
	var job domain.DocumentJob
	err := s.withTx(ctx, func(tx pgx.Tx) error {
		if _, err := lockActiveDocumentJobFence(ctx, tx, jobID, owner, generation); err != nil {
			return err
		}
		if status == domain.DocumentJobSucceeded {
			var commandsClosedAt *time.Time
			var unfinished, failed bool
			if err := tx.QueryRow(ctx, `
SELECT j.commands_closed_at,
 EXISTS(SELECT 1 FROM document_commands WHERE job_id=j.id AND status IN ('pending','executing')),
 EXISTS(SELECT 1 FROM document_commands WHERE job_id=j.id AND status IN ('failed','expired'))
FROM document_jobs j WHERE j.id=$1
`, jobID).Scan(&commandsClosedAt, &unfinished, &failed); err != nil {
				return err
			}
			if commandsClosedAt == nil {
				return domain.ErrDocumentCommandsNotClosed
			}
			if unfinished {
				return domain.ErrDocumentCommandsPending
			}
			if failed {
				return domain.ErrDocumentCommandsFailed
			}
		} else {
			commandStatus := domain.DocumentCommandFailed
			if status == domain.DocumentJobExpired {
				commandStatus = domain.DocumentCommandExpired
			}
			if _, err := tx.Exec(ctx, `
UPDATE document_commands SET status=$2,completed_at=timezone('utc',now()),updated_at=timezone('utc',now())
WHERE job_id=$1 AND status IN ('pending','executing')
`, jobID, string(commandStatus)); err != nil {
				return err
			}
		}
		stored, err := scanDocumentJob(tx.QueryRow(ctx, `
UPDATE document_jobs SET status=$4,claim_owner=NULL,claim_lease_until=NULL,
 commands_closed_at=COALESCE(commands_closed_at,timezone('utc',now())),
 terminal_error_code=NULLIF($5,''),terminal_error_message=NULLIF($6,''),
 terminal_at=timezone('utc',now()),updated_at=timezone('utc',now())
WHERE id=$1 AND claim_owner=$2 AND generation=$3 AND status IN ('starting','running')
 AND claim_lease_until > timezone('utc',now())
RETURNING `+documentJobColumns, jobID, owner, generation, string(status), errorCode, errorMessage))
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrDocumentFenceLost
		}
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
UPDATE document_instances SET status='destroying',destroy_at=COALESCE(destroy_at,timezone('utc',now())),
 updated_at=timezone('utc',now()) WHERE id=$1 AND allocated_job_id=$2
`, stored.InstanceID, stored.ID); err != nil {
			return err
		}
		job = stored
		return nil
	})
	return job, err
}

// SweepExpiredDocumentWork terminalizes stale work and records destroy intent.
// It never returns a job to queued and never replays an executing command.
func (s *Store) SweepExpiredDocumentWork(ctx context.Context) (domain.DocumentSweepResult, error) {
	var result domain.DocumentSweepResult
	err := s.withTx(ctx, func(tx pgx.Tx) error {
		var jobTimeout, idleTimeout, commandTimeout int
		if err := tx.QueryRow(ctx, `
SELECT job_timeout_seconds,job_idle_ttl_seconds,command_timeout_seconds
FROM document_pool_settings WHERE singleton=TRUE FOR UPDATE
`).Scan(&jobTimeout, &idleTimeout, &commandTimeout); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `
WITH expired AS (
 UPDATE document_commands SET status='expired',completed_at=timezone('utc',now()),updated_at=timezone('utc',now())
 WHERE status='executing' AND started_at < timezone('utc',now())-$1*interval '1 second'
 RETURNING 1
) SELECT count(*) FROM expired
`, commandTimeout).Scan(&result.CommandsExpired); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `
WITH expired_jobs AS (
 SELECT id FROM document_jobs
 WHERE status='queued' AND created_at < timezone('utc',now())-$1*interval '1 second'
 FOR UPDATE
), expired_commands AS (
 UPDATE document_commands c
 SET status='expired',completed_at=timezone('utc',now()),updated_at=timezone('utc',now())
 FROM expired_jobs j
 WHERE c.job_id=j.id AND c.status IN ('pending','executing')
 RETURNING c.id
), expired AS (
 UPDATE document_jobs j
 SET status='expired',terminal_at=timezone('utc',now()),updated_at=timezone('utc',now()),
  commands_closed_at=COALESCE(j.commands_closed_at,timezone('utc',now())),
  terminal_error_code='DOCUMENT_QUEUE_TIMEOUT',terminal_error_message='document job expired in queue'
 FROM expired_jobs selected
 WHERE j.id=selected.id
 RETURNING 1
) SELECT count(*) FROM expired
`, jobTimeout).Scan(&result.QueuedExpired); err != nil {
			return err
		}

		rows, err := tx.Query(ctx, `
SELECT j.id::text,j.instance_id::text
FROM document_jobs j
WHERE j.status IN ('starting','running') AND (
 j.claim_lease_until < timezone('utc',now())
 OR j.started_at < timezone('utc',now())-$1*interval '1 second'
 OR j.last_activity_at < timezone('utc',now())-$2*interval '1 second'
 OR EXISTS (SELECT 1 FROM document_commands c WHERE c.job_id=j.id AND c.status='expired')
)
FOR UPDATE OF j
`, jobTimeout, idleTimeout)
		if err != nil {
			return err
		}
		type staleBinding struct{ jobID, instanceID string }
		var stale []staleBinding
		for rows.Next() {
			var binding staleBinding
			if err := rows.Scan(&binding.jobID, &binding.instanceID); err != nil {
				rows.Close()
				return err
			}
			stale = append(stale, binding)
		}
		rows.Close()
		for _, binding := range stale {
			if _, err := tx.Exec(ctx, `
UPDATE document_commands SET status='expired',completed_at=timezone('utc',now()),updated_at=timezone('utc',now())
WHERE job_id=$1 AND status IN ('pending','executing')
`, binding.jobID); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `
UPDATE document_jobs SET status='failed',claim_owner=NULL,claim_lease_until=NULL,
 commands_closed_at=COALESCE(commands_closed_at,timezone('utc',now())),
 generation=generation+1,terminal_at=timezone('utc',now()),updated_at=timezone('utc',now()),
 terminal_error_code='DOCUMENT_EXECUTION_EXPIRED',terminal_error_message='document execution lease or TTL expired'
WHERE id=$1 AND status IN ('starting','running')
`, binding.jobID); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `
UPDATE document_instances SET status='destroying',destroy_at=COALESCE(destroy_at,timezone('utc',now())),
 updated_at=timezone('utc',now()) WHERE id=$1 AND allocated_job_id=$2
`, binding.instanceID, binding.jobID); err != nil {
				return err
			}
		}
		result.JobsFailed = len(stale)
		return nil
	})
	return result, err
}

func (s *Store) GetDocumentJob(ctx context.Context, jobID string) (domain.DocumentJob, error) {
	return scanDocumentJob(s.pool.QueryRow(ctx, `SELECT `+documentJobColumns+` FROM document_jobs WHERE id=$1`, jobID))
}

func (s *Store) GetDocumentInstance(ctx context.Context, instanceID string) (domain.DocumentInstance, error) {
	return scanDocumentInstance(s.pool.QueryRow(ctx, `SELECT `+documentInstanceColumns+` FROM document_instances WHERE id=$1`, instanceID))
}

func canonicalDocumentPayload(payload json.RawMessage) ([]byte, string, error) {
	if len(payload) == 0 || !json.Valid(payload) {
		return nil, "", fmt.Errorf("payload must be valid JSON")
	}
	var value any
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		return nil, "", fmt.Errorf("decode payload: %w", err)
	}
	canonical, err := json.Marshal(value)
	if err != nil {
		return nil, "", fmt.Errorf("encode payload: %w", err)
	}
	digest := sha256.Sum256(canonical)
	return canonical, hex.EncodeToString(digest[:]), nil
}

func getDocumentJobByIdempotencyTx(ctx context.Context, tx pgx.Tx, workspaceID uuid.UUID, key string) (domain.DocumentJob, string, error) {
	var hash string
	job, err := scanDocumentJobWithHash(tx.QueryRow(ctx, `
SELECT `+documentJobColumns+`,payload_hash FROM document_jobs
WHERE workspace_id=$1 AND idempotency_key=$2 FOR UPDATE
`, workspaceID, key), &hash)
	return job, hash, err
}

type documentScannable interface{ Scan(...any) error }

func scanDocumentJob(row documentScannable) (domain.DocumentJob, error) {
	return scanDocumentJobWithHash(row, nil)
}

func scanDocumentJobWithHash(row documentScannable, hash *string) (domain.DocumentJob, error) {
	var job domain.DocumentJob
	var payload []byte
	dest := []any{
		&job.ID, &job.WorkspaceID, &job.RequesterUserID, &job.IdempotencyKey, &payload,
		&job.Status, &job.InstanceID, &job.ClaimOwner, &job.Generation,
		&job.ClaimLeaseUntil, &job.ClaimedAt, &job.LastActivityAt,
		&job.TerminalErrorCode, &job.TerminalErrorMessage,
		&job.CreatedAt, &job.UpdatedAt, &job.StartedAt, &job.TerminalAt, &job.CommandsClosedAt,
	}
	if hash != nil {
		dest = append(dest, hash)
	}
	if err := row.Scan(dest...); err != nil {
		return domain.DocumentJob{}, err
	}
	job.Payload = append(json.RawMessage(nil), payload...)
	return job, nil
}

func scanDocumentInstance(row documentScannable) (domain.DocumentInstance, error) {
	var instance domain.DocumentInstance
	err := row.Scan(&instance.ID, &instance.InstanceName, &instance.SlotPath, &instance.Status,
		&instance.AllocatedJobID, &instance.CreatedAt, &instance.UpdatedAt,
		&instance.ReadyAt, &instance.AllocatedAt, &instance.DestroyAt)
	return instance, err
}

func scanDocumentCommand(row documentScannable) (domain.DocumentCommand, error) {
	return scanDocumentCommandWithHash(row, nil)
}

func scanDocumentCommandWithHash(row documentScannable, hash *string) (domain.DocumentCommand, error) {
	var command domain.DocumentCommand
	var payload []byte
	dest := []any{&command.ID, &command.JobID, &command.CommandID, &payload, &command.Status,
		&command.Generation, &command.CreatedAt, &command.UpdatedAt, &command.StartedAt, &command.CompletedAt}
	if hash != nil {
		dest = append(dest, hash)
	}
	if err := row.Scan(dest...); err != nil {
		return domain.DocumentCommand{}, err
	}
	command.Payload = append(json.RawMessage(nil), payload...)
	return command, nil
}
