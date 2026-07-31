package postgres

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestDocumentPoolStatusReturnsSingleAggregateSnapshot(t *testing.T) {
	pool := requireDB(t)
	store := newDocumentTestStore(t, pool, 4)
	ctx := context.Background()
	workspaceID := seedDocumentWorkspace(t, pool, 501)
	oldest := time.Date(2026, 7, 31, 1, 2, 3, 0, time.UTC)

	queuedOne := uuid.NewString()
	queuedTwo := uuid.NewString()
	starting := uuid.NewString()
	for _, row := range []struct {
		id, key string
		at      time.Time
	}{
		{queuedOne, "status-queued-1", oldest},
		{queuedTwo, "status-queued-2", oldest.Add(time.Minute)},
		{starting, "status-starting", oldest.Add(2 * time.Minute)},
	} {
		if _, err := pool.Exec(ctx, `
INSERT INTO document_jobs(id,workspace_id,requester_user_id,idempotency_key,payload,payload_hash,status,created_at,updated_at,last_activity_at)
VALUES($1,$2,501,$3,'{}',$4,'queued',$5,$5,$5)`, row.id, workspaceID, row.key, "0"+"000000000000000000000000000000000000000000000000000000000000000", row.at); err != nil {
			t.Fatal(err)
		}
	}
	readyID := uuid.NewString()
	creatingID := uuid.NewString()
	allocatedID := uuid.NewString()
	if _, err := pool.Exec(ctx, `INSERT INTO document_instances(id,instance_name,slot_path,status,ready_at) VALUES($1,'status-ready','/slots/status-ready','ready',now())`, readyID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO document_instances(id,instance_name,slot_path,status) VALUES($1,'status-creating','/slots/status-creating','creating')`, creatingID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO document_instances(id,instance_name,slot_path,status,allocated_job_id,allocated_at) VALUES($1,'status-allocated','/slots/status-allocated','allocated',$2,now())`, allocatedID, starting); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE document_jobs SET status='starting',instance_id=$1,claim_owner='status-owner',generation=1,claim_lease_until=now()+interval '1 minute',claimed_at=now(),started_at=now() WHERE id=$2`, allocatedID, starting); err != nil {
		t.Fatal(err)
	}
	payloadHash := "0" + "000000000000000000000000000000000000000000000000000000000000000"
	if _, err := pool.Exec(ctx, `INSERT INTO document_commands(id,job_id,command_id,payload,payload_hash,status) VALUES($1,$2,'pending','{}',$3,'pending')`, uuid.NewString(), starting, payloadHash); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO document_commands(id,job_id,command_id,payload,payload_hash,status,generation,started_at) VALUES($1,$2,'executing','{}',$3,'executing',1,now())`, uuid.NewString(), starting, payloadHash); err != nil {
		t.Fatal(err)
	}

	status, err := store.GetDocumentPoolStatus(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if status.JobsQueued != 2 || status.JobsStarting != 1 || status.JobsRunning != 0 {
		t.Fatalf("job counts=%+v", status)
	}
	if status.InstancesReady != 1 || status.InstancesCreating != 1 || status.InstancesAllocated != 1 {
		t.Fatalf("instance counts=%+v", status)
	}
	if status.CommandsPending != 1 || status.CommandsExecuting != 1 {
		t.Fatalf("command counts=%+v", status)
	}
	if status.OldestQueuedAt == nil || !status.OldestQueuedAt.Equal(oldest) {
		t.Fatalf("oldest queued=%v want=%v", status.OldestQueuedAt, oldest)
	}
	if status.ObservedAt.IsZero() {
		t.Fatal("observed_at must come from PostgreSQL")
	}
}
