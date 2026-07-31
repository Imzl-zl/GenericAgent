package postgres

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/domain"
)

func TestDocumentMigrationRegisteredAndConstrained(t *testing.T) {
	pool := requireDB(t)
	ctx := context.Background()

	found := false
	for _, name := range migrationFiles() {
		found = found || name == "0033_document_job_finish.sql"
	}
	if !found {
		t.Fatal("0033 migration is not registered")
	}
	for _, table := range []string{"document_jobs", "document_commands", "document_instances", "migration_0032_document_job_pool_marker", "migration_0033_document_job_finish_marker"} {
		var exists bool
		if err := pool.QueryRow(ctx, `SELECT to_regclass('public.' || $1) IS NOT NULL`, table).Scan(&exists); err != nil || !exists {
			t.Fatalf("table %s exists=%v err=%v", table, exists, err)
		}
	}

	workspaceID := seedDocumentWorkspace(t, pool, 11)
	assertDocumentConstraint(t, pool, `
INSERT INTO document_jobs(id,workspace_id,requester_user_id,idempotency_key,payload,payload_hash,status)
VALUES($1,$2,11,'bad-status','{}','hash','invalid')`, uuid.NewString(), workspaceID)
	assertDocumentConstraint(t, pool, `
INSERT INTO document_instances(id,instance_name,slot_path,status,allocated_job_id)
VALUES($1,$2,$3,'ready',$4)`, uuid.NewString(), "ready-bound", "/slots/ready-bound", uuid.NewString())
	creatingID := uuid.NewString()
	if _, err := pool.Exec(ctx, `
INSERT INTO document_instances(id,instance_name,slot_path,status)
VALUES($1,$2,$3,'creating')`, creatingID, "creating-unbound", "/slots/creating-unbound"); err != nil {
		t.Fatalf("creating intent must be unbound: %v", err)
	}
	assertDocumentConstraint(t, pool, `
INSERT INTO document_instances(id,instance_name,slot_path,status)
VALUES($1,$2,$3,'allocated')`, uuid.NewString(), "allocated-unbound", "/slots/allocated-unbound")
	assertDocumentConstraint(t, pool, `
INSERT INTO document_instances(id,instance_name,slot_path,status)
VALUES($1,$2,$3,'running')`, uuid.NewString(), "running-unbound", "/slots/running-unbound")
	assertDocumentConstraint(t, pool, `
INSERT INTO document_commands(id,job_id,command_id,payload,payload_hash,status)
VALUES($1,$2,'bad-status','{}','hash','invalid')`, uuid.NewString(), uuid.NewString())
	assertDocumentConstraint(t, pool, `
INSERT INTO document_jobs(id,workspace_id,requester_user_id,idempotency_key,payload,payload_hash,status,terminal_at)
VALUES($1,$2,11,'success-without-close','{}','hash','succeeded',now())`, uuid.NewString(), workspaceID)
}

func TestDocumentSubmitDedupeAndQueueCapacity(t *testing.T) {
	pool := requireDB(t)
	store := newDocumentTestStore(t, pool, 4)
	ctx := context.Background()
	workspaceID := seedDocumentWorkspace(t, pool, 21)
	setDocumentPoolSettings(t, pool, true, 4, 2, 1, 1)

	cmd := domain.SubmitDocumentJobCommand{WorkspaceID: workspaceID, RequesterUserID: 21, IdempotencyKey: "request-1", Payload: []byte(`{"file":"a.pdf"}`)}
	first, err := store.SubmitDocumentJob(ctx, cmd)
	if err != nil {
		t.Fatal(err)
	}
	repeated, err := store.SubmitDocumentJob(ctx, cmd)
	if err != nil || repeated.ID != first.ID {
		t.Fatalf("idempotent submit got=%+v err=%v", repeated, err)
	}
	mismatch := cmd
	mismatch.Payload = []byte(`{"file":"different.pdf"}`)
	if _, err := store.SubmitDocumentJob(ctx, mismatch); !errors.Is(err, domain.ErrDocumentIdempotencyConflict) {
		t.Fatalf("payload mismatch err=%v", err)
	}
	if _, err := store.SubmitDocumentJob(ctx, domain.SubmitDocumentJobCommand{WorkspaceID: workspaceID, RequesterUserID: 21, IdempotencyKey: "request-2", Payload: []byte(`{}`)}); !errors.Is(err, domain.ErrDocumentWorkspaceQueueFull) {
		t.Fatalf("workspace cap err=%v", err)
	}
	workspace2 := seedDocumentWorkspace(t, pool, 22)
	if _, err := store.SubmitDocumentJob(ctx, domain.SubmitDocumentJobCommand{WorkspaceID: workspace2, RequesterUserID: 22, IdempotencyKey: "request-3", Payload: []byte(`{}`)}); err != nil {
		t.Fatal(err)
	}
	workspace3 := seedDocumentWorkspace(t, pool, 23)
	if _, err := store.SubmitDocumentJob(ctx, domain.SubmitDocumentJobCommand{WorkspaceID: workspace3, RequesterUserID: 23, IdempotencyKey: "request-4", Payload: []byte(`{}`)}); !errors.Is(err, domain.ErrDocumentGlobalQueueFull) {
		t.Fatalf("global cap err=%v", err)
	}
}

func TestDocumentPayloadDedupePreservesLargeIntegerPrecision(t *testing.T) {
	pool := requireDB(t)
	store := newDocumentTestStore(t, pool, 2)
	ctx := context.Background()
	workspaceID := seedDocumentWorkspace(t, pool, 24)
	setDocumentPoolSettings(t, pool, true, 2, 10, 10, 2)

	job, err := store.SubmitDocumentJob(ctx, domain.SubmitDocumentJobCommand{
		WorkspaceID: workspaceID, RequesterUserID: 24, IdempotencyKey: "large-job",
		Payload: []byte(`{"value":9007199254740992}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.SubmitDocumentJob(ctx, domain.SubmitDocumentJobCommand{
		WorkspaceID: workspaceID, RequesterUserID: 24, IdempotencyKey: "large-job",
		Payload: []byte(`{"value":9007199254740993}`),
	}); !errors.Is(err, domain.ErrDocumentIdempotencyConflict) {
		t.Fatalf("large job payload mismatch err=%v", err)
	}

	createReadyDocumentInstance(t, store, "large-payload-instance")
	claim, ok, err := store.ClaimNextDocumentJob(ctx, "large-manager", time.Minute)
	if err != nil || !ok || claim.Job.ID != job.ID {
		t.Fatalf("claim=%+v ok=%v err=%v", claim, ok, err)
	}
	if _, err := store.MarkDocumentJobAndInstanceRunning(ctx, job.ID, "large-manager", claim.Job.Generation); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SubmitDocumentCommand(ctx, documentCommand(job.ID, 24, "large-command", `{"value":9007199254740992}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SubmitDocumentCommand(ctx, documentCommand(job.ID, 24, "large-command", `{"value":9007199254740993}`)); !errors.Is(err, domain.ErrDocumentIdempotencyConflict) {
		t.Fatalf("large command payload mismatch err=%v", err)
	}
}

func TestDocumentClaimFairnessUniqueInstanceAndCapsAreAtomic(t *testing.T) {
	pool := requireDB(t)
	store := newDocumentTestStore(t, pool, 4)
	ctx := context.Background()
	workspaceA := seedDocumentWorkspace(t, pool, 31)
	workspaceB := seedDocumentWorkspace(t, pool, 32)
	setDocumentPoolSettings(t, pool, true, 2, 20, 2, 1)

	a1 := submitDocumentJob(t, store, workspaceA, 31, "a1")
	a2 := submitDocumentJob(t, store, workspaceA, 31, "a2")
	b1 := submitDocumentJob(t, store, workspaceB, 32, "b1")
	for i := 0; i < 4; i++ {
		createReadyDocumentInstance(t, store, fmt.Sprintf("instance-%d", i))
	}

	first, ok, err := store.ClaimNextDocumentJob(ctx, "manager-1", time.Minute)
	if err != nil || !ok || first.Job.ID != a1.ID {
		t.Fatalf("first claim=%+v ok=%v err=%v", first, ok, err)
	}
	second, ok, err := store.ClaimNextDocumentJob(ctx, "manager-2", time.Minute)
	if err != nil || !ok || second.Job.ID != b1.ID {
		t.Fatalf("fair second claim=%+v ok=%v err=%v", second, ok, err)
	}
	if first.Instance.ID == second.Instance.ID || first.Job.InstanceID != first.Instance.ID || second.Job.InstanceID != second.Instance.ID {
		t.Fatalf("instance allocation not unique/atomic: first=%+v second=%+v", first, second)
	}
	if _, ok, err := store.ClaimNextDocumentJob(ctx, "manager-3", time.Minute); err != nil || ok {
		t.Fatalf("global cap claim ok=%v err=%v", ok, err)
	}
	if _, err := store.CloseDocumentJobCommands(ctx, first.Job.ID, 31); err != nil {
		t.Fatal(err)
	}
	if _, err := store.FinalizeDocumentJob(ctx, first.Job.ID, "manager-1", first.Job.Generation, domain.DocumentJobSucceeded, "", ""); err != nil {
		t.Fatal(err)
	}
	third, ok, err := store.ClaimNextDocumentJob(ctx, "manager-3", time.Minute)
	if err != nil || !ok || third.Job.ID != a2.ID || third.Instance.ID == first.Instance.ID {
		t.Fatalf("third claim=%+v ok=%v err=%v", third, ok, err)
	}

	for _, item := range []struct {
		claim domain.DocumentClaim
		owner string
	}{{second, "manager-2"}, {third, "manager-3"}} {
		requester := int64(31)
		if item.claim.Job.ID == second.Job.ID {
			requester = 32
		}
		if _, err := store.CloseDocumentJobCommands(ctx, item.claim.Job.ID, requester); err != nil {
			t.Fatal(err)
		}
		if _, err := store.FinalizeDocumentJob(ctx, item.claim.Job.ID, item.owner, item.claim.Job.Generation, domain.DocumentJobSucceeded, "", ""); err != nil {
			t.Fatal(err)
		}
	}
	setDocumentPoolSettings(t, pool, true, 1, 20, 2, 1)
	submitDocumentJob(t, store, workspaceA, 31, "a3")
	submitDocumentJob(t, store, workspaceB, 32, "b2")
	var wg sync.WaitGroup
	results := make(chan domain.DocumentClaim, 2)
	errs := make(chan error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			claim, claimed, err := store.ClaimNextDocumentJob(ctx, fmt.Sprintf("concurrent-%d", i), time.Minute)
			if err != nil {
				errs <- err
			} else if claimed {
				results <- claim
			}
		}(i)
	}
	wg.Wait()
	if len(errs) != 0 || len(results) != 1 {
		t.Fatalf("concurrent claims errors=%d successes=%d, want 0/1", len(errs), len(results))
	}
	var active int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM document_jobs WHERE status IN ('starting','running')`).Scan(&active); err != nil || active != 1 {
		t.Fatalf("active=%d err=%v", active, err)
	}
}

func TestDocumentWorkspaceActiveCapIsAtomic(t *testing.T) {
	pool := requireDB(t)
	store := newDocumentTestStore(t, pool, 3)
	ctx := context.Background()
	workspaceID := seedDocumentWorkspace(t, pool, 35)
	setDocumentPoolSettings(t, pool, true, 3, 20, 3, 1)
	submitDocumentJob(t, store, workspaceID, 35, "workspace-a")
	submitDocumentJob(t, store, workspaceID, 35, "workspace-b")
	createReadyDocumentInstance(t, store, "workspace-instance-a")
	createReadyDocumentInstance(t, store, "workspace-instance-b")

	var wg sync.WaitGroup
	results := make(chan domain.DocumentClaim, 2)
	errs := make(chan error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			claim, claimed, err := store.ClaimNextDocumentJob(ctx, fmt.Sprintf("workspace-manager-%d", i), time.Minute)
			if err != nil {
				errs <- err
			} else if claimed {
				results <- claim
			}
		}(i)
	}
	wg.Wait()
	if len(errs) != 0 || len(results) != 1 {
		t.Fatalf("workspace concurrent claims errors=%d successes=%d, want 0/1", len(errs), len(results))
	}
	var active int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM document_jobs WHERE workspace_id=$1 AND status IN ('starting','running')`, workspaceID).Scan(&active); err != nil || active != 1 {
		t.Fatalf("workspace active=%d err=%v", active, err)
	}
}

func TestDocumentClaimRejectsSettingsAboveDeploymentHardMax(t *testing.T) {
	pool := requireDB(t)
	store := newDocumentTestStore(t, pool, 2)
	ctx := context.Background()
	workspaceID := seedDocumentWorkspace(t, pool, 36)
	setDocumentPoolSettings(t, pool, true, 2, 20, 2, 2)
	job := submitDocumentJob(t, store, workspaceID, 36, "hard-max")
	instance := createReadyDocumentInstance(t, store, "hard-max-instance")

	if _, err := pool.Exec(ctx, `UPDATE document_pool_settings SET max_active=3,per_tenant_active_limit=3 WHERE singleton`); err != nil {
		t.Fatal(err)
	}
	if _, claimed, err := store.ClaimNextDocumentJob(ctx, "manager", time.Minute); err == nil || claimed {
		t.Fatalf("claim above deployment hard max claimed=%v err=%v", claimed, err)
	}
	storedJob, err := store.GetDocumentJob(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	storedInstance, err := store.GetDocumentInstance(ctx, instance.ID)
	if err != nil {
		t.Fatal(err)
	}
	if storedJob.Status != domain.DocumentJobQueued || storedInstance.Status != domain.DocumentInstanceReady {
		t.Fatalf("claim mutated state: job=%s instance=%s", storedJob.Status, storedInstance.Status)
	}
}

func TestDocumentExpiredLeaseRejectsAllFencedWrites(t *testing.T) {
	pool := requireDB(t)
	store := newDocumentTestStore(t, pool, 3)
	ctx := context.Background()
	workspaceID := seedDocumentWorkspace(t, pool, 37)
	setDocumentPoolSettings(t, pool, true, 3, 20, 3, 3)

	startingJob := submitDocumentJob(t, store, workspaceID, 37, "expired-starting")
	createReadyDocumentInstance(t, store, "expired-starting-instance")
	startingClaim, ok, err := store.ClaimNextDocumentJob(ctx, "old-starting-manager", time.Minute)
	if err != nil || !ok || startingClaim.Job.ID != startingJob.ID {
		t.Fatalf("starting claim=%+v ok=%v err=%v", startingClaim, ok, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE document_jobs SET claim_lease_until=now()-interval '1 second' WHERE id=$1`, startingJob.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.HeartbeatDocumentJob(ctx, startingJob.ID, "old-starting-manager", startingClaim.Job.Generation, time.Minute); !errors.Is(err, domain.ErrDocumentFenceLost) {
		t.Fatalf("expired heartbeat err=%v", err)
	}
	if _, err := store.MarkDocumentJobAndInstanceRunning(ctx, startingJob.ID, "old-starting-manager", startingClaim.Job.Generation); !errors.Is(err, domain.ErrDocumentFenceLost) {
		t.Fatalf("expired mark running err=%v", err)
	}
	if _, err := store.FinalizeDocumentJob(ctx, startingJob.ID, "old-starting-manager", startingClaim.Job.Generation, domain.DocumentJobFailed, "EXPIRED", "expired"); !errors.Is(err, domain.ErrDocumentFenceLost) {
		t.Fatalf("expired starting finalize err=%v", err)
	}

	runningJob := submitDocumentJob(t, store, workspaceID, 37, "expired-running")
	createReadyDocumentInstance(t, store, "expired-running-instance")
	runningClaim, ok, err := store.ClaimNextDocumentJob(ctx, "old-running-manager", time.Minute)
	if err != nil || !ok || runningClaim.Job.ID != runningJob.ID {
		t.Fatalf("running claim=%+v ok=%v err=%v", runningClaim, ok, err)
	}
	if _, err := store.MarkDocumentJobAndInstanceRunning(ctx, runningJob.ID, "old-running-manager", runningClaim.Job.Generation); err != nil {
		t.Fatal(err)
	}
	for _, commandID := range []string{"executing", "pending"} {
		if _, err := store.SubmitDocumentCommand(ctx, documentCommand(runningJob.ID, 37, commandID, `{}`)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.StartDocumentCommand(ctx, runningJob.ID, "executing", "old-running-manager", runningClaim.Job.Generation); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE document_jobs SET claim_lease_until=now()-interval '1 second' WHERE id=$1`, runningJob.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.StartDocumentCommand(ctx, runningJob.ID, "pending", "old-running-manager", runningClaim.Job.Generation); !errors.Is(err, domain.ErrDocumentFenceLost) {
		t.Fatalf("expired start command err=%v", err)
	}
	if _, err := store.CompleteDocumentCommand(ctx, runningJob.ID, "executing", "old-running-manager", runningClaim.Job.Generation, domain.DocumentCommandSucceeded); !errors.Is(err, domain.ErrDocumentFenceLost) {
		t.Fatalf("expired complete command err=%v", err)
	}
	if _, err := store.FinalizeDocumentJob(ctx, runningJob.ID, "old-running-manager", runningClaim.Job.Generation, domain.DocumentJobSucceeded, "", ""); !errors.Is(err, domain.ErrDocumentFenceLost) {
		t.Fatalf("expired running finalize err=%v", err)
	}
}

func TestDocumentHeartbeatDoesNotMaskIdleTTL(t *testing.T) {
	pool := requireDB(t)
	store := newDocumentTestStore(t, pool, 1)
	ctx := context.Background()
	workspaceID := seedDocumentWorkspace(t, pool, 38)
	setDocumentPoolSettings(t, pool, true, 1, 10, 1, 1)
	job := submitDocumentJob(t, store, workspaceID, 38, "idle")
	createReadyDocumentInstance(t, store, "idle-instance")
	claim, ok, err := store.ClaimNextDocumentJob(ctx, "idle-manager", time.Minute)
	if err != nil || !ok || claim.Job.ID != job.ID {
		t.Fatalf("claim=%+v ok=%v err=%v", claim, ok, err)
	}
	if _, err := store.MarkDocumentJobAndInstanceRunning(ctx, job.ID, "idle-manager", claim.Job.Generation); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE document_jobs SET last_activity_at=now()-interval '10 seconds' WHERE id=$1`, job.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.HeartbeatDocumentJob(ctx, job.ID, "idle-manager", claim.Job.Generation, time.Minute); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE document_pool_settings SET job_idle_ttl_seconds=1 WHERE singleton`); err != nil {
		t.Fatal(err)
	}
	swept, err := store.SweepExpiredDocumentWork(ctx)
	if err != nil || swept.JobsFailed != 1 {
		t.Fatalf("sweep=%+v err=%v", swept, err)
	}
}

func TestDocumentCommandCompletionAndTerminalConsistency(t *testing.T) {
	pool := requireDB(t)
	store := newDocumentTestStore(t, pool, 1)
	ctx := context.Background()
	workspaceID := seedDocumentWorkspace(t, pool, 39)
	setDocumentPoolSettings(t, pool, true, 1, 10, 1, 1)
	job := submitDocumentJob(t, store, workspaceID, 39, "completion")
	createReadyDocumentInstance(t, store, "completion-instance")
	claim, ok, err := store.ClaimNextDocumentJob(ctx, "completion-manager", time.Minute)
	if err != nil || !ok || claim.Job.ID != job.ID {
		t.Fatalf("claim=%+v ok=%v err=%v", claim, ok, err)
	}
	if _, err := store.MarkDocumentJobAndInstanceRunning(ctx, job.ID, "completion-manager", claim.Job.Generation); err != nil {
		t.Fatal(err)
	}
	for _, commandID := range []string{"completed", "unfinished"} {
		if _, err := store.SubmitDocumentCommand(ctx, documentCommand(job.ID, 39, commandID, `{}`)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.StartDocumentCommand(ctx, job.ID, "completed", "completion-manager", claim.Job.Generation); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CompleteDocumentCommand(ctx, job.ID, "completed", "wrong-manager", claim.Job.Generation, domain.DocumentCommandSucceeded); !errors.Is(err, domain.ErrDocumentFenceLost) {
		t.Fatalf("wrong owner complete err=%v", err)
	}
	completed, err := store.CompleteDocumentCommand(ctx, job.ID, "completed", "completion-manager", claim.Job.Generation, domain.DocumentCommandSucceeded)
	if err != nil || completed.Status != domain.DocumentCommandSucceeded {
		t.Fatalf("completed=%+v err=%v", completed, err)
	}
	if _, err := store.CloseDocumentJobCommands(ctx, job.ID, 39); err != nil {
		t.Fatal(err)
	}
	if _, err := store.FinalizeDocumentJob(ctx, job.ID, "completion-manager", claim.Job.Generation, domain.DocumentJobSucceeded, "", ""); !errors.Is(err, domain.ErrDocumentCommandsPending) {
		t.Fatalf("success with unfinished command err=%v", err)
	}
	final, err := store.FinalizeDocumentJob(ctx, job.ID, "completion-manager", claim.Job.Generation, domain.DocumentJobFailed, "COMMAND_FAILED", "command failed")
	if err != nil || final.Status != domain.DocumentJobFailed {
		t.Fatalf("final=%+v err=%v", final, err)
	}
	var unfinishedStatus domain.DocumentCommandStatus
	var completedAt *time.Time
	if err := pool.QueryRow(ctx, `SELECT status,completed_at FROM document_commands WHERE job_id=$1 AND command_id='unfinished'`, job.ID).Scan(&unfinishedStatus, &completedAt); err != nil {
		t.Fatal(err)
	}
	if unfinishedStatus != domain.DocumentCommandFailed || completedAt == nil {
		t.Fatalf("unfinished status=%s completed_at=%v", unfinishedStatus, completedAt)
	}
}

func TestDocumentCommandCloseAuthorizationAndSuccessfulFinalize(t *testing.T) {
	pool := requireDB(t)
	store := newDocumentTestStore(t, pool, 1)
	ctx := context.Background()
	workspaceID := seedDocumentWorkspace(t, pool, 61)
	if _, err := pool.Exec(ctx, `INSERT INTO users(id,username,status) VALUES(62,'doc-user-62','approved')`); err != nil {
		t.Fatal(err)
	}
	setDocumentPoolSettings(t, pool, true, 1, 10, 1, 1)
	job, claim := startRunningDocumentJob(t, store, workspaceID, 61, "close-success", "close-success-instance", "close-manager")
	commandInput := documentCommand(job.ID, 61, "command-1", `{"value":9007199254740993}`)

	if _, err := store.SubmitDocumentCommand(ctx, documentCommand(job.ID, 62, "unauthorized", `{}`)); !errors.Is(err, domain.ErrDocumentUnauthorized) {
		t.Fatalf("unauthorized submit err=%v", err)
	}
	if _, err := store.CloseDocumentJobCommands(ctx, job.ID, 62); !errors.Is(err, domain.ErrDocumentUnauthorized) {
		t.Fatalf("unauthorized close err=%v", err)
	}
	stored, err := store.SubmitDocumentCommand(ctx, commandInput)
	if err != nil {
		t.Fatal(err)
	}
	closed, err := store.CloseDocumentJobCommands(ctx, job.ID, 61)
	if err != nil || closed.CommandsClosedAt == nil {
		t.Fatalf("closed=%+v err=%v", closed, err)
	}
	closedAgain, err := store.CloseDocumentJobCommands(ctx, job.ID, 61)
	if err != nil || closedAgain.CommandsClosedAt == nil || !closedAgain.CommandsClosedAt.Equal(*closed.CommandsClosedAt) {
		t.Fatalf("idempotent close first=%v second=%v err=%v", closed.CommandsClosedAt, closedAgain.CommandsClosedAt, err)
	}
	if _, err := store.SubmitDocumentCommand(ctx, documentCommand(job.ID, 61, "after-close", `{}`)); !errors.Is(err, domain.ErrDocumentCommandsClosed) {
		t.Fatalf("new command after close err=%v", err)
	}
	repeated, err := store.SubmitDocumentCommand(ctx, commandInput)
	if err != nil || repeated.ID != stored.ID {
		t.Fatalf("retry after close=%+v err=%v", repeated, err)
	}
	claimed, ok, err := store.ClaimNextDocumentCommand(ctx, job.ID, "close-manager", claim.Job.Generation)
	if err != nil || !ok || claimed.ID != stored.ID || claimed.Status != domain.DocumentCommandExecuting {
		t.Fatalf("claimed=%+v ok=%v err=%v", claimed, ok, err)
	}
	if _, err := store.CompleteDocumentCommand(ctx, job.ID, claimed.CommandID, "close-manager", claim.Job.Generation, domain.DocumentCommandSucceeded); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := store.ClaimNextDocumentCommand(ctx, job.ID, "close-manager", claim.Job.Generation); err != nil || ok {
		t.Fatalf("empty claim ok=%v err=%v", ok, err)
	}
	final, err := store.FinalizeDocumentJob(ctx, job.ID, "close-manager", claim.Job.Generation, domain.DocumentJobSucceeded, "", "")
	if err != nil || final.Status != domain.DocumentJobSucceeded || final.CommandsClosedAt == nil {
		t.Fatalf("final=%+v err=%v", final, err)
	}
	instance, err := store.GetDocumentInstance(ctx, claim.Instance.ID)
	if err != nil || instance.Status != domain.DocumentInstanceDestroying {
		t.Fatalf("instance=%+v err=%v", instance, err)
	}
	repeated, err = store.SubmitDocumentCommand(ctx, commandInput)
	if err != nil || repeated.ID != stored.ID {
		t.Fatalf("retry after terminal=%+v err=%v", repeated, err)
	}
}

func TestDocumentCloseAndSubmitSerializeWithoutPostCloseInsert(t *testing.T) {
	pool := requireDB(t)
	store := newDocumentTestStore(t, pool, 1)
	ctx := context.Background()
	workspaceID := seedDocumentWorkspace(t, pool, 63)
	setDocumentPoolSettings(t, pool, true, 1, 10, 1, 1)
	job, _ := startRunningDocumentJob(t, store, workspaceID, 63, "close-race", "close-race-instance", "race-manager")

	start := make(chan struct{})
	errs := make(chan error, 2)
	go func() {
		<-start
		_, err := store.SubmitDocumentCommand(ctx, documentCommand(job.ID, 63, "racing-command", `{}`))
		errs <- err
	}()
	go func() {
		<-start
		_, err := store.CloseDocumentJobCommands(ctx, job.ID, 63)
		errs <- err
	}()
	close(start)
	for i := 0; i < 2; i++ {
		err := <-errs
		if err != nil && !errors.Is(err, domain.ErrDocumentCommandsClosed) {
			t.Fatalf("race err=%v", err)
		}
	}
	var commandsClosedAt *time.Time
	var insertedAfterClose int
	if err := pool.QueryRow(ctx, `
SELECT j.commands_closed_at,
       count(c.id) FILTER (WHERE c.created_at > j.commands_closed_at)
FROM document_jobs j LEFT JOIN document_commands c ON c.job_id=j.id
WHERE j.id=$1 GROUP BY j.commands_closed_at
`, job.ID).Scan(&commandsClosedAt, &insertedAfterClose); err != nil {
		t.Fatal(err)
	}
	if commandsClosedAt == nil || insertedAfterClose != 0 {
		t.Fatalf("commands_closed_at=%v inserted_after_close=%d", commandsClosedAt, insertedAfterClose)
	}
}

func TestDocumentFinalizeRequiresCloseAndRejectsFailedCommands(t *testing.T) {
	pool := requireDB(t)
	store := newDocumentTestStore(t, pool, 2)
	ctx := context.Background()
	workspaceID := seedDocumentWorkspace(t, pool, 64)
	setDocumentPoolSettings(t, pool, true, 2, 10, 2, 2)

	unclosedJob, unclosedClaim := startRunningDocumentJob(t, store, workspaceID, 64, "unclosed", "unclosed-instance", "unclosed-manager")
	if _, err := store.FinalizeDocumentJob(ctx, unclosedJob.ID, "unclosed-manager", unclosedClaim.Job.Generation, domain.DocumentJobSucceeded, "", ""); !errors.Is(err, domain.ErrDocumentCommandsNotClosed) {
		t.Fatalf("unclosed success err=%v", err)
	}

	failedJob, failedClaim := startRunningDocumentJob(t, store, workspaceID, 64, "failed-command", "failed-command-instance", "failed-manager")
	if _, err := store.SubmitDocumentCommand(ctx, documentCommand(failedJob.ID, 64, "failed", `{}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CloseDocumentJobCommands(ctx, failedJob.ID, 64); err != nil {
		t.Fatal(err)
	}
	claimed, ok, err := store.ClaimNextDocumentCommand(ctx, failedJob.ID, "failed-manager", failedClaim.Job.Generation)
	if err != nil || !ok {
		t.Fatalf("claim=%+v ok=%v err=%v", claimed, ok, err)
	}
	if _, err := store.CompleteDocumentCommand(ctx, failedJob.ID, claimed.CommandID, "failed-manager", failedClaim.Job.Generation, domain.DocumentCommandFailed); err != nil {
		t.Fatal(err)
	}
	if _, err := store.FinalizeDocumentJob(ctx, failedJob.ID, "failed-manager", failedClaim.Job.Generation, domain.DocumentJobSucceeded, "", ""); !errors.Is(err, domain.ErrDocumentCommandsFailed) {
		t.Fatalf("failed command success err=%v", err)
	}
}

func TestDocumentClaimNextCommandFIFO(t *testing.T) {
	pool := requireDB(t)
	store := newDocumentTestStore(t, pool, 1)
	ctx := context.Background()
	workspaceID := seedDocumentWorkspace(t, pool, 65)
	setDocumentPoolSettings(t, pool, true, 1, 10, 1, 1)
	job, claim := startRunningDocumentJob(t, store, workspaceID, 65, "fifo", "fifo-instance", "fifo-manager")
	first, err := store.SubmitDocumentCommand(ctx, documentCommand(job.ID, 65, "first", `{}`))
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.SubmitDocumentCommand(ctx, documentCommand(job.ID, 65, "second", `{}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE document_commands SET created_at=CASE id WHEN $1 THEN now()-interval '2 seconds' ELSE now()-interval '1 second' END WHERE id IN ($1,$2)`, first.ID, second.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CloseDocumentJobCommands(ctx, job.ID, 65); err != nil {
		t.Fatal(err)
	}
	claimedFirst, ok, err := store.ClaimNextDocumentCommand(ctx, job.ID, "fifo-manager", claim.Job.Generation)
	if err != nil || !ok || claimedFirst.ID != first.ID {
		t.Fatalf("first claim=%+v ok=%v err=%v", claimedFirst, ok, err)
	}
	if _, err := store.CompleteDocumentCommand(ctx, job.ID, claimedFirst.CommandID, "fifo-manager", claim.Job.Generation, domain.DocumentCommandSucceeded); err != nil {
		t.Fatal(err)
	}
	claimedSecond, ok, err := store.ClaimNextDocumentCommand(ctx, job.ID, "fifo-manager", claim.Job.Generation)
	if err != nil || !ok || claimedSecond.ID != second.ID {
		t.Fatalf("second claim=%+v ok=%v err=%v", claimedSecond, ok, err)
	}
}

func TestDocumentQueuedCommandsCloseAndTTLConverge(t *testing.T) {
	pool := requireDB(t)
	store := newDocumentTestStore(t, pool, 1)
	ctx := context.Background()
	workspaceID := seedDocumentWorkspace(t, pool, 66)
	setDocumentPoolSettings(t, pool, true, 1, 10, 10, 1)
	job := submitDocumentJob(t, store, workspaceID, 66, "queued-command")
	input := documentCommand(job.ID, 66, "queued", `{}`)
	stored, err := store.SubmitDocumentCommand(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	closed, err := store.CloseDocumentJobCommands(ctx, job.ID, 66)
	if err != nil || closed.CommandsClosedAt == nil {
		t.Fatalf("closed=%+v err=%v", closed, err)
	}
	repeated, err := store.SubmitDocumentCommand(ctx, input)
	if err != nil || repeated.ID != stored.ID {
		t.Fatalf("queued retry after close=%+v err=%v", repeated, err)
	}
	if _, err := store.SubmitDocumentCommand(ctx, documentCommand(job.ID, 66, "after-close", `{}`)); !errors.Is(err, domain.ErrDocumentCommandsClosed) {
		t.Fatalf("new queued command after close err=%v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE document_jobs SET created_at=now()-interval '2 hours' WHERE id=$1`, job.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE document_pool_settings SET job_timeout_seconds=1,command_timeout_seconds=1 WHERE singleton`); err != nil {
		t.Fatal(err)
	}
	swept, err := store.SweepExpiredDocumentWork(ctx)
	if err != nil || swept.QueuedExpired != 1 {
		t.Fatalf("sweep=%+v err=%v", swept, err)
	}
	var commandStatus domain.DocumentCommandStatus
	if err := pool.QueryRow(ctx, `SELECT status FROM document_commands WHERE id=$1`, stored.ID).Scan(&commandStatus); err != nil {
		t.Fatal(err)
	}
	if commandStatus != domain.DocumentCommandExpired {
		t.Fatalf("queued command status=%s", commandStatus)
	}
}

func TestDocumentCloseUsesLockAcquisitionTimeAndRefreshesActivity(t *testing.T) {
	pool := requireDB(t)
	store := newDocumentTestStore(t, pool, 1)
	ctx := context.Background()
	workspaceID := seedDocumentWorkspace(t, pool, 67)
	setDocumentPoolSettings(t, pool, true, 1, 10, 1, 1)
	job, _ := startRunningDocumentJob(t, store, workspaceID, 67, "close-clock", "close-clock-instance", "clock-manager")
	if _, err := pool.Exec(ctx, `UPDATE document_jobs SET last_activity_at=now()-interval '1 hour' WHERE id=$1`, job.ID); err != nil {
		t.Fatal(err)
	}

	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `SELECT id FROM document_jobs WHERE id=$1 FOR UPDATE`, job.ID); err != nil {
		t.Fatal(err)
	}
	type closeResult struct {
		job domain.DocumentJob
		err error
	}
	result := make(chan closeResult, 1)
	go func() {
		closed, err := store.CloseDocumentJobCommands(ctx, job.ID, 67)
		result <- closeResult{job: closed, err: err}
	}()
	time.Sleep(200 * time.Millisecond)
	var releasedAt time.Time
	if err := tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&releasedAt); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	closed := <-result
	if closed.err != nil || closed.job.CommandsClosedAt == nil {
		t.Fatalf("closed=%+v err=%v", closed.job, closed.err)
	}
	if closed.job.CommandsClosedAt.Before(releasedAt.Add(-20 * time.Millisecond)) {
		t.Fatalf("commands_closed_at=%v predates lock release=%v", closed.job.CommandsClosedAt, releasedAt)
	}
	if closed.job.LastActivityAt.Before(releasedAt.Add(-20 * time.Millisecond)) {
		t.Fatalf("last_activity_at=%v was not refreshed at close", closed.job.LastActivityAt)
	}
}

func TestDocumentStartCommandCannotBypassFIFO(t *testing.T) {
	pool := requireDB(t)
	store := newDocumentTestStore(t, pool, 1)
	ctx := context.Background()
	workspaceID := seedDocumentWorkspace(t, pool, 68)
	setDocumentPoolSettings(t, pool, true, 1, 10, 1, 1)
	job, claim := startRunningDocumentJob(t, store, workspaceID, 68, "start-fifo", "start-fifo-instance", "start-fifo-manager")
	first, err := store.SubmitDocumentCommand(ctx, documentCommand(job.ID, 68, "first", `{}`))
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.SubmitDocumentCommand(ctx, documentCommand(job.ID, 68, "second", `{}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE document_commands SET created_at=CASE id WHEN $1 THEN now()-interval '2 seconds' ELSE now()-interval '1 second' END WHERE id IN ($1,$2)`, first.ID, second.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.StartDocumentCommand(ctx, job.ID, second.CommandID, "start-fifo-manager", claim.Job.Generation); !errors.Is(err, domain.ErrDocumentFenceLost) {
		t.Fatalf("started non-FIFO command err=%v", err)
	}
	started, err := store.StartDocumentCommand(ctx, job.ID, first.CommandID, "start-fifo-manager", claim.Job.Generation)
	if err != nil || started.ID != first.ID {
		t.Fatalf("started=%+v err=%v", started, err)
	}
}

func TestDocument0033RejectsHistoricalSucceededJobWithNonSucceededCommand(t *testing.T) {
	pool := requireDB(t)
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	workspaceID := seedDocumentWorkspace(t, tx, 69)
	if _, err := tx.Exec(ctx, `ALTER TABLE document_jobs DROP CONSTRAINT document_jobs_succeeded_commands_closed_check; ALTER TABLE document_jobs DROP COLUMN commands_closed_at; DROP TABLE migration_0033_document_job_finish_marker`); err != nil {
		t.Fatal(err)
	}
	jobID := uuid.NewString()
	if _, err := tx.Exec(ctx, `
INSERT INTO document_jobs(id,workspace_id,requester_user_id,idempotency_key,payload,payload_hash,status,terminal_at)
VALUES($1,$2,69,'historical-success','{}',repeat('a',64),'succeeded',now())
`, jobID, workspaceID); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `
INSERT INTO document_commands(id,job_id,command_id,payload,payload_hash,status)
VALUES($1,$2,'historical-pending','{}',repeat('b',64),'pending')
`, uuid.NewString(), jobID); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(migrationsDir(), "0033_document_job_finish.sql"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, string(raw)); err == nil || !strings.Contains(strings.ToLower(err.Error()), "non-succeeded") {
		t.Fatalf("migration err=%v", err)
	}
}

func TestDocumentFencingCommandIdempotencyAndRecovery(t *testing.T) {
	pool := requireDB(t)
	store := newDocumentTestStore(t, pool, 2)
	ctx := context.Background()
	workspaceID := seedDocumentWorkspace(t, pool, 41)
	setDocumentPoolSettings(t, pool, true, 2, 20, 2, 2)
	job := submitDocumentJob(t, store, workspaceID, 41, "job")
	createReadyDocumentInstance(t, store, "fenced-instance")
	claim, ok, err := store.ClaimNextDocumentJob(ctx, "manager", time.Minute)
	if err != nil || !ok || claim.Job.ID != job.ID || claim.Job.Generation != 1 {
		t.Fatalf("claim=%+v ok=%v err=%v", claim, ok, err)
	}
	if err := store.HeartbeatDocumentJob(ctx, job.ID, "wrong-owner", claim.Job.Generation, time.Minute); !errors.Is(err, domain.ErrDocumentFenceLost) {
		t.Fatalf("wrong-owner heartbeat err=%v", err)
	}
	if err := store.HeartbeatDocumentJob(ctx, job.ID, "manager", claim.Job.Generation+1, time.Minute); !errors.Is(err, domain.ErrDocumentFenceLost) {
		t.Fatalf("wrong-generation heartbeat err=%v", err)
	}
	if _, err := store.MarkDocumentJobAndInstanceRunning(ctx, job.ID, "manager", claim.Job.Generation); err != nil {
		t.Fatal(err)
	}

	command := documentCommand(job.ID, 41, "command-1", `{"source":"extract"}`)
	stored, err := store.SubmitDocumentCommand(ctx, command)
	if err != nil {
		t.Fatal(err)
	}
	repeated, err := store.SubmitDocumentCommand(ctx, command)
	if err != nil || repeated.ID != stored.ID {
		t.Fatalf("command dedupe=%+v err=%v", repeated, err)
	}
	command.Operation.Parameters = []byte(`{"source":"mutate"}`)
	if _, err := store.SubmitDocumentCommand(ctx, command); !errors.Is(err, domain.ErrDocumentIdempotencyConflict) {
		t.Fatalf("command mismatch err=%v", err)
	}
	if _, err := store.SubmitDocumentCommand(ctx, documentCommand(job.ID, 41, "pending-command", `{}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.StartDocumentCommand(ctx, job.ID, "command-1", "manager", claim.Job.Generation); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE document_commands SET started_at=now()-interval '10 seconds' WHERE id=$1`, stored.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE document_pool_settings SET command_timeout_seconds=1 WHERE singleton`); err != nil {
		t.Fatal(err)
	}
	swept, err := store.SweepExpiredDocumentWork(ctx)
	if err != nil || swept.CommandsExpired != 1 || swept.JobsFailed != 1 {
		t.Fatalf("sweep=%+v err=%v", swept, err)
	}
	recovered, _ := store.GetDocumentJob(ctx, job.ID)
	instance, _ := store.GetDocumentInstance(ctx, claim.Instance.ID)
	var pendingStatus domain.DocumentCommandStatus
	if err := pool.QueryRow(ctx, `SELECT status FROM document_commands WHERE job_id=$1 AND command_id='pending-command'`, job.ID).Scan(&pendingStatus); err != nil {
		t.Fatal(err)
	}
	if recovered.Status != domain.DocumentJobFailed || instance.Status != domain.DocumentInstanceDestroying || instance.AllocatedJobID != job.ID || pendingStatus != domain.DocumentCommandExpired {
		t.Fatalf("job=%+v instance=%+v pending_status=%s", recovered, instance, pendingStatus)
	}
	if err := store.HeartbeatDocumentJob(ctx, job.ID, "manager", claim.Job.Generation, time.Minute); !errors.Is(err, domain.ErrDocumentFenceLost) {
		t.Fatalf("old owner remained valid after recovery: %v", err)
	}
}

func TestDocumentQueuedTTLLeaseRecoveryAndTerminalDestroyIntent(t *testing.T) {
	pool := requireDB(t)
	store := newDocumentTestStore(t, pool, 3)
	ctx := context.Background()
	claimedWorkspace := seedDocumentWorkspace(t, pool, 51)
	queuedWorkspace := seedDocumentWorkspace(t, pool, 52)
	setDocumentPoolSettings(t, pool, true, 3, 20, 3, 3)
	claimed := submitDocumentJob(t, store, claimedWorkspace, 51, "lease-expire")
	queued := submitDocumentJob(t, store, queuedWorkspace, 52, "queued-expire")
	createReadyDocumentInstance(t, store, "lease-instance")
	claim, ok, err := store.ClaimNextDocumentJob(ctx, "old-manager", time.Minute)
	if err != nil || !ok || claim.Job.ID != claimed.ID {
		t.Fatalf("claim=%+v ok=%v err=%v", claim, ok, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE document_jobs SET created_at=now()-interval '2 hours' WHERE id=$1`, queued.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE document_jobs SET claim_lease_until=now()-interval '1 second' WHERE id=$1`, claimed.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE document_pool_settings SET job_timeout_seconds=1,command_timeout_seconds=1 WHERE singleton`); err != nil {
		t.Fatal(err)
	}
	swept, err := store.SweepExpiredDocumentWork(ctx)
	if err != nil || swept.QueuedExpired != 1 || swept.JobsFailed != 1 {
		t.Fatalf("sweep=%+v err=%v", swept, err)
	}
	queuedAfter, _ := store.GetDocumentJob(ctx, queued.ID)
	claimedAfter, _ := store.GetDocumentJob(ctx, claimed.ID)
	instance, _ := store.GetDocumentInstance(ctx, claim.Instance.ID)
	if queuedAfter.Status != domain.DocumentJobExpired || claimedAfter.Status != domain.DocumentJobFailed || instance.Status != domain.DocumentInstanceDestroying {
		t.Fatalf("queued=%s claimed=%s instance=%s", queuedAfter.Status, claimedAfter.Status, instance.Status)
	}

	// A normal terminal transition persists destroy intent in the same transaction.
	job := submitDocumentJob(t, store, claimedWorkspace, 51, "normal-terminal")
	createReadyDocumentInstance(t, store, "normal-terminal-instance")
	terminalClaim, ok, err := store.ClaimNextDocumentJob(ctx, "manager-2", time.Minute)
	if err != nil || !ok || terminalClaim.Job.ID != job.ID {
		t.Fatalf("terminal claim=%+v ok=%v err=%v", terminalClaim, ok, err)
	}
	if _, err := store.CloseDocumentJobCommands(ctx, job.ID, 51); err != nil {
		t.Fatal(err)
	}
	final, err := store.FinalizeDocumentJob(ctx, job.ID, "manager-2", terminalClaim.Job.Generation, domain.DocumentJobSucceeded, "", "")
	if err != nil || final.Status != domain.DocumentJobSucceeded {
		t.Fatalf("final=%+v err=%v", final, err)
	}
	terminalInstance, _ := store.GetDocumentInstance(ctx, terminalClaim.Instance.ID)
	if terminalInstance.Status != domain.DocumentInstanceDestroying {
		t.Fatalf("terminal instance status=%s", terminalInstance.Status)
	}
}

func TestDocumentReserveInstanceCapacityIsAtomic(t *testing.T) {
	pool := requireDB(t)
	store := newDocumentTestStore(t, pool, 3)
	ctx := context.Background()
	setDocumentPoolSettings(t, pool, true, 3, 10, 10, 3)
	if _, err := pool.Exec(ctx, `UPDATE document_pool_settings SET min_ready=2 WHERE singleton`); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	reserved := make(chan domain.DocumentInstance, 8)
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			name := fmt.Sprintf("reserve-concurrent-%d", i)
			instance, ok, err := store.ReserveDocumentInstance(ctx, name, "/manager/slots/"+name)
			if err != nil {
				errs <- err
			} else if ok {
				reserved <- instance
			}
		}(i)
	}
	wg.Wait()
	if len(errs) != 0 || len(reserved) != 2 {
		t.Fatalf("reserve errors=%d successes=%d, want 0/2", len(errs), len(reserved))
	}
	var creating, live int
	if err := pool.QueryRow(ctx, `SELECT count(*) FILTER (WHERE status='creating'),count(*) FILTER (WHERE status IN ('creating','ready','allocated','running','destroying')) FROM document_instances`).Scan(&creating, &live); err != nil {
		t.Fatal(err)
	}
	if creating != 2 || live != 2 {
		t.Fatalf("creating=%d live=%d", creating, live)
	}

	if recovered, err := store.RecoverCreatingDocumentInstances(ctx); err != nil || recovered != 2 {
		t.Fatalf("recovered=%d err=%v", recovered, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE document_pool_settings SET min_ready=3 WHERE singleton`); err != nil {
		t.Fatal(err)
	}
	instance, ok, err := store.ReserveDocumentInstance(ctx, "hard-cap-last", "/manager/slots/hard-cap-last")
	if err != nil || !ok || instance.Status != domain.DocumentInstanceCreating {
		t.Fatalf("last reserve=%+v ok=%v err=%v", instance, ok, err)
	}
	if _, ok, err := store.ReserveDocumentInstance(ctx, "hard-cap-over", "/manager/slots/hard-cap-over"); err != nil || ok {
		t.Fatalf("over hard cap ok=%v err=%v", ok, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE document_pool_settings SET max_active=4,min_ready=4,per_tenant_active_limit=4 WHERE singleton`); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := store.ReserveDocumentInstance(ctx, "deployment-over", "/manager/slots/deployment-over"); err == nil || ok {
		t.Fatalf("deployment max violation ok=%v err=%v", ok, err)
	}
}

func TestDocumentInstancePrewarmTransitionsAndCleanupIntent(t *testing.T) {
	pool := requireDB(t)
	store := newDocumentTestStore(t, pool, 4)
	ctx := context.Background()
	setDocumentPoolSettings(t, pool, true, 4, 10, 10, 4)
	if _, err := pool.Exec(ctx, `UPDATE document_pool_settings SET min_ready=2,ready_idle_ttl_seconds=60 WHERE singleton`); err != nil {
		t.Fatal(err)
	}

	first, ok, err := store.ReserveDocumentInstance(ctx, "prewarm-first", "/manager/slots/prewarm-first")
	if err != nil || !ok {
		t.Fatalf("first reserve=%+v ok=%v err=%v", first, ok, err)
	}
	second, ok, err := store.ReserveDocumentInstance(ctx, "prewarm-second", "/manager/slots/prewarm-second")
	if err != nil || !ok {
		t.Fatalf("second reserve=%+v ok=%v err=%v", second, ok, err)
	}
	for _, instance := range []domain.DocumentInstance{first, second} {
		ready, err := store.MarkDocumentInstanceReady(ctx, instance.ID)
		if err != nil || ready.Status != domain.DocumentInstanceReady || ready.ReadyAt == nil {
			t.Fatalf("ready=%+v err=%v", ready, err)
		}
	}
	if reserved, err := store.ReserveExcessReadyForDestroy(ctx); err != nil || reserved != 0 {
		t.Fatalf("cleanup at target=%d err=%v", reserved, err)
	}

	if _, err := pool.Exec(ctx, `UPDATE document_pool_settings SET min_ready=1 WHERE singleton`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE document_instances SET ready_at=now()-interval '2 minutes' WHERE id=$1`, first.ID); err != nil {
		t.Fatal(err)
	}
	if reserved, err := store.ReserveExcessReadyForDestroy(ctx); err != nil || reserved != 1 {
		t.Fatalf("ttl cleanup=%d err=%v", reserved, err)
	}
	firstAfter, err := store.GetDocumentInstance(ctx, first.ID)
	if err != nil || firstAfter.Status != domain.DocumentInstanceDestroying {
		t.Fatalf("first after=%+v err=%v", firstAfter, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE document_pool_settings SET enabled=false,max_active=0,min_ready=0,per_tenant_active_limit=0 WHERE singleton`); err != nil {
		t.Fatal(err)
	}
	if reserved, err := store.ReserveExcessReadyForDestroy(ctx); err != nil || reserved != 1 {
		t.Fatalf("disabled drain=%d err=%v", reserved, err)
	}
	destroying, err := store.ListDestroyingDocumentInstances(ctx)
	if err != nil || len(destroying) != 2 {
		t.Fatalf("destroying=%+v err=%v", destroying, err)
	}
	destroyed, err := store.MarkDocumentInstanceDestroyed(ctx, first.ID)
	if err != nil || destroyed.Status != domain.DocumentInstanceDestroyed {
		t.Fatalf("destroyed=%+v err=%v", destroyed, err)
	}
	secondAfter, err := store.GetDocumentInstance(ctx, second.ID)
	if err != nil || secondAfter.Status != domain.DocumentInstanceDestroying {
		t.Fatalf("failed destroy must remain retryable: instance=%+v err=%v", secondAfter, err)
	}
}

func TestDocumentInstanceFailedPrewarmCanBeReservedForCleanup(t *testing.T) {
	pool := requireDB(t)
	store := newDocumentTestStore(t, pool, 2)
	ctx := context.Background()
	setDocumentPoolSettings(t, pool, true, 2, 10, 10, 2)
	if _, err := pool.Exec(ctx, `UPDATE document_pool_settings SET min_ready=2 WHERE singleton`); err != nil {
		t.Fatal(err)
	}

	creating, ok, err := store.ReserveDocumentInstance(ctx, "failed-creating", "/manager/slots/failed-creating")
	if err != nil || !ok {
		t.Fatalf("creating reserve=%+v ok=%v err=%v", creating, ok, err)
	}
	ready, ok, err := store.ReserveDocumentInstance(ctx, "failed-ready-response", "/manager/slots/failed-ready-response")
	if err != nil || !ok {
		t.Fatalf("ready reserve=%+v ok=%v err=%v", ready, ok, err)
	}
	if _, err := store.MarkDocumentInstanceReady(ctx, ready.ID); err != nil {
		t.Fatal(err)
	}
	for _, instance := range []domain.DocumentInstance{creating, ready} {
		destroying, err := store.MarkDocumentInstanceDestroying(ctx, instance.ID)
		if err != nil || destroying.Status != domain.DocumentInstanceDestroying || destroying.DestroyAt == nil {
			t.Fatalf("destroying=%+v err=%v", destroying, err)
		}
	}
	repeated, err := store.MarkDocumentInstanceDestroying(ctx, ready.ID)
	if err != nil || repeated.Status != domain.DocumentInstanceDestroying {
		t.Fatalf("repeat cleanup=%+v err=%v", repeated, err)
	}
}

func TestDocumentJobAndInstanceRunningAreAtomic(t *testing.T) {
	pool := requireDB(t)
	store := newDocumentTestStore(t, pool, 2)
	ctx := context.Background()
	workspaceID := seedDocumentWorkspace(t, pool, 71)
	setDocumentPoolSettings(t, pool, true, 2, 10, 2, 2)
	createReadyDocumentInstance(t, store, "atomic-running")
	job := submitDocumentJob(t, store, workspaceID, 71, "atomic-running")
	claim, ok, err := store.ClaimNextDocumentJob(ctx, "atomic-manager", time.Minute)
	if err != nil || !ok || claim.Job.ID != job.ID {
		t.Fatalf("claim=%+v ok=%v err=%v", claim, ok, err)
	}
	running, err := store.MarkDocumentJobAndInstanceRunning(ctx, job.ID, "atomic-manager", claim.Job.Generation)
	if err != nil || running.Status != domain.DocumentJobRunning {
		t.Fatalf("running=%+v err=%v", running, err)
	}
	instance, err := store.GetDocumentInstance(ctx, claim.Instance.ID)
	if err != nil || instance.Status != domain.DocumentInstanceRunning {
		t.Fatalf("instance=%+v err=%v", instance, err)
	}
}

func seedDocumentWorkspace(t *testing.T, executor interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
}, userID int64) string {
	t.Helper()
	workspaceID := uuid.NewString()
	ctx := context.Background()
	if _, err := executor.Exec(ctx, `INSERT INTO users(id,username,status) VALUES($1,$2,'approved')`, userID, fmt.Sprintf("doc-user-%d", userID)); err != nil {
		t.Fatal(err)
	}
	if _, err := executor.Exec(ctx, `INSERT INTO workspaces(id,session_key,owner_user_id,kind,volume_id) VALUES($1,$2,$3,'personal',$4)`, workspaceID, fmt.Sprintf("personal:%d", userID), userID, "doc-vol-"+workspaceID); err != nil {
		t.Fatal(err)
	}
	return workspaceID
}

func assertDocumentConstraint(t *testing.T, pool interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
}, sql string, args ...any) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), sql, args...); err == nil {
		t.Fatal("expected database constraint violation")
	}
}

func newDocumentTestStore(t *testing.T, pool *pgxpool.Pool, maxActive int) *Store {
	t.Helper()
	store, err := NewStore(pool, WithDocumentPoolDeploymentMaxActive(maxActive))
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func setDocumentPoolSettings(t *testing.T, pool *pgxpool.Pool, enabled bool, maxActive, globalQueue, workspaceQueue, workspaceActive int) {
	t.Helper()
	_, err := pool.Exec(context.Background(), `UPDATE document_pool_settings SET enabled=$1,max_active=$2,min_ready=0,global_queue_limit=$3,per_tenant_queue_limit=$4,per_tenant_active_limit=$5 WHERE singleton`, enabled, maxActive, globalQueue, workspaceQueue, workspaceActive)
	if err != nil {
		t.Fatal(err)
	}
}

func submitDocumentJob(t *testing.T, store *Store, workspaceID string, requester int64, key string) domain.DocumentJob {
	t.Helper()
	job, err := store.SubmitDocumentJob(context.Background(), domain.SubmitDocumentJobCommand{WorkspaceID: workspaceID, RequesterUserID: requester, IdempotencyKey: key, Payload: []byte(`{"key":"` + key + `"}`)})
	if err != nil {
		t.Fatal(err)
	}
	return job
}

func startRunningDocumentJob(t *testing.T, store *Store, workspaceID string, requester int64, key, instanceName, owner string) (domain.DocumentJob, domain.DocumentClaim) {
	t.Helper()
	job := submitDocumentJob(t, store, workspaceID, requester, key)
	createReadyDocumentInstance(t, store, instanceName)
	claim, ok, err := store.ClaimNextDocumentJob(context.Background(), owner, time.Minute)
	if err != nil || !ok || claim.Job.ID != job.ID {
		t.Fatalf("claim=%+v ok=%v err=%v", claim, ok, err)
	}
	if _, err := store.MarkDocumentJobAndInstanceRunning(context.Background(), job.ID, owner, claim.Job.Generation); err != nil {
		t.Fatal(err)
	}
	return job, claim
}

func documentCommand(jobID string, requester int64, commandID, parameters string) domain.SubmitDocumentCommand {
	return domain.SubmitDocumentCommand{
		JobID:           jobID,
		CommandID:       commandID,
		RequesterUserID: requester,
		Operation: domain.DocumentOperationRequest{
			SchemaVersion: 1,
			Operation:     "test_operation",
			Parameters:    []byte(parameters),
		},
	}
}

func createReadyDocumentInstance(t *testing.T, store *Store, name string) domain.DocumentInstance {
	t.Helper()
	instance, err := store.CreateReadyDocumentInstance(context.Background(), name, "/manager/slots/"+name)
	if err != nil {
		t.Fatal(err)
	}
	return instance
}
