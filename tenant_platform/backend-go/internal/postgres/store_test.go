package postgres

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/domain"
)

func requireDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	return OpenTestPool(t)
}

func seedDev(t *testing.T, store *Store) DevelopmentContext {
	t.Helper()
	dev, err := store.EnsureDevelopmentContext(context.Background(), 1, "dev1")
	if err != nil {
		t.Fatal(err)
	}
	return dev
}

func TestSubmitDedupeAndCrossInstance(t *testing.T) {
	pool := requireDB(t)
	store, err := NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	dev := seedDev(t, store)

	a, err := store.SubmitTask(ctx, domain.SubmitTaskCommand{
		SessionKey: dev.SessionKey, RequesterUserID: 1,
		Source: "web", SourceInstanceID: "bot-a", MessageID: "m1",
		Prompt: "first", PersonaSnapshot: []string{"p"}, ToolPolicyVersion: "foundation.no-host-tools.v1",
	})
	if err != nil {
		t.Fatal(err)
	}
	// Duplicate different prompt returns original unchanged.
	b, err := store.SubmitTask(ctx, domain.SubmitTaskCommand{
		SessionKey: dev.SessionKey, RequesterUserID: 1,
		Source: "web", SourceInstanceID: "bot-a", MessageID: "m1",
		Prompt: "DIFFERENT", PersonaSnapshot: []string{"other"}, ToolPolicyVersion: "foundation.no-host-tools.v1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if a.ID != b.ID || b.Prompt != "first" {
		t.Fatalf("dedupe failed: a=%+v b=%+v", a, b)
	}
	// Same message under different source instance creates second task.
	c, err := store.SubmitTask(ctx, domain.SubmitTaskCommand{
		SessionKey: dev.SessionKey, RequesterUserID: 1,
		Source: "web", SourceInstanceID: "bot-b", MessageID: "m1",
		Prompt: "second-instance", PersonaSnapshot: []string{"p"}, ToolPolicyVersion: "foundation.no-host-tools.v1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if c.ID == a.ID {
		t.Fatal("expected distinct task for different source_instance_id")
	}
}

func TestClaimFIFOAndConcurrentSkipLocked(t *testing.T) {
	pool := requireDB(t)
	store, err := NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	dev := seedDev(t, store)
	t1, _ := store.SubmitTask(ctx, domain.SubmitTaskCommand{
		SessionKey: dev.SessionKey, RequesterUserID: 1, Source: "web", SourceInstanceID: "i", MessageID: "a",
		Prompt: "one", PersonaSnapshot: []string{}, ToolPolicyVersion: "foundation.no-host-tools.v1",
	})
	t2, _ := store.SubmitTask(ctx, domain.SubmitTaskCommand{
		SessionKey: dev.SessionKey, RequesterUserID: 1, Source: "web", SourceInstanceID: "i", MessageID: "b",
		Prompt: "two", PersonaSnapshot: []string{}, ToolPolicyVersion: "foundation.no-host-tools.v1",
	})
	if t1.SessionSequence >= t2.SessionSequence {
		t.Fatalf("sequence order: %d %d", t1.SessionSequence, t2.SessionSequence)
	}

	owner := "p1"
	first, ok, err := store.ClaimNextTask(ctx, dev.SessionKey, owner, time.Minute)
	if err != nil || !ok {
		t.Fatalf("claim1: %v ok=%v", err, ok)
	}
	if first.ID != t1.ID || first.Prompt != "one" {
		t.Fatalf("FIFO broke: got %s prompt=%s", first.ID, first.Prompt)
	}
	// Concurrent second claim cannot take same or violate one-running index.
	var wg sync.WaitGroup
	var got bool
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, ok2, _ := store.ClaimNextTask(ctx, dev.SessionKey, "p2", time.Minute)
		got = ok2
	}()
	wg.Wait()
	if got {
		t.Fatal("second claim should fail while first is starting")
	}
}

func TestRecoverExpiredPriorOwnerOnly(t *testing.T) {
	pool := requireDB(t)
	store, err := NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	dev := seedDev(t, store)

	// Seed expired prior-owner running-like row.
	expired, _ := store.SubmitTask(ctx, domain.SubmitTaskCommand{
		SessionKey: dev.SessionKey, RequesterUserID: 1, Source: "web", SourceInstanceID: "i", MessageID: "exp",
		Prompt: "expired", PersonaSnapshot: []string{}, ToolPolicyVersion: "foundation.no-host-tools.v1",
	})
	// Seed unexpired foreign owner.
	live, _ := store.SubmitTask(ctx, domain.SubmitTaskCommand{
		SessionKey: dev.SessionKey, RequesterUserID: 1, Source: "web", SourceInstanceID: "i", MessageID: "live",
		Prompt: "live", PersonaSnapshot: []string{}, ToolPolicyVersion: "foundation.no-host-tools.v1",
	})
	// Seed queued.
	queued, _ := store.SubmitTask(ctx, domain.SubmitTaskCommand{
		SessionKey: dev.SessionKey, RequesterUserID: 1, Source: "web", SourceInstanceID: "i", MessageID: "q",
		Prompt: "queued", PersonaSnapshot: []string{}, ToolPolicyVersion: "foundation.no-host-tools.v1",
	})

	// Manually place expired prior owner into starting.
	_, err = pool.Exec(ctx, `
UPDATE tasks SET status='starting', claim_owner='prior', claimed_at=timezone('utc', now()),
 claim_lease_until=timezone('utc', now()) - interval '1 minute'
WHERE id=$1
`, expired.ID)
	if err != nil {
		t.Fatal(err)
	}
	// Unexpired foreign owner on a different session (one_running is per session).
	// Second workspace cannot reuse bootstrap_marker; use volume_id instead.
	_, err = pool.Exec(ctx, `
INSERT INTO users (id, username, status) VALUES (2, 'dev2', 'approved')
ON CONFLICT (id) DO NOTHING;
INSERT INTO workspaces (id, session_key, owner_user_id, kind, team_id, volume_id, bootstrap_marker)
VALUES ('00000000-0000-4000-8000-000000000002', 'personal:2', 2, 'personal', NULL, 'vol-test-2', NULL)
ON CONFLICT (session_key) DO NOTHING;
`)
	if err != nil {
		t.Fatal(err)
	}
	live2, err := store.SubmitTask(ctx, domain.SubmitTaskCommand{
		SessionKey: "personal:2", RequesterUserID: 2, Source: "web", SourceInstanceID: "i", MessageID: "live2",
		Prompt: "live2", PersonaSnapshot: []string{}, ToolPolicyVersion: "foundation.no-host-tools.v1",
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = live
	_, err = pool.Exec(ctx, `
UPDATE tasks SET status='starting', claim_owner='foreign-live', claimed_at=timezone('utc', now()),
 claim_lease_until=timezone('utc', now()) + interval '10 minute'
WHERE id=$1
`, live2.ID)
	if err != nil {
		t.Fatal(err)
	}

	n, err := store.RecoverAfterRestart(ctx, "new-instance")
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("recovered=%d want 1", n)
	}
	expTask, _ := store.GetTask(ctx, expired.ID)
	if expTask.Status != domain.TaskInterrupted {
		t.Fatalf("expired status=%s", expTask.Status)
	}
	d, err := store.GetDelivery(ctx, expired.ID, domain.DeliveryTaskInterrupted)
	if err != nil || d.DeliveryID != domain.StableDeliveryID(expired.ID, domain.DeliveryTaskInterrupted) {
		t.Fatalf("delivery: %+v err=%v", d, err)
	}
	liveTask, _ := store.GetTask(ctx, live2.ID)
	if liveTask.Status != domain.TaskStarting {
		t.Fatalf("live foreign should remain starting: %s", liveTask.Status)
	}
	qTask, _ := store.GetTask(ctx, queued.ID)
	if qTask.Status != domain.TaskQueued {
		t.Fatalf("queued should remain: %s", qTask.Status)
	}
}

func TestCancelQueuedAndStartingNoDispatch(t *testing.T) {
	pool := requireDB(t)
	store, err := NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	dev := seedDev(t, store)
	q, _ := store.SubmitTask(ctx, domain.SubmitTaskCommand{
		SessionKey: dev.SessionKey, RequesterUserID: 1, Source: "web", SourceInstanceID: "i", MessageID: "cq",
		Prompt: "cancel-q", PersonaSnapshot: []string{}, ToolPolicyVersion: "foundation.no-host-tools.v1",
	})
	task, need, err := store.CancelTask(ctx, q.ID, 1)
	if err != nil || need {
		t.Fatalf("cancel queued: need=%v err=%v", need, err)
	}
	if task.Status != domain.TaskCancelled {
		t.Fatalf("status=%s", task.Status)
	}
	if _, err := store.GetDelivery(ctx, q.ID, domain.DeliveryTaskCancelled); err != nil {
		t.Fatal(err)
	}

	s, _ := store.SubmitTask(ctx, domain.SubmitTaskCommand{
		SessionKey: dev.SessionKey, RequesterUserID: 1, Source: "web", SourceInstanceID: "i", MessageID: "cs",
		Prompt: "cancel-s", PersonaSnapshot: []string{}, ToolPolicyVersion: "foundation.no-host-tools.v1",
	})
	claimed, ok, _ := store.ClaimNextTask(ctx, dev.SessionKey, "p", time.Minute)
	if !ok || claimed.ID != s.ID {
		t.Fatal("claim")
	}
	// starting without dispatch
	task, need, err = store.CancelTask(ctx, s.ID, 1)
	if err != nil || need {
		t.Fatalf("cancel starting pre-dispatch: need=%v err=%v", need, err)
	}
	if task.Status != domain.TaskCancelled {
		t.Fatalf("status=%s", task.Status)
	}
}

func TestTaskEventsNeverStorePromptText(t *testing.T) {
	pool := requireDB(t)
	store, err := NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	dev := seedDev(t, store)
	secret := "SECRET_PROMPT_VALUE_XYZ"
	task, _ := store.SubmitTask(ctx, domain.SubmitTaskCommand{
		SessionKey: dev.SessionKey, RequesterUserID: 1, Source: "web", SourceInstanceID: "i", MessageID: "ev",
		Prompt: secret, PersonaSnapshot: []string{}, ToolPolicyVersion: "foundation.no-host-tools.v1",
	})
	_ = store.RecordChunkEvent(ctx, task.ID, 3, "sha256:abc")
	var n int
	if err := pool.QueryRow(ctx, `
SELECT COUNT(*) FROM task_events WHERE task_id=$1 AND (
  COALESCE(digest,'') LIKE $2 OR COALESCE(error_code,'') LIKE $2
)`, task.ID, "%"+secret+"%").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatal("prompt leaked into task_events")
	}
	// Ensure events exist with byte_count/digest only for chunks.
	var bc int
	var dig string
	if err := pool.QueryRow(ctx, `
SELECT byte_count, digest FROM task_events WHERE task_id=$1 AND event_type='chunk'
`, task.ID).Scan(&bc, &dig); err != nil {
		t.Fatal(err)
	}
	if bc != 3 || dig == "" {
		t.Fatalf("chunk meta bc=%d dig=%s", bc, dig)
	}
}
