package postgres

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/domain"
)

func TestDocumentToolStore(t *testing.T) {
	pool := requireDB(t)
	store := newDocumentTestStore(t, pool, 2)
	tests := []struct {
		name string
		run  func(*testing.T, *pgxpool.Pool, *Store)
	}{
		{"atomic reuse and requester", testDocumentToolStoreAtomicallyReusesTaskJobAndDerivesRequester},
		{"idempotency and task scope", testDocumentToolStoreIsIdempotentAndRejectsTaskScopeOrStateDrift},
		{"close and status", testDocumentToolStoreCloseAndStatusRequireSameRunningTask},
		{"authorized artifact", testDocumentToolStoreArtifactRequiresSameRunningTaskAndRequest},
		{"membership removal race", testDocumentToolStoreSerializesTeamMembershipRemovalAgainstSubmit},
		{"disabled pool", testDocumentToolStoreDisabledPoolRejectsNewJobButAllowsExistingJobCommands},
		{"task terminal race", testDocumentToolStoreSerializesTaskTerminalTransitionAgainstSubmit},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) { test.run(t, pool, store) })
	}
}

func testDocumentToolStoreAtomicallyReusesTaskJobAndDerivesRequester(t *testing.T, pool *pgxpool.Pool, store *Store) {
	ctx := context.Background()
	workspaceID := seedDocumentWorkspace(t, pool, 81)
	setDocumentPoolSettings(t, pool, true, 2, 10, 10, 1)
	task := seedRunningDocumentToolTask(t, pool, workspaceID, "personal:81", 81, "docs.v1")
	scope := domain.DocumentToolTaskScope{TaskID: task.ID, SessionKey: task.SessionKey, WorkspaceID: task.WorkspaceID}

	first, err := store.SubmitDocumentToolCommand(ctx, domain.SubmitDocumentToolCommand{
		Scope: scope, RequestID: "tool-call-1",
		Operation: domain.DocumentOperationRequest{SchemaVersion: 1, Operation: "export_docx", Parameters: []byte(`{"output_name":"report.docx"}`)},
	})
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.SubmitDocumentToolCommand(ctx, domain.SubmitDocumentToolCommand{
		Scope: scope, RequestID: "tool-call-2",
		Operation: domain.DocumentOperationRequest{SchemaVersion: 1, Operation: "export_docx", Parameters: []byte(`{"output_name":"appendix.docx"}`)},
	})
	if err != nil {
		t.Fatal(err)
	}
	if first.Job.ID != second.Job.ID || first.Job.RequesterUserID != 81 || first.Job.WorkspaceID != workspaceID {
		t.Fatalf("first=%+v second=%+v", first, second)
	}
	if first.Command.CommandID == second.Command.CommandID || first.Command.CommandID == "" {
		t.Fatalf("command IDs first=%q second=%q", first.Command.CommandID, second.Command.CommandID)
	}
	var jobs, commands int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM document_jobs WHERE idempotency_key=$1`, documentToolJobKey(task.ID)).Scan(&jobs); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM document_commands WHERE job_id=$1`, first.Job.ID).Scan(&commands); err != nil {
		t.Fatal(err)
	}
	if jobs != 1 || commands != 2 {
		t.Fatalf("jobs=%d commands=%d", jobs, commands)
	}
}

func testDocumentToolStoreIsIdempotentAndRejectsTaskScopeOrStateDrift(t *testing.T, pool *pgxpool.Pool, store *Store) {
	ctx := context.Background()
	workspaceID := seedDocumentWorkspace(t, pool, 82)
	setDocumentPoolSettings(t, pool, true, 2, 10, 10, 1)
	task := seedRunningDocumentToolTask(t, pool, workspaceID, "personal:82", 82, "docs.v1")
	scope := domain.DocumentToolTaskScope{TaskID: task.ID, SessionKey: task.SessionKey, WorkspaceID: task.WorkspaceID}
	command := domain.SubmitDocumentToolCommand{
		Scope: scope, RequestID: "stable-call",
		Operation: domain.DocumentOperationRequest{SchemaVersion: 1, Operation: "convert", Parameters: []byte(`{"format":"docx"}`)},
	}

	first, err := store.SubmitDocumentToolCommand(ctx, command)
	if err != nil {
		t.Fatal(err)
	}
	repeated, err := store.SubmitDocumentToolCommand(ctx, command)
	if err != nil || repeated.Job.ID != first.Job.ID || repeated.Command.ID != first.Command.ID {
		t.Fatalf("repeated=%+v err=%v", repeated, err)
	}
	changed := command
	changed.Operation.Parameters = []byte(`{"format":"pdf"}`)
	if _, err := store.SubmitDocumentToolCommand(ctx, changed); !errors.Is(err, domain.ErrDocumentIdempotencyConflict) {
		t.Fatalf("changed payload err=%v", err)
	}
	mismatch := command
	mismatch.RequestID = "wrong-scope"
	mismatch.Scope.SessionKey = "personal:other"
	if _, err := store.SubmitDocumentToolCommand(ctx, mismatch); !errors.Is(err, domain.ErrDocumentUnauthorized) {
		t.Fatalf("scope mismatch err=%v", err)
	}

	if _, err := pool.Exec(ctx, `UPDATE tasks SET cancel_requested_at=timezone('utc',now()) WHERE id=$1`, task.ID); err != nil {
		t.Fatal(err)
	}
	cancelled := command
	cancelled.RequestID = "after-cancel"
	if _, err := store.SubmitDocumentToolCommand(ctx, cancelled); !errors.Is(err, domain.ErrDocumentTaskInactive) {
		t.Fatalf("cancelled task err=%v", err)
	}
}

func testDocumentToolStoreCloseAndStatusRequireSameRunningTask(t *testing.T, pool *pgxpool.Pool, store *Store) {
	ctx := context.Background()
	workspaceID := seedDocumentWorkspace(t, pool, 83)
	setDocumentPoolSettings(t, pool, true, 2, 10, 10, 1)
	task := seedRunningDocumentToolTask(t, pool, workspaceID, "personal:83", 83, "docs.v1")
	scope := domain.DocumentToolTaskScope{TaskID: task.ID, SessionKey: task.SessionKey, WorkspaceID: task.WorkspaceID}
	created, err := store.SubmitDocumentToolCommand(ctx, domain.SubmitDocumentToolCommand{
		Scope: scope, RequestID: "call-1",
		Operation: domain.DocumentOperationRequest{SchemaVersion: 1, Operation: "export_docx", Parameters: []byte(`{}`)},
	})
	if err != nil {
		t.Fatal(err)
	}
	closed, err := store.CloseDocumentToolJob(ctx, scope)
	if err != nil {
		t.Fatal(err)
	}
	status, err := store.GetDocumentToolStatus(ctx, scope, "call-1")
	if err != nil {
		t.Fatal(err)
	}
	if closed.ID != created.Job.ID || closed.CommandsClosedAt == nil || status.Job.ID != closed.ID || status.Command == nil || status.Command.ID != created.Command.ID {
		t.Fatalf("created=%+v closed=%+v status=%+v", created, closed, status)
	}

	workspaceID = seedDocumentWorkspace(t, pool, 831)
	setDocumentPoolSettings(t, pool, true, 2, 10, 10, 1)
	cancelledTask := seedRunningDocumentToolTask(t, pool, workspaceID, "personal:831", 831, "docs.v1")
	cancelledScope := domain.DocumentToolTaskScope{TaskID: cancelledTask.ID, SessionKey: cancelledTask.SessionKey, WorkspaceID: cancelledTask.WorkspaceID}
	if _, err := store.SubmitDocumentToolCommand(ctx, domain.SubmitDocumentToolCommand{
		Scope: cancelledScope, RequestID: "cancelled-call",
		Operation: domain.DocumentOperationRequest{SchemaVersion: 1, Operation: "export_docx", Parameters: []byte(`{}`)},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE tasks SET cancel_requested_at=timezone('utc',now()) WHERE id=$1`, cancelledTask.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CloseDocumentToolJob(ctx, cancelledScope); err != nil {
		t.Fatalf("cancelled task close err=%v", err)
	}
	if _, err := store.GetDocumentToolStatus(ctx, cancelledScope, "cancelled-call"); !errors.Is(err, domain.ErrDocumentTaskInactive) {
		t.Fatalf("cancelled task status err=%v", err)
	}

	terminalWorkspaceID := seedDocumentWorkspace(t, pool, 832)
	setDocumentPoolSettings(t, pool, true, 2, 10, 10, 1)
	terminalTask := seedRunningDocumentToolTask(t, pool, terminalWorkspaceID, "personal:832", 832, "docs.v1")
	terminalScope := domain.DocumentToolTaskScope{TaskID: terminalTask.ID, SessionKey: terminalTask.SessionKey, WorkspaceID: terminalTask.WorkspaceID}
	terminalSubmission, err := store.SubmitDocumentToolCommand(ctx, domain.SubmitDocumentToolCommand{
		Scope: terminalScope, RequestID: "terminal-call",
		Operation: domain.DocumentOperationRequest{SchemaVersion: 1, Operation: "export_docx", Parameters: []byte(`{}`)},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
UPDATE document_jobs SET status='failed',terminal_at=timezone('utc',now()) WHERE id=$1
`, terminalSubmission.Job.ID); err != nil {
		t.Fatal(err)
	}
	terminalClosed, err := store.CloseDocumentToolJob(ctx, terminalScope)
	if err != nil || terminalClosed.CommandsClosedAt == nil {
		t.Fatalf("terminal job close=%+v err=%v", terminalClosed, err)
	}

	otherWorkspaceID := seedDocumentWorkspace(t, pool, 85)
	otherTask := seedRunningDocumentToolTask(t, pool, otherWorkspaceID, "personal:85", 85, "docs.v1")
	otherScope := domain.DocumentToolTaskScope{TaskID: otherTask.ID, SessionKey: otherTask.SessionKey, WorkspaceID: otherTask.WorkspaceID}
	if _, err := store.GetDocumentToolStatus(ctx, otherScope, ""); !errors.Is(err, domain.ErrDocumentJobNotFound) {
		t.Fatalf("other task status err=%v", err)
	}
}

func testDocumentToolStoreArtifactRequiresSameRunningTaskAndRequest(t *testing.T, pool *pgxpool.Pool, store *Store) {
	ctx := context.Background()
	workspaceID := seedDocumentWorkspace(t, pool, 94)
	setDocumentPoolSettings(t, pool, true, 2, 10, 10, 1)
	task := seedRunningDocumentToolTask(t, pool, workspaceID, "personal:94", 94, "docs.v1")
	scope := domain.DocumentToolTaskScope{TaskID: task.ID, SessionKey: task.SessionKey, WorkspaceID: task.WorkspaceID}
	submission, err := store.SubmitDocumentToolCommand(ctx, domain.SubmitDocumentToolCommand{
		Scope: scope, RequestID: "artifact-call",
		Operation: domain.DocumentOperationRequest{SchemaVersion: 1, Operation: "export_docx", Parameters: []byte(`{"content":"report"}`)},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
UPDATE document_commands SET status='succeeded',generation=1,started_at=timezone('utc',now()),completed_at=timezone('utc',now())
WHERE id=$1
`, submission.Command.ID); err != nil {
		t.Fatal(err)
	}
	content := []byte("durable-docx")
	digest := sha256.Sum256(content)
	if _, err := pool.Exec(ctx, `
INSERT INTO document_artifacts(id,job_id,command_id,file_name,media_type,content,size_bytes,sha256)
VALUES($1,$2,$3,'report.docx',$4,$5,$6,$7)
`, uuid.NewString(), submission.Job.ID, submission.Command.CommandID, "application/vnd.openxmlformats-officedocument.wordprocessingml.document", content, len(content), hex.EncodeToString(digest[:])); err != nil {
		t.Fatal(err)
	}
	artifact, err := store.GetDocumentToolArtifact(ctx, scope, "artifact-call")
	if err != nil || string(artifact.Content) != string(content) || artifact.FileName != "report.docx" {
		t.Fatalf("artifact=%+v err=%v", artifact, err)
	}
	if _, err := store.GetDocumentToolArtifact(ctx, scope, "other-call"); !errors.Is(err, domain.ErrDocumentArtifactNotFound) {
		t.Fatalf("other request err=%v", err)
	}
	if _, err := pool.Exec(ctx, `
UPDATE tasks SET status='failed',claim_owner=NULL,claim_lease_until=NULL,claimed_at=NULL,terminal_at=timezone('utc',now())
WHERE id=$1
`, task.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.GetDocumentToolArtifact(ctx, scope, "artifact-call"); !errors.Is(err, domain.ErrDocumentTaskInactive) {
		t.Fatalf("terminal task artifact err=%v", err)
	}
}

func testDocumentToolStoreSerializesTeamMembershipRemovalAgainstSubmit(t *testing.T, pool *pgxpool.Pool, store *Store) {
	ctx := context.Background()
	ownerID, memberID := int64(88), int64(89)
	ownerWorkspaceID := seedDocumentWorkspace(t, pool, ownerID)
	_ = ownerWorkspaceID
	if _, err := pool.Exec(ctx, `INSERT INTO users(id,username,status) VALUES($1,$2,'approved')`, memberID, "doc-user-89"); err != nil {
		t.Fatal(err)
	}
	teamID := uuid.NewString()
	workspaceID := uuid.NewString()
	if _, err := pool.Exec(ctx, `INSERT INTO teams(id,name,owner_user_id) VALUES($1,'docs-team',$2)`, teamID, ownerID); err != nil {
		t.Fatal(err)
	}
	var membershipID int64
	if err := pool.QueryRow(ctx, `
INSERT INTO team_members(team_id,user_id,role,status) VALUES($1,$2,'member','approved') RETURNING id
`, teamID, memberID).Scan(&membershipID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
INSERT INTO workspaces(id,session_key,owner_user_id,kind,team_id,volume_id)
VALUES($1,$2,$3,'team',$4,$5)
`, workspaceID, "team:"+teamID, ownerID, teamID, "doc-team-vol-"+workspaceID); err != nil {
		t.Fatal(err)
	}
	setDocumentPoolSettings(t, pool, true, 2, 10, 10, 1)
	task := seedRunningDocumentToolTask(t, pool, workspaceID, "team:"+teamID, memberID, "docs.v1")
	scope := domain.DocumentToolTaskScope{TaskID: task.ID, SessionKey: task.SessionKey, WorkspaceID: task.WorkspaceID}

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `SELECT id FROM team_members WHERE id=$1 FOR UPDATE`, membershipID); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, submitErr := store.SubmitDocumentToolCommand(ctx, domain.SubmitDocumentToolCommand{
			Scope: scope, RequestID: "membership-race",
			Operation: domain.DocumentOperationRequest{SchemaVersion: 1, Operation: "export_docx", Parameters: []byte(`{}`)},
		})
		done <- submitErr
	}()
	select {
	case err := <-done:
		t.Fatalf("submit did not block on membership lock: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	if _, err := tx.Exec(ctx, `UPDATE team_members SET status='removed',removed_at=timezone('utc',now()) WHERE id=$1`, membershipID); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-done; !errors.Is(err, domain.ErrDocumentUnauthorized) {
		t.Fatalf("membership racing submit err=%v", err)
	}
	var jobs int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM document_jobs WHERE workspace_id=$1`, workspaceID).Scan(&jobs); err != nil || jobs != 0 {
		t.Fatalf("jobs=%d err=%v", jobs, err)
	}
}

func testDocumentToolStoreDisabledPoolRejectsNewJobButAllowsExistingJobCommands(t *testing.T, pool *pgxpool.Pool, store *Store) {
	ctx := context.Background()
	workspaceID := seedDocumentWorkspace(t, pool, 86)
	setDocumentPoolSettings(t, pool, true, 2, 10, 10, 1)
	task := seedRunningDocumentToolTask(t, pool, workspaceID, "personal:86", 86, "docs.v1")
	scope := domain.DocumentToolTaskScope{TaskID: task.ID, SessionKey: task.SessionKey, WorkspaceID: task.WorkspaceID}
	first, err := store.SubmitDocumentToolCommand(ctx, domain.SubmitDocumentToolCommand{
		Scope: scope, RequestID: "first",
		Operation: domain.DocumentOperationRequest{SchemaVersion: 1, Operation: "export_docx", Parameters: []byte(`{}`)},
	})
	if err != nil {
		t.Fatal(err)
	}
	setDocumentPoolSettings(t, pool, false, 0, 10, 10, 0)
	second, err := store.SubmitDocumentToolCommand(ctx, domain.SubmitDocumentToolCommand{
		Scope: scope, RequestID: "second",
		Operation: domain.DocumentOperationRequest{SchemaVersion: 1, Operation: "export_docx", Parameters: []byte(`{}`)},
	})
	if err != nil || second.Job.ID != first.Job.ID {
		t.Fatalf("existing job second=%+v err=%v", second, err)
	}

	otherWorkspaceID := seedDocumentWorkspace(t, pool, 87)
	otherTask := seedRunningDocumentToolTask(t, pool, otherWorkspaceID, "personal:87", 87, "docs.v1")
	otherScope := domain.DocumentToolTaskScope{TaskID: otherTask.ID, SessionKey: otherTask.SessionKey, WorkspaceID: otherTask.WorkspaceID}
	if _, err := store.SubmitDocumentToolCommand(ctx, domain.SubmitDocumentToolCommand{
		Scope: otherScope, RequestID: "first",
		Operation: domain.DocumentOperationRequest{SchemaVersion: 1, Operation: "export_docx", Parameters: []byte(`{}`)},
	}); !errors.Is(err, domain.ErrDocumentPoolDisabled) {
		t.Fatalf("disabled new job err=%v", err)
	}
}

func testDocumentToolStoreSerializesTaskTerminalTransitionAgainstSubmit(t *testing.T, pool *pgxpool.Pool, store *Store) {
	ctx := context.Background()
	workspaceID := seedDocumentWorkspace(t, pool, 84)
	setDocumentPoolSettings(t, pool, true, 2, 10, 10, 1)
	task := seedRunningDocumentToolTask(t, pool, workspaceID, "personal:84", 84, "docs.v1")
	scope := domain.DocumentToolTaskScope{TaskID: task.ID, SessionKey: task.SessionKey, WorkspaceID: task.WorkspaceID}

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `SELECT id FROM tasks WHERE id=$1 FOR UPDATE`, task.ID); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, submitErr := store.SubmitDocumentToolCommand(ctx, domain.SubmitDocumentToolCommand{
			Scope: scope, RequestID: "racing-call",
			Operation: domain.DocumentOperationRequest{SchemaVersion: 1, Operation: "export_docx", Parameters: []byte(`{}`)},
		})
		done <- submitErr
	}()
	select {
	case err := <-done:
		t.Fatalf("submit did not block on task lock: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	updateCtx, cancelUpdate := context.WithTimeout(ctx, 2*time.Second)
	defer cancelUpdate()
	if _, err := tx.Exec(updateCtx, `UPDATE workspaces SET reset_at=reset_at WHERE id=$1`, workspaceID); err != nil {
		t.Fatalf("workspace update deadlocked behind document submit: %v", err)
	}
	if _, err := tx.Exec(ctx, `UPDATE tasks SET status='failed',claim_owner=NULL,claim_lease_until=NULL,claimed_at=NULL,terminal_at=timezone('utc',now()) WHERE id=$1`, task.ID); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-done; !errors.Is(err, domain.ErrDocumentTaskInactive) {
		t.Fatalf("racing submit err=%v", err)
	}
}

func seedRunningDocumentToolTask(t *testing.T, pool *pgxpool.Pool, workspaceID, sessionKey string, requester int64, toolPolicy string) domain.Task {
	t.Helper()
	taskID := uuid.NewString()
	ctx := context.Background()
	row := pool.QueryRow(ctx, `
INSERT INTO tasks(
 id,workspace_id,session_key,session_sequence,requester_user_id,
 source,source_instance_id,message_id,message_idempotency_key,prompt,persona_snapshot,
 tool_policy_version,prompt_bytes,persona_bytes,status,claim_owner,claim_lease_until,
 claimed_at,worker_instance_id,worker_dispatch_started_at,started_at
) VALUES(
 $1,$2,$3,(SELECT COALESCE(max(session_sequence),0)+1 FROM tasks WHERE session_key=$3),$4,
 'web',$5,$6,$6,'document tool task','[]',$7,18,2,'running','document-tool-test',
 timezone('utc',now())+interval '5 minutes',timezone('utc',now()),'worker-test',
 timezone('utc',now()),timezone('utc',now())
) RETURNING `+taskSelectColumns,
		taskID, workspaceID, sessionKey, requester, "source-"+taskID, "message-"+taskID, toolPolicy)
	task, err := scanTask(row)
	if err != nil {
		t.Fatal(err)
	}
	return task
}
