package postgres

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/domain"
	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/infrastructure/llmproxy"
)

func requireDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	return OpenTestPool(t)
}

func seedDev(t *testing.T, store *Store) AdminContext {
	t.Helper()
	dev, err := store.EnsureAdminContext(context.Background(), 1, "dev1")
	if err != nil {
		t.Fatal(err)
	}
	return dev
}

func TestAgentMaxTurnsSettingPersistsAndValidates(t *testing.T) {
	pool := requireDB(t)
	store, err := NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	if got, err := store.GetAgentMaxTurns(ctx); err != nil || got != domain.DefaultAgentMaxTurns {
		t.Fatalf("default max turns=%d err=%v", got, err)
	}
	if got, err := store.UpdateAgentMaxTurns(ctx, 120, 1); err != nil || got != 120 {
		t.Fatalf("updated max turns=%d err=%v", got, err)
	}
	if _, err := store.UpdateAgentMaxTurns(ctx, domain.MaxAgentMaxTurns+1, 1); err == nil {
		t.Fatal("expected max-turn validation error")
	}
}

// TestSubmitPerUserQueueLimitSerializesAcrossWorkspaces 验证 round9 修复:
// per-user 队列硬上限必须跨 workspace 串行(同一 requester 并发向个人与
// 第二 workspace 提交时, 只有一个能通过限流检查)。修复前两个事务各自持有
// 不同 workspace 行锁、计数都未超限, 双双插入突破上限。
func TestSubmitPerUserQueueLimitSerializesAcrossWorkspaces(t *testing.T) {
	pool := requireDB(t)
	store, err := NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	store.SetPerUserQueueLimit(1)
	ctx := context.Background()
	dev := seedDev(t, store)

	// 第二个 requester=1 可提交的 workspace(personal:1b, owner 仍是 1)。
	if _, err := pool.Exec(ctx, `
INSERT INTO workspaces (id, session_key, owner_user_id, kind, team_id, volume_id, bootstrap_marker)
VALUES ('00000000-0000-4000-8000-0000000000b1', 'personal:1b', 1, 'personal', NULL, 'vol-test-1b', NULL)
ON CONFLICT (session_key) DO NOTHING;
`); err != nil {
		t.Fatal(err)
	}

	// 串行基线: 第一个任务占用唯一的队列槽位; 满槽时同 workspace 也拒绝。
	first, err := store.SubmitTask(ctx, domain.SubmitTaskCommand{
		SessionKey: dev.SessionKey, RequesterUserID: 1, Source: "web", SourceInstanceID: "i", MessageID: "base",
		Prompt: "base", PersonaSnapshot: []string{}, ToolPolicyVersion: "foundation.no-host-tools.v1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.SubmitTask(ctx, domain.SubmitTaskCommand{
		SessionKey: dev.SessionKey, RequesterUserID: 1, Source: "web", SourceInstanceID: "i", MessageID: "overflow-same-ws",
		Prompt: "overflow", PersonaSnapshot: []string{}, ToolPolicyVersion: "foundation.no-host-tools.v1",
	}); !errors.Is(err, domain.ErrPerUserQueueFull) {
		t.Fatalf("same-workspace overflow: want ErrPerUserQueueFull, got %v", err)
	}

	// 释放占位槽位(取消 queued 任务), 使并发测试从空队列开始。
	if _, _, err := store.CancelTask(ctx, first.ID, 1); err != nil {
		t.Fatal(err)
	}

	// 并发跨 workspace(队列为空, limit=1): 两个事务同时进入, 修复后串行在
	// requester advisory lock 上, 第一个看到 0 个 queued 成功插入, 第二个
	// 看到 1 个 -> 拒绝。修复前两者各自持不同 workspace 行锁、计数均未超限。
	var wg sync.WaitGroup
	results := make([]error, 2)
	cmds := []domain.SubmitTaskCommand{
		{SessionKey: dev.SessionKey, RequesterUserID: 1, Source: "web", SourceInstanceID: "i", MessageID: "race-a", Prompt: "a", PersonaSnapshot: []string{}, ToolPolicyVersion: "foundation.no-host-tools.v1"},
		{SessionKey: "personal:1b", RequesterUserID: 1, Source: "web", SourceInstanceID: "i", MessageID: "race-b", Prompt: "b", PersonaSnapshot: []string{}, ToolPolicyVersion: "foundation.no-host-tools.v1"},
	}
	for i := range cmds {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, results[i] = store.SubmitTask(ctx, cmds[i])
		}(i)
	}
	wg.Wait()
	succeeded := 0
	for _, err := range results {
		if err == nil {
			succeeded++
		} else if !errors.Is(err, domain.ErrPerUserQueueFull) {
			t.Fatalf("unexpected error: %v", err)
		}
	}
	if succeeded != 1 {
		t.Fatalf("cross-workspace concurrency allowed %d inserts (want 1): results=%v", succeeded, results)
	}
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

	// Manually place expired prior owner into starting(已派发: 有 dispatch 标记,
	// 与真实运行中的任务一致——未派发场景见
	// TestRecoverAfterRestartRequeuesUndispatchedStarting)。
	_, err = pool.Exec(ctx, `
UPDATE tasks SET status='starting', claim_owner='prior', claimed_at=timezone('utc', now()),
 claim_lease_until=timezone('utc', now()) - interval '1 minute',
 worker_dispatch_started_at=timezone('utc', now())
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

// TestRecoverAfterRestartRequeuesUndispatchedStarting 验证 F4: claim 后、
// MarkDispatchStarted 前崩溃/lease 过期的 starting 任务(worker_dispatch_started_at
// IS NULL)必须退回 queued 并清空 claim 字段, 而不是被误判为 interrupted——
// 容量满载等瞬时窗口的任务从未交给 Worker 执行, 应保持排队等待重试。
func TestRecoverAfterRestartRequeuesUndispatchedStarting(t *testing.T) {
	pool := requireDB(t)
	store, err := NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	dev := seedDev(t, store)
	task, err := store.SubmitTask(ctx, domain.SubmitTaskCommand{
		SessionKey: dev.SessionKey, RequesterUserID: 1, Source: "web", SourceInstanceID: "i", MessageID: "undisp",
		Prompt: "undisp", PersonaSnapshot: []string{}, ToolPolicyVersion: "foundation.no-host-tools.v1",
	})
	if err != nil {
		t.Fatal(err)
	}
	// 未派发 starting + lease 过期。
	_, err = pool.Exec(ctx, `
UPDATE tasks SET status='starting', claim_owner='prior-undisp', claimed_at=timezone('utc', now()),
 claim_lease_until=timezone('utc', now()) - interval '1 minute',
 worker_dispatch_started_at=NULL
WHERE id=$1
`, task.ID)
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
	got, _ := store.GetTask(ctx, task.ID)
	if got.Status != domain.TaskQueued {
		t.Fatalf("undispatched expired must be requeued, got %s", got.Status)
	}
	if got.ClaimOwner != "" {
		t.Fatalf("requeued task claim_owner must be cleared: %q", got.ClaimOwner)
	}
	if !got.ClaimLeaseUntil.IsZero() {
		t.Fatalf("requeued task claim_lease_until must be cleared")
	}
	// 已派发(有 dispatch 标记)的过期 starting 仍按 interrupted 恢复。
	task2, err := store.SubmitTask(ctx, domain.SubmitTaskCommand{
		SessionKey: dev.SessionKey, RequesterUserID: 1, Source: "web", SourceInstanceID: "i", MessageID: "disp2",
		Prompt: "disp2", PersonaSnapshot: []string{}, ToolPolicyVersion: "foundation.no-host-tools.v1",
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = pool.Exec(ctx, `
UPDATE tasks SET status='starting', claim_owner='prior-disp', claimed_at=timezone('utc', now()),
 claim_lease_until=timezone('utc', now()) - interval '1 minute',
 worker_dispatch_started_at=timezone('utc', now())
WHERE id=$1
`, task2.ID)
	if err != nil {
		t.Fatal(err)
	}
	n2, err := store.RecoverAfterRestart(ctx, "new-instance")
	if err != nil {
		t.Fatal(err)
	}
	if n2 != 1 {
		t.Fatalf("recovered dispatched=%d want 1", n2)
	}
	got2, _ := store.GetTask(ctx, task2.ID)
	if got2.Status != domain.TaskInterrupted {
		t.Fatalf("dispatched expired must be interrupted, got %s", got2.Status)
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
	// RecordChunkEvent only writes for tasks in 'starting'/'running' status;
	// SubmitTask leaves the task in 'queued'. Claim it first.
	claimed, ok, _ := store.ClaimNextTask(ctx, dev.SessionKey, "p", time.Minute)
	if !ok {
		t.Fatal("claim task")
	}
	task = claimed
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

func TestCompleteSucceededMapsAcceptedCancelToInterruptedWithoutPublishingSnapshot(t *testing.T) {
	pool := requireDB(t)
	store, err := NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	dev := seedDev(t, store)
	task, err := store.SubmitTask(ctx, domain.SubmitTaskCommand{
		SessionKey: dev.SessionKey, RequesterUserID: dev.UserID,
		Source: "web", SourceInstanceID: "complete-cancel", MessageID: "complete-cancel",
		Prompt: "race", PersonaSnapshot: []string{}, ToolPolicyVersion: "foundation.no-host-tools.v1",
	})
	if err != nil {
		t.Fatal(err)
	}
	claimed, ok, err := store.ClaimNextTask(ctx, dev.SessionKey, "owner-cancel", time.Minute)
	if err != nil || !ok {
		t.Fatalf("claim: ok=%v err=%v", ok, err)
	}
	if _, err := store.MarkDispatchStarted(ctx, claimed.ID, "owner-cancel", "worker-cancel", false); err != nil {
		t.Fatal(err)
	}
	if _, err := store.MarkRunning(ctx, claimed.ID, "owner-cancel"); err != nil {
		t.Fatal(err)
	}
	stagingRefFor := func(snapshotID, token string, generation int64) string { return "staging" }
	snapshotID, _, _, err := store.PrepareCheckpoint(ctx, task.ID, "owner-cancel", 1, stagingRefFor, 1024)
	if err != nil {
		t.Fatal(err)
	}
	if _, needWorker, err := store.CancelTask(ctx, task.ID, dev.UserID); err != nil || !needWorker {
		t.Fatalf("cancel: needWorker=%v err=%v", needWorker, err)
	}
	final, err := store.CompleteSucceeded(ctx, task.ID, "owner-cancel", snapshotID,
		"snapshot:race", "sha256:bundle", "result:race", "sha256:result", 6, nil)
	if err != nil {
		t.Fatal(err)
	}
	if final.Status != domain.TaskInterrupted {
		t.Fatalf("status=%s want interrupted", final.Status)
	}
	var snapshotState string
	if err := pool.QueryRow(ctx, `SELECT state FROM workspace_snapshots WHERE id=$1::uuid`, snapshotID).Scan(&snapshotState); err != nil {
		t.Fatal(err)
	}
	if snapshotState == "committed" {
		t.Fatal("cancelled task published its checkpoint")
	}
	var currentSnapshot *string
	if err := pool.QueryRow(ctx, `SELECT current_snapshot_id::text FROM workspaces WHERE id=$1::uuid`, task.WorkspaceID).Scan(&currentSnapshot); err != nil {
		t.Fatal(err)
	}
	if currentSnapshot != nil {
		t.Fatalf("workspace current snapshot=%s want null", *currentSnapshot)
	}
	if _, err := store.GetDelivery(ctx, task.ID, domain.DeliveryTaskInterrupted); err != nil {
		t.Fatal(err)
	}
	if _, err := store.GetDelivery(ctx, task.ID, domain.DeliveryTaskComplete); err == nil {
		t.Fatal("unexpected task_complete delivery")
	}
}

func TestRecordChunkEventSerializesConcurrentSequenceAllocation(t *testing.T) {
	pool := requireDB(t)
	store, err := NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	dev := seedDev(t, store)
	task, err := store.SubmitTask(ctx, domain.SubmitTaskCommand{
		SessionKey: dev.SessionKey, RequesterUserID: dev.UserID,
		Source: "web", SourceInstanceID: "events", MessageID: "events",
		Prompt: "events", PersonaSnapshot: []string{}, ToolPolicyVersion: "foundation.no-host-tools.v1",
	})
	if err != nil {
		t.Fatal(err)
	}
	// RecordChunkEvent only writes for 'starting'/'running' tasks; claim first.
	claimed, ok, err := store.ClaimNextTask(ctx, dev.SessionKey, "p", time.Minute)
	if err != nil || !ok {
		t.Fatalf("claim: ok=%v err=%v", ok, err)
	}
	task = claimed
	const writers = 16
	start := make(chan struct{})
	errs := make(chan error, writers)
	var wg sync.WaitGroup
	for range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			errs <- store.RecordChunkEvent(ctx, task.ID, 1, "sha256:chunk")
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("record chunk event: %v", err)
		}
	}
	var total, distinct int
	if err := pool.QueryRow(ctx, `
SELECT count(*), count(DISTINCT sequence_no) FROM task_events WHERE task_id=$1 AND event_type='chunk'
`, task.ID).Scan(&total, &distinct); err != nil {
		t.Fatal(err)
	}
	if total != writers || distinct != writers {
		t.Fatalf("events total=%d distinct_sequences=%d want=%d", total, distinct, writers)
	}
}

func TestEnsureSchemaCreatesMaxBundleColumnOnFreshDB(t *testing.T) {
	pool := requireDB(t)
	ctx := context.Background()
	// G-M6: runtime ALTER patches were removed. EnsureSchema only applies the
	// migration on a fresh DB (tasks table absent). Verify the base migration
	// includes max_bundle_bytes rather than expecting runtime upgrades.
	if err := DropFoundationSchema(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if err := EnsureSchema(ctx, pool, DefaultMigrationPath()); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := pool.QueryRow(ctx, `
SELECT count(*) FROM information_schema.columns
WHERE table_schema='public' AND table_name='workspace_snapshots' AND column_name='max_bundle_bytes'
`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("max_bundle_bytes column missing on fresh DB (count=%d)", n)
	}
}

// TestSetTaskCapabilityJTIsRequiresActiveClaim 验证审查 R5-I2: JTI 持久化
// 必须绑定任务活跃 claim——已终态/被接管/lease 过期的任务行不得接受新签发
// 的 JTI(否则崩溃窗口内无人撤销)。
func TestSetTaskCapabilityJTIsRequiresActiveClaim(t *testing.T) {
	pool := requireDB(t)
	store, err := NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	dev := seedDev(t, store)

	submit := func(msgID string) domain.Task {
		t.Helper()
		task, err := store.SubmitTask(ctx, domain.SubmitTaskCommand{
			SessionKey: dev.SessionKey, RequesterUserID: 1,
			Source: "web", SourceInstanceID: "bot-jti", MessageID: msgID,
			Prompt: "jti-claim", PersonaSnapshot: []string{"p"}, ToolPolicyVersion: "foundation.no-host-tools.v1",
		})
		if err != nil {
			t.Fatal(err)
		}
		return task
	}

	// queued(未 claim)必须拒绝。
	q := submit("jti-q")
	if err := store.SetTaskCapabilityJTIs(ctx, q.ID, "p1", []string{"jti-x"}); err == nil {
		t.Fatal("SetTaskCapabilityJTIs on queued task must fail")
	}
	// starting + 本实例 claim 必须成功。
	claimed, ok, err := store.ClaimNextTask(ctx, dev.SessionKey, "p1", time.Minute)
	if err != nil || !ok {
		t.Fatalf("claim: %v ok=%v", err, ok)
	}
	if err := store.SetTaskCapabilityJTIs(ctx, claimed.ID, "p1", []string{"jti-1"}); err != nil {
		t.Fatalf("SetTaskCapabilityJTIs on active claim: %v", err)
	}
	// 其他实例 owner 必须拒绝(用第二个 session 避免同 session 活跃约束)。
	dev2, err := store.EnsureAdminContext(ctx, 2, "dev2")
	if err != nil {
		t.Fatal(err)
	}
	otherTask, err := store.SubmitTask(ctx, domain.SubmitTaskCommand{
		SessionKey: dev2.SessionKey, RequesterUserID: 2,
		Source: "web", SourceInstanceID: "bot-jti2", MessageID: "jti-o",
		Prompt: "jti-claim-2", PersonaSnapshot: []string{"p"}, ToolPolicyVersion: "foundation.no-host-tools.v1",
	})
	if err != nil {
		t.Fatal(err)
	}
	other, ok, err := store.ClaimNextTask(ctx, dev2.SessionKey, "p2", time.Minute)
	if err != nil || !ok {
		t.Fatalf("second claim: %v ok=%v", err, ok)
	}
	_ = otherTask
	if err := store.SetTaskCapabilityJTIs(ctx, other.ID, "p1", []string{"jti-2"}); err == nil {
		t.Fatal("SetTaskCapabilityJTIs with foreign owner must fail")
	}
	// 终态任务必须拒绝。
	if _, err := store.CompleteFailedTerminal(ctx, other.ID, "p2", domain.TaskFailed, domain.DeliveryTaskFailed, "E", "m", ""); err != nil {
		t.Fatal(err)
	}
	if err := store.SetTaskCapabilityJTIs(ctx, other.ID, "p2", []string{"jti-3"}); err == nil {
		t.Fatal("SetTaskCapabilityJTIs on terminal task must fail")
	}
}

// TestCompleteSucceededPersistsDeliveryFiles 验证审查 R5-I3: 成功事务把
// [FILE:...] 输出文件快照与任务成功状态原子绑定到 task_complete outbox,
// delivery 可经 LoadDeliveryFiles 读取快照内容。
func TestCompleteSucceededPersistsDeliveryFiles(t *testing.T) {
	pool := requireDB(t)
	store, err := NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	dev := seedDev(t, store)
	task, err := store.SubmitTask(ctx, domain.SubmitTaskCommand{
		SessionKey: dev.SessionKey, RequesterUserID: 1,
		Source: "web", SourceInstanceID: "bot-df", MessageID: "df-1",
		Prompt: "files", PersonaSnapshot: []string{"p"}, ToolPolicyVersion: "foundation.no-host-tools.v1",
	})
	if err != nil {
		t.Fatal(err)
	}
	claimed, ok, err := store.ClaimNextTask(ctx, dev.SessionKey, "p1", time.Minute)
	if err != nil || !ok {
		t.Fatalf("claim: %v ok=%v", err, ok)
	}
	// Prepare checkpoint(需要 workspace 行锁与 lease 校验; loopback 无 lease 行跳过)。
	snapshotID, token, gen, err := store.PrepareCheckpoint(ctx, claimed.ID, "p1", 1, func(sid, tok string, g int64) string {
		return "staging:" + sid + ":" + tok
	}, 1<<20)
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	_ = token
	_ = gen
	files := []domain.DeliveryFile{
		{Marker: "outputs/report.docx", FileName: "report.docx", RelPath: "outputs/report.docx",
			Content: []byte("final-content"), Digest: "sha256:abc", SizeBytes: 13},
		// spool 引用行(2026-08-14 修复回归): Content 为 nil、SpoolPath 非空——
		// 插入必须落空 bytea(COALESCE), 否则 content NOT NULL 违反→成功事务回滚。
		{Marker: "outputs/image.png", FileName: "image.png", RelPath: "outputs/image.png",
			Digest: "sha256:def", SizeBytes: 5, SpoolPath: "capture/k/image_abc.png"},
	}
	final, err := store.CompleteSucceeded(ctx, claimed.ID, "p1", snapshotID,
		"snapshot:df", "sha256:bundle", "result:df", "sha256:result", 5, files)
	if err != nil {
		t.Fatalf("CompleteSucceeded: %v", err)
	}
	if final.Status != domain.TaskSucceeded {
		t.Fatalf("status = %s", final.Status)
	}
	// LoadDeliveryFiles 必须返回快照(ORDER BY marker: image.png 在前)。
	got, err := store.LoadDeliveryFiles(ctx, domain.StableDeliveryID(task.ID, domain.DeliveryTaskComplete))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("delivery files = %+v", got)
	}
	if got[0].Marker != "outputs/image.png" || got[0].SpoolPath != "capture/k/image_abc.png" || len(got[0].Content) != 0 {
		t.Fatalf("spool delivery file = %+v", got[0])
	}
	if got[1].Marker != "outputs/report.docx" || string(got[1].Content) != "final-content" {
		t.Fatalf("content delivery file = %+v", got[1])
	}
}

// TestRemoveMemberCancelsDispatchedTasksAndScopesContextClear 验证审查 R5-I4:
// 移除团队成员时, 已派发(starting/running)任务写入 durable cancel_requested_at
// (scheduler 轮询执行 Worker cancel, 终态撤销 JTI); active_contexts 清理
// 限定当前团队(不抹掉用户其他团队上下文)。
func TestRemoveMemberCancelsDispatchedTasksAndScopesContextClear(t *testing.T) {
	pool := requireDB(t)
	store, err := NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	seedDev(t, store)
	if _, err := store.EnsureAdminContext(ctx, 2, "dev2"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.EnsureAdminContext(ctx, 3, "dev3"); err != nil {
		t.Fatal(err)
	}
	// owner=1 建团队, member=2 加入。
	team, err := store.CreateTeam(ctx, 1, "review-team")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
INSERT INTO team_members (team_id, user_id, role, status)
VALUES ($1::uuid, 2, 'member', 'approved')
`, team.ID); err != nil {
		t.Fatal(err)
	}
	var memberID int64
	if err := pool.QueryRow(ctx, `SELECT id FROM team_members WHERE team_id = $1::uuid AND user_id = 2`, team.ID).Scan(&memberID); err != nil {
		t.Fatal(err)
	}
	// 另一团队上下文(用户 2 正在使用): 移除后不得被清掉。
	other, err := store.CreateTeam(ctx, 3, "other-team")
	if err != nil {
		t.Fatal(err)
	}
	// 用户 2 同时是 other 团队的成员(正在使用该团队上下文)。
	if _, err := pool.Exec(ctx, `
INSERT INTO team_members (team_id, user_id, role, status)
VALUES ($1::uuid, 2, 'member', 'approved')
`, other.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SetActiveContextTeam(ctx, 2, other.ID); err != nil {
		t.Fatal(err)
	}
	// 用户 2 在目标团队提交任务并 claim(starting, 已派发)。
	sessionKey := fmt.Sprintf("team:%s", team.ID)
	task, err := store.SubmitTask(ctx, domain.SubmitTaskCommand{
		SessionKey: sessionKey, RequesterUserID: 2,
		Source: "web", SourceInstanceID: "bot-rm", MessageID: "rm-1",
		Prompt: "member task", PersonaSnapshot: []string{"p"}, ToolPolicyVersion: "foundation.no-host-tools.v1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, err := store.ClaimNextTask(ctx, sessionKey, "p1", time.Minute); err != nil || !ok {
		t.Fatalf("claim: %v ok=%v", err, ok)
	}
	// 模拟已派发(dispatch 已 MarkDispatchStarted)。
	if _, err := store.MarkDispatchStarted(ctx, task.ID, "p1", "worker-1", false); err != nil {
		t.Fatalf("MarkDispatchStarted: %v", err)
	}
	// 移除成员。
	if _, err := store.RemoveMember(ctx, fmt.Sprintf("t-%d", memberID), 1); err != nil {
		t.Fatalf("RemoveMember: %v", err)
	}
	// 已派发任务必须带 durable cancel_requested_at(未终态化, 由 scheduler 收尾)。
	got, err := store.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.CancelRequestedAt == nil {
		t.Fatal("dispatched task must have durable cancel_requested_at after member removal")
	}
	if got.Status != domain.TaskStarting {
		t.Fatalf("dispatched task status = %s, want starting (scheduler drives terminalization)", got.Status)
	}
	// active_contexts: 用户 2 的其他团队上下文保留。
	ac, err := store.GetActiveContext(ctx, 2)
	if err != nil {
		t.Fatal(err)
	}
	if ac.TeamID == nil || *ac.TeamID != other.ID {
		t.Fatalf("other team context must be preserved, got %v", ac.TeamID)
	}
}

// 审查 R5-Critical-2: 失败终态必须由当前 claim owner 在 lease 有效期内
// 执行——旧实例在 lease 被接管/过期后不得把新 owner 的任务终态化。
func TestCompleteFailedTerminalRequiresOwnedActiveClaim(t *testing.T) {
	pool := requireDB(t)
	store, err := NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	dev := seedDev(t, store)
	if _, err := store.SubmitTask(ctx, domain.SubmitTaskCommand{
		SessionKey: dev.SessionKey, RequesterUserID: 1,
		Source: "web", SourceInstanceID: "bot-fn", MessageID: "fn-1",
		Prompt: "fencing", PersonaSnapshot: []string{"p"}, ToolPolicyVersion: "foundation.no-host-tools.v1",
	}); err != nil {
		t.Fatal(err)
	}
	claimed, ok, err := store.ClaimNextTask(ctx, dev.SessionKey, "p1", time.Minute)
	if err != nil || !ok {
		t.Fatalf("claim: %v ok=%v", err, ok)
	}
	// 其他实例(非 owner)不得终态化。
	if _, err := store.CompleteFailedTerminal(ctx, claimed.ID, "p2", domain.TaskFailed, domain.DeliveryTaskFailed, "E", "m", ""); !errors.Is(err, ErrTaskNotOwned) {
		t.Fatalf("foreign owner finalize err = %v, want ErrTaskNotOwned", err)
	}
	got, err := store.GetTask(ctx, claimed.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != domain.TaskStarting {
		t.Fatalf("task status = %s after foreign finalize, want starting", got.Status)
	}
	// 空 owner 同样拒绝。
	if _, err := store.CompleteFailedTerminal(ctx, claimed.ID, "", domain.TaskFailed, domain.DeliveryTaskFailed, "E", "m", ""); !errors.Is(err, ErrTaskNotOwned) {
		t.Fatalf("empty owner finalize err = %v, want ErrTaskNotOwned", err)
	}
	// owner 匹配且 lease 有效: 成功终态。
	if _, err := store.CompleteFailedTerminal(ctx, claimed.ID, "p1", domain.TaskFailed, domain.DeliveryTaskFailed, "E", "m", ""); err != nil {
		t.Fatalf("owner finalize: %v", err)
	}
	got, err = store.GetTask(ctx, claimed.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != domain.TaskFailed {
		t.Fatalf("task status = %s after owner finalize, want failed", got.Status)
	}
}

func TestCompleteFailedTerminalRejectsExpiredLease(t *testing.T) {
	pool := requireDB(t)
	store, err := NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	dev := seedDev(t, store)
	if _, err := store.SubmitTask(ctx, domain.SubmitTaskCommand{
		SessionKey: dev.SessionKey, RequesterUserID: 1,
		Source: "web", SourceInstanceID: "bot-fn2", MessageID: "fn-2",
		Prompt: "fencing-expired", PersonaSnapshot: []string{"p"}, ToolPolicyVersion: "foundation.no-host-tools.v1",
	}); err != nil {
		t.Fatal(err)
	}
	claimed, ok, err := store.ClaimNextTask(ctx, dev.SessionKey, "p1", time.Minute)
	if err != nil || !ok {
		t.Fatalf("claim: %v ok=%v", err, ok)
	}
	// 模拟 lease 已过期(旧实例续租失败)。
	if _, err := pool.Exec(ctx, `UPDATE tasks SET claim_lease_until = timezone('utc', now()) - interval '1 second' WHERE id = $1`, claimed.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CompleteFailedTerminal(ctx, claimed.ID, "p1", domain.TaskFailed, domain.DeliveryTaskFailed, "E", "m", ""); !errors.Is(err, ErrTaskNotOwned) {
		t.Fatalf("expired lease finalize err = %v, want ErrTaskNotOwned", err)
	}
	got, err := store.GetTask(ctx, claimed.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != domain.TaskStarting {
		t.Fatalf("task status = %s after expired-lease finalize, want starting", got.Status)
	}
}

// 审查 R5-I4: 移除成员时 starting 但尚未 MarkDispatchStarted 的任务不得
// 永久卡在 starting——直接终态化(cancelled), 而不是只写 cancel_requested_at
// 依赖 dispatch 兜底(dispatch 对未派发取消任务直接 return, 无人收尾)。
func TestRemoveMemberTerminalizesUndispatchedStartingTask(t *testing.T) {
	pool := requireDB(t)
	store, err := NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	seedDev(t, store)
	if _, err := store.EnsureAdminContext(ctx, 2, "dev2"); err != nil {
		t.Fatal(err)
	}
	team, err := store.CreateTeam(ctx, 1, "rm-team2")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
INSERT INTO team_members (team_id, user_id, role, status)
VALUES ($1::uuid, 2, 'member', 'approved')
`, team.ID); err != nil {
		t.Fatal(err)
	}
	var memberID int64
	if err := pool.QueryRow(ctx, `SELECT id FROM team_members WHERE team_id = $1::uuid AND user_id = 2`, team.ID).Scan(&memberID); err != nil {
		t.Fatal(err)
	}
	sessionKey := fmt.Sprintf("team:%s", team.ID)
	if _, err := store.SubmitTask(ctx, domain.SubmitTaskCommand{
		SessionKey: sessionKey, RequesterUserID: 2,
		Source: "web", SourceInstanceID: "bot-rm2", MessageID: "rm-2",
		Prompt: "member task undispatched", PersonaSnapshot: []string{"p"}, ToolPolicyVersion: "foundation.no-host-tools.v1",
	}); err != nil {
		t.Fatal(err)
	}
	claimed, ok, err := store.ClaimNextTask(ctx, sessionKey, "p1", time.Minute)
	if err != nil || !ok {
		t.Fatalf("claim: %v ok=%v", err, ok)
	}
	if claimed.WorkerDispatchStartedAt != nil {
		t.Fatal("claimed task must not have dispatch started yet")
	}
	if _, err := store.RemoveMember(ctx, fmt.Sprintf("t-%d", memberID), 1); err != nil {
		t.Fatalf("RemoveMember: %v", err)
	}
	got, err := store.GetTask(ctx, claimed.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != domain.TaskCancelled {
		t.Fatalf("undispatched starting task status = %s after removal, want cancelled", got.Status)
	}
	if got.ClaimOwner != "" || !got.ClaimLeaseUntil.IsZero() {
		t.Fatalf("undispatched starting task claim must be cleared after removal: owner=%q lease=%v", got.ClaimOwner, got.ClaimLeaseUntil)
	}
	if got.CancelRequestedAt != nil {
		t.Fatal("terminalized task must not carry cancel_requested_at")
	}
}

// 审查 R5-M2: 终态事务必须取消尚未发送的 task_started delivery——否则
// task_started 发送失败重试期间, 用户会先收到完成消息、后收到"正在处理"。
func TestTerminalFinalizeCancelsPendingTaskStartedDelivery(t *testing.T) {
	pool := requireDB(t)
	store, err := NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	dev := seedDev(t, store)
	if _, err := store.SubmitTask(ctx, domain.SubmitTaskCommand{
		SessionKey: dev.SessionKey, RequesterUserID: 1,
		Source: "web", SourceInstanceID: "bot-ts", MessageID: "ts-1",
		Prompt: "started-cancel", PersonaSnapshot: []string{"p"}, ToolPolicyVersion: "foundation.no-host-tools.v1",
	}); err != nil {
		t.Fatal(err)
	}
	claimed, ok, err := store.ClaimNextTask(ctx, dev.SessionKey, "p1", time.Minute)
	if err != nil || !ok {
		t.Fatalf("claim: %v ok=%v", err, ok)
	}
	// task_started delivery 已随 SubmitTask 插入(pending)。
	var stStatus string
	if err := pool.QueryRow(ctx, `SELECT status FROM task_deliveries WHERE task_id = $1 AND delivery_type = 'task_started'`, claimed.ID).Scan(&stStatus); err != nil {
		t.Fatal(err)
	}
	if stStatus != "pending" {
		t.Fatalf("task_started status = %q, want pending", stStatus)
	}
	if _, err := store.CompleteFailedTerminal(ctx, claimed.ID, "p1", domain.TaskFailed, domain.DeliveryTaskFailed, "E", "m", ""); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT status FROM task_deliveries WHERE task_id = $1 AND delivery_type = 'task_started'`, claimed.ID).Scan(&stStatus); err != nil {
		t.Fatal(err)
	}
	if stStatus != "cancelled" {
		t.Fatalf("task_started status after terminal = %q, want cancelled", stStatus)
	}
	// 终态 delivery 本身仍 pending 等待发送。
	var failStatus string
	if err := pool.QueryRow(ctx, `SELECT status FROM task_deliveries WHERE task_id = $1 AND delivery_type = 'task_failed'`, claimed.ID).Scan(&failStatus); err != nil {
		t.Fatal(err)
	}
	if failStatus != "pending" {
		t.Fatalf("task_failed status = %q, want pending", failStatus)
	}
}

// TestSetTaskCapabilityJTIsAppendsAndDeduplicates 验证审查 C1(I4): 刷新凭据
// 时 SetTaskCapabilityJTIs 必须**追加去重**而不是整体替换——旧 JTI 对应的
// token 在 Worker 确认前尚未撤销, 若被覆盖, Platform 崩溃后恢复事务只能
// 撤销新 JTI, 旧 token 存活至 TTL。
func TestSetTaskCapabilityJTIsAppendsAndDeduplicates(t *testing.T) {
	pool := requireDB(t)
	store, err := NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	dev := seedDev(t, store)

	task, err := store.SubmitTask(ctx, domain.SubmitTaskCommand{
		SessionKey: dev.SessionKey, RequesterUserID: 1,
		Source: "web", SourceInstanceID: "bot-jti-append", MessageID: "jti-a",
		Prompt: "jti-append", PersonaSnapshot: []string{"p"}, ToolPolicyVersion: "foundation.no-host-tools.v1",
	})
	if err != nil {
		t.Fatal(err)
	}
	claimed, ok, err := store.ClaimNextTask(ctx, dev.SessionKey, "p1", time.Minute)
	if err != nil || !ok {
		t.Fatalf("claim: %v ok=%v", err, ok)
	}
	_ = task

	if err := store.SetTaskCapabilityJTIs(ctx, claimed.ID, "p1", []string{"jti-old-1", "jti-old-2"}); err != nil {
		t.Fatalf("first SetTaskCapabilityJTIs: %v", err)
	}
	// 刷新: 新 JTI 与旧 JTI 部分重叠(重复项必须去重)。
	if err := store.SetTaskCapabilityJTIs(ctx, claimed.ID, "p1", []string{"jti-old-2", "jti-new"}); err != nil {
		t.Fatalf("second SetTaskCapabilityJTIs: %v", err)
	}
	row := store.pool.QueryRow(ctx, `SELECT capability_jtis FROM tasks WHERE id = $1`, claimed.ID)
	var jtis []string
	if err := row.Scan(&jtis); err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, j := range jtis {
		got[j] = true
	}
	for _, want := range []string{"jti-old-1", "jti-old-2", "jti-new"} {
		if !got[want] {
			t.Fatalf("capability_jtis = %v, missing %q (must append+dedupe, not replace)", jtis, want)
		}
	}
	if len(jtis) != 3 {
		t.Fatalf("capability_jtis = %v, want exactly 3 unique entries", jtis)
	}
}

// TestRemoveMemberRevokesCapabilityJTIsOfUndispatchedStartingTask 验证审查
// C1(I5): 移除成员直接终态化 starting 未派发任务时, 必须走统一终态逻辑
// (finalizeTerminal)——已签发并暴露给 Runner 的 capability JTI 必须与任务
// 终态在同一事务内写入撤销表, 否则 Platform 崩溃后该终态行不被恢复扫描,
// 旧 token 在 TTL 内仍可调用模型。
func TestRemoveMemberRevokesCapabilityJTIsOfUndispatchedStartingTask(t *testing.T) {
	pool := requireDB(t)
	store, err := NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	seedDev(t, store)
	if _, err := store.EnsureAdminContext(ctx, 2, "dev2"); err != nil {
		t.Fatal(err)
	}
	team, err := store.CreateTeam(ctx, 1, "rm-jti-team")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
INSERT INTO team_members (team_id, user_id, role, status)
VALUES ($1::uuid, 2, 'member', 'approved')
`, team.ID); err != nil {
		t.Fatal(err)
	}
	var memberID int64
	if err := pool.QueryRow(ctx, `SELECT id FROM team_members WHERE team_id = $1::uuid AND user_id = 2`, team.ID).Scan(&memberID); err != nil {
		t.Fatal(err)
	}
	sessionKey := fmt.Sprintf("team:%s", team.ID)
	if _, err := store.SubmitTask(ctx, domain.SubmitTaskCommand{
		SessionKey: sessionKey, RequesterUserID: 2,
		Source: "web", SourceInstanceID: "bot-rm-jti", MessageID: "rm-jti-1",
		Prompt: "member task jti", PersonaSnapshot: []string{"p"}, ToolPolicyVersion: "foundation.no-host-tools.v1",
	}); err != nil {
		t.Fatal(err)
	}
	claimed, ok, err := store.ClaimNextTask(ctx, sessionKey, "p1", time.Minute)
	if err != nil || !ok {
		t.Fatalf("claim: %v ok=%v", err, ok)
	}
	// 签发 JTI(签发先于 MarkDispatchStarted, 与生产路径一致)。
	if err := store.SetTaskCapabilityJTIs(ctx, claimed.ID, "p1", []string{"jti-member-1", "jti-member-2"}); err != nil {
		t.Fatalf("SetTaskCapabilityJTIs: %v", err)
	}
	if _, err := store.RemoveMember(ctx, fmt.Sprintf("t-%d", memberID), 1); err != nil {
		t.Fatalf("RemoveMember: %v", err)
	}
	// 撤销表必须包含全部已签发 JTI。
	for _, jti := range []string{"jti-member-1", "jti-member-2"} {
		var n int
		if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM llm_capability_revocations WHERE jti_hash = $1`, func() []byte { d := llmproxy.HashJTI(jti); return d[:] }()).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 1 {
			t.Fatalf("capability JTI %s must be revoked after member removal (rows=%d)", jti, n)
		}
	}
	// 统一终态逻辑必须写 status_transition 事件(供审计/前端追踪)。
	var n int
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM task_events WHERE task_id = $1 AND event_type = 'status_transition'`, claimed.ID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n == 0 {
		t.Fatal("member-removal terminalization must write a status_transition event")
	}
}

// round9 审查: MarkDispatchStarted/MarkRunning 必须拒绝 claim lease 已过期
// 的任务(进程暂停/心跳丢失后恢复不得继续派发, 防与接管者重叠执行)。
func TestMarkDispatchStartedRejectsExpiredClaim(t *testing.T) {
	pool := requireDB(t)
	store, err := NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	dev := seedDev(t, store)
	task, err := store.SubmitTask(ctx, domain.SubmitTaskCommand{
		SessionKey: dev.SessionKey, RequesterUserID: 1, Source: "web", SourceInstanceID: "i", MessageID: "exp-claim",
		Prompt: "p", PersonaSnapshot: []string{}, ToolPolicyVersion: "foundation.no-host-tools.v1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, err := store.ClaimNextTask(ctx, dev.SessionKey, "owner-1", time.Minute); err != nil || !ok {
		t.Fatalf("claim: ok=%v err=%v", ok, err)
	}
	// 模拟心跳丢失: lease 过期。
	if _, err := pool.Exec(ctx, `
UPDATE tasks SET claim_lease_until = timezone('utc', now()) - interval '1 second' WHERE id = $1
`, task.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.MarkDispatchStarted(ctx, task.ID, "owner-1", "w1", false); err == nil {
		t.Fatal("MarkDispatchStarted must reject expired claim")
	}
	// 续租(直接 SQL 模拟心跳成功)后可以派发; 再次过期后 MarkRunning 也必须拒绝。
	if _, err := pool.Exec(ctx, `
UPDATE tasks SET claim_lease_until = timezone('utc', now()) + interval '10 minutes' WHERE id = $1
`, task.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.MarkDispatchStarted(ctx, task.ID, "owner-1", "w1", false); err != nil {
		t.Fatalf("MarkDispatchStarted after renew: %v", err)
	}
	if _, err := pool.Exec(ctx, `
UPDATE tasks SET claim_lease_until = timezone('utc', now()) - interval '1 second' WHERE id = $1
`, task.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.MarkRunning(ctx, task.ID, "owner-1"); err == nil {
		t.Fatal("MarkRunning must reject expired claim")
	}
}

// round9 审查: 成员移除必须在移除事务内立即撤销已派发任务的 capability
// JTI——不等终态事务(若 scheduler 停机/取消 RPC 卡住, 旧 token 在 TTL 内
// 仍可调用 LLM/Sophub)。
func TestRemoveMemberRevokesDispatchedTaskJTIs(t *testing.T) {
	pool := requireDB(t)
	store, err := NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	seedDev(t, store)
	if _, err := store.EnsureAdminContext(ctx, 2, "dev2"); err != nil {
		t.Fatal(err)
	}
	team, err := store.CreateTeam(ctx, 1, "jti-team")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
INSERT INTO team_members (team_id, user_id, role, status)
VALUES ($1::uuid, 2, 'member', 'approved')
`, team.ID); err != nil {
		t.Fatal(err)
	}
	var memberID int64
	if err := pool.QueryRow(ctx, `SELECT id FROM team_members WHERE team_id = $1::uuid AND user_id = 2`, team.ID).Scan(&memberID); err != nil {
		t.Fatal(err)
	}
	sessionKey := fmt.Sprintf("team:%s", team.ID)
	task, err := store.SubmitTask(ctx, domain.SubmitTaskCommand{
		SessionKey: sessionKey, RequesterUserID: 2,
		Source: "web", SourceInstanceID: "bot-jti", MessageID: "rm-jti",
		Prompt: "member task", PersonaSnapshot: []string{"p"}, ToolPolicyVersion: "foundation.no-host-tools.v1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, err := store.ClaimNextTask(ctx, sessionKey, "p1", time.Minute); err != nil || !ok {
		t.Fatalf("claim: %v ok=%v", ok, err)
	}
	if _, err := store.MarkDispatchStarted(ctx, task.ID, "p1", "worker-1", false); err != nil {
		t.Fatal(err)
	}
	if err := store.SetTaskCapabilityJTIs(ctx, task.ID, "p1", []string{"jti-member-removed"}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.RemoveMember(ctx, fmt.Sprintf("t-%d", memberID), 1); err != nil {
		t.Fatalf("RemoveMember: %v", err)
	}
	revoked, err := store.IsCapabilityRevoked(ctx, llmproxy.HashJTI("jti-member-removed"))
	if err != nil {
		t.Fatal(err)
	}
	if !revoked {
		t.Fatal("dispatched task JTI must be revoked inside the removal transaction")
	}
	got, err := store.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.CancelRequestedAt == nil {
		t.Fatal("dispatched task must still carry durable cancel_requested_at")
	}
}

// round9 审查: 在线 task 活跃性校验的状态矩阵。
func TestIsTaskCapabilityActiveMatrix(t *testing.T) {
	pool := requireDB(t)
	store, err := NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	dev := seedDev(t, store)
	// runner lease 行(generation=1, 未过期)。
	if _, err := pool.Exec(ctx, `
INSERT INTO runner_leases (runner_key, owner, generation, status, expires_at)
VALUES ($1, 'p1', 1, 'active', timezone('utc', now()) + interval '10 minutes')
`, dev.SessionKey); err != nil {
		t.Fatal(err)
	}
	task, err := store.SubmitTask(ctx, domain.SubmitTaskCommand{
		SessionKey: dev.SessionKey, RequesterUserID: 1, Source: "web", SourceInstanceID: "i", MessageID: "active-matrix",
		Prompt: "p", PersonaSnapshot: []string{}, ToolPolicyVersion: "foundation.no-host-tools.v1",
	})
	if err != nil {
		t.Fatal(err)
	}
	// queued 状态: 不活跃。
	if active, err := store.IsTaskCapabilityActive(ctx, task.ID, 1); err != nil || active {
		t.Fatalf("queued task must be inactive, active=%v err=%v", active, err)
	}
	if _, ok, err := store.ClaimNextTask(ctx, dev.SessionKey, "p1", time.Minute); err != nil || !ok {
		t.Fatalf("claim: %v ok=%v", ok, err)
	}
	if _, err := store.MarkDispatchStarted(ctx, task.ID, "p1", "w1", false); err != nil {
		t.Fatal(err)
	}
	// starting + 有效 lease: 活跃。
	if active, err := store.IsTaskCapabilityActive(ctx, task.ID, 1); err != nil || !active {
		t.Fatalf("starting task with valid lease must be active, active=%v err=%v", active, err)
	}
	// generation 不匹配: 不活跃。
	if active, err := store.IsTaskCapabilityActive(ctx, task.ID, 2); err != nil || active {
		t.Fatalf("generation mismatch must be inactive, active=%v err=%v", active, err)
	}
	// lease 过期: 不活跃。
	if _, err := pool.Exec(ctx, `
UPDATE runner_leases SET expires_at = timezone('utc', now()) - interval '1 second'
WHERE runner_key = $1
`, dev.SessionKey); err != nil {
		t.Fatal(err)
	}
	if active, err := store.IsTaskCapabilityActive(ctx, task.ID, 1); err != nil || active {
		t.Fatalf("expired lease must be inactive, active=%v err=%v", active, err)
	}
}

// round10 审查(B5): BlockUser 必须终态化未派发 starting 任务(而不是只写
// cancel_requested_at)——dispatch 对未派发取消任务直接 return, 残留任务会
// 永久卡在 starting 占住串行槽; 同时 pending task_started delivery 必须取消
// (用户不得收到"正在处理"却无后续), claim 必须清空。
func TestBlockUserTerminalizesUndispatchedStartingTask(t *testing.T) {
	pool := requireDB(t)
	store, err := NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	dev := seedDev(t, store)

	if _, err := store.SubmitTask(ctx, domain.SubmitTaskCommand{
		SessionKey: dev.SessionKey, RequesterUserID: 1,
		Source: "web", SourceInstanceID: "bot-blk", MessageID: "blk-1",
		Prompt: "blocked undispatched", PersonaSnapshot: []string{"p"}, ToolPolicyVersion: "foundation.no-host-tools.v1",
	}); err != nil {
		t.Fatal(err)
	}
	claimed, ok, err := store.ClaimNextTask(ctx, dev.SessionKey, "p1", time.Minute)
	if err != nil || !ok {
		t.Fatalf("claim: %v ok=%v", err, ok)
	}
	// 模拟 capability 已签发(签发先于 MarkDispatchStarted, 审查 F1)。
	if err := store.SetTaskCapabilityJTIs(ctx, claimed.ID, "p1", []string{"jti-blocked"}); err != nil {
		t.Fatal(err)
	}
	// queued 任务在封禁前提交, 同样必须被终态化。
	if _, err := store.SubmitTask(ctx, domain.SubmitTaskCommand{
		SessionKey: dev.SessionKey, RequesterUserID: 1,
		Source: "web", SourceInstanceID: "bot-blk", MessageID: "blk-2",
		Prompt: "blocked queued", PersonaSnapshot: []string{"p"}, ToolPolicyVersion: "foundation.no-host-tools.v1",
	}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.BlockUser(ctx, 1); err != nil {
		t.Fatalf("BlockUser: %v", err)
	}
	got, err := store.GetTask(ctx, claimed.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != domain.TaskCancelled {
		t.Fatalf("undispatched starting task status = %s after block, want cancelled", got.Status)
	}
	if got.ClaimOwner != "" || !got.ClaimLeaseUntil.IsZero() {
		t.Fatalf("claim must be cleared after block: owner=%q lease=%v", got.ClaimOwner, got.ClaimLeaseUntil)
	}
	if got.CancelRequestedAt != nil {
		t.Fatal("terminalized task must not carry cancel_requested_at")
	}
	// JTI 必须被撤销(终态事务内)。
	var revoked bool
	if err := pool.QueryRow(ctx, `
SELECT EXISTS (SELECT 1 FROM llm_capability_revocations WHERE jti_hash = sha256('jti-blocked'::bytea))
`).Scan(&revoked); err != nil {
		t.Fatal(err)
	}
	if !revoked {
		t.Fatal("capability JTI must be revoked in the block transaction")
	}
	// pending task_started delivery 必须取消。
	var stStatus string
	if err := pool.QueryRow(ctx, `SELECT status FROM task_deliveries WHERE task_id = $1 AND delivery_type = 'task_started'`, claimed.ID).Scan(&stStatus); err != nil {
		t.Fatal(err)
	}
	if stStatus != "cancelled" {
		t.Fatalf("task_started delivery status = %q after block, want cancelled", stStatus)
	}
	var qStatus string
	if err := pool.QueryRow(ctx, `SELECT status FROM tasks WHERE message_id = 'blk-2'`).Scan(&qStatus); err != nil {
		t.Fatal(err)
	}
	if qStatus != "cancelled" {
		t.Fatalf("queued task status = %q after block, want cancelled", qStatus)
	}
}

// round10 审查(B5): IsTaskCapabilityActive 必须对 blocked 用户的任务返回
// inactive——封禁后下一次 LLM/Sophub 调用即被拒绝, 不等 Worker 终态化。
func TestIsTaskCapabilityActiveRejectsBlockedUser(t *testing.T) {
	pool := requireDB(t)
	store, err := NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	dev := seedDev(t, store)

	task, err := store.SubmitTask(ctx, domain.SubmitTaskCommand{
		SessionKey: dev.SessionKey, RequesterUserID: 1,
		Source: "web", SourceInstanceID: "bot-blk2", MessageID: "blk-active",
		Prompt: "p", PersonaSnapshot: []string{"p"}, ToolPolicyVersion: "foundation.no-host-tools.v1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, err := store.ClaimNextTask(ctx, dev.SessionKey, "p1", time.Minute); err != nil || !ok {
		t.Fatalf("claim: %v ok=%v", err, ok)
	}
	if _, err := store.MarkDispatchStarted(ctx, task.ID, "p1", "w1", false); err != nil {
		t.Fatal(err)
	}
	// loopback/dev 模式无 runner_leases 行 → 跳过 lease 校验; 用户 approved → 活跃。
	if active, err := store.IsTaskCapabilityActive(ctx, task.ID, 1); err != nil || !active {
		t.Fatalf("approved user task must be active, active=%v err=%v", active, err)
	}
	// task1 已派发(starting)且用户被 BlockUser(已派发任务只写
	// cancel_requested_at, 状态仍 starting)——capability 在线校验必须因
	// users.status='blocked' 拒绝(封禁后下一次 LLM/Sophub 调用即失效)。
	if _, _, err := store.BlockUser(ctx, 1); err != nil {
		t.Fatal(err)
	}
	if active, err := store.IsTaskCapabilityActive(ctx, task.ID, 1); err != nil || active {
		t.Fatalf("blocked user task must be inactive, active=%v err=%v", active, err)
	}
}

// round10 审查(B8): 同一 task 的 task_started 未完成(pending/sending)时,
// 其他 delivery(task_complete/task_failed 等)不得被 claim——否则并发发送
// 会让完成消息先于"正在处理"送达。task_started 自身始终可 claim。
func TestClaimPendingDeliveriesDefersTerminalUntilStartedAcked(t *testing.T) {
	pool := requireDB(t)
	store, err := NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	dev := seedDev(t, store)

	task, err := store.SubmitTask(ctx, domain.SubmitTaskCommand{
		SessionKey: dev.SessionKey, RequesterUserID: 1,
		Source: "web", SourceInstanceID: "bot-ord", MessageID: "ord-1",
		Prompt: "ordering", PersonaSnapshot: []string{"p"}, ToolPolicyVersion: "foundation.no-host-tools.v1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, err := store.ClaimNextTask(ctx, dev.SessionKey, "p1", time.Minute); err != nil || !ok {
		t.Fatalf("claim task: %v ok=%v", err, ok)
	}
	// 终态事务插入 task_failed delivery 并取消 pending task_started。
	if _, err := store.CompleteFailedTerminal(ctx, task.ID, "p1", domain.TaskFailed, domain.DeliveryTaskFailed, "E", "m", ""); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	// task_started 已被终态事务置 cancelled → task_failed 可 claim。
	dels, err := store.ClaimPendingDeliveries(ctx, 10, time.Minute, 5*time.Minute, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(dels) != 1 || dels[0].DeliveryType != domain.DeliveryTaskFailed {
		t.Fatalf("terminal delivery must be claimable after task_started cancelled, got %+v", dels)
	}

	// 场景 B: 模拟 task_started 仍 pending(如发送失败重试中)——同 task 的
	// terminal delivery 不得被 claim; task_started 自身可 claim。
	if _, err := pool.Exec(ctx, `
UPDATE task_deliveries SET status = 'pending' WHERE task_id = $1 AND delivery_type = 'task_started'
`, task.ID); err != nil {
		t.Fatal(err)
	}
	// 重置 task_failed 为 pending 供 claim。
	if _, err := pool.Exec(ctx, `UPDATE task_deliveries SET status = 'pending', attempt_lease_until = NULL, next_attempt_at = NULL WHERE task_id = $1`, task.ID); err != nil {
		t.Fatal(err)
	}
	dels, err = store.ClaimPendingDeliveries(ctx, 10, time.Minute, 5*time.Minute, now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if len(dels) != 1 || dels[0].DeliveryType != domain.DeliveryTaskStarted {
		t.Fatalf("only task_started may be claimed while it is pending, got %+v", dels)
	}
}

// round10 审查(B7): 任务与入站消息行同事务——成功时原子写入; 消息行已存在
// (已处理过)时短路且任务不重复创建; 任务已存在(崩溃重试)时返回已有任务并
// 补齐消息行。
func TestSubmitTaskWithInboundMessageAtomicity(t *testing.T) {
	pool := requireDB(t)
	store, err := NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	dev := seedDev(t, store)
	// messages.bot_id 外键要求 channel_configs 行存在(0053 后 bots → channel_configs)。
	if _, err := pool.Exec(ctx, `
INSERT INTO channel_configs (id, bot_uuid, channel_type, owner_id, config_ciphertext, state, created_at, updated_at)
VALUES (1, '00000000-0000-4000-8000-000000000001', 'wechat', 1, 'ciphertext', 'active', timezone('utc', now()), timezone('utc', now()))
ON CONFLICT (id) DO NOTHING
`); err != nil {
		t.Fatal(err)
	}

	msg := domain.Message{
		UserID: 1, BotID: 1, SessionKey: dev.SessionKey, MessageID: "atomic-1",
		MessageType: domain.MessageTypeText, Content: "hello",
	}
	cmd := domain.SubmitTaskCommand{
		SessionKey: dev.SessionKey, RequesterUserID: 1,
		Source: "web", SourceInstanceID: "bot-atom", MessageID: "atomic-1",
		Prompt: "p", PersonaSnapshot: []string{"p"}, ToolPolicyVersion: "foundation.no-host-tools.v1",
	}
	task, msgRow, err := store.SubmitTaskWithInboundMessage(ctx, cmd, msg)
	if err != nil {
		t.Fatalf("atomic submit: %v", err)
	}
	if task.ID == "" || msgRow.ID == 0 {
		t.Fatalf("atomic submit must persist both task and message, task=%q msg=%d", task.ID, msgRow.ID)
	}

	// 消息行已存在 → ErrDuplicateInboundMessage, 且不创建第二个任务。
	if _, _, err := store.SubmitTaskWithInboundMessage(ctx, cmd, msg); !errors.Is(err, domain.ErrDuplicateInboundMessage) {
		t.Fatalf("duplicate message must short-circuit with ErrDuplicateInboundMessage, got %v", err)
	}
	var taskCount int
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM tasks WHERE message_id = 'atomic-1'`).Scan(&taskCount); err != nil {
		t.Fatal(err)
	}
	if taskCount != 1 {
		t.Fatalf("task count = %d, want 1 (no duplicate task)", taskCount)
	}

	// 模拟崩溃重试: 任务已存在但消息行缺失(删除消息行)——重试必须返回已有
	// 任务并补齐消息行, 不创建重复任务。
	if _, err := pool.Exec(ctx, `DELETE FROM messages WHERE id = $1`, msgRow.ID); err != nil {
		t.Fatal(err)
	}
	retryTask, retryRow, err := store.SubmitTaskWithInboundMessage(ctx, cmd, msg)
	if err != nil {
		t.Fatalf("retry after crash window: %v", err)
	}
	if retryTask.ID != task.ID {
		t.Fatalf("retry must return existing task %s, got %s", task.ID, retryTask.ID)
	}
	if retryRow.ID == 0 {
		t.Fatal("retry must backfill the inbound message row")
	}
}

// 审查 D3(用户生命周期): BlockUser 必须撤销该用户全部登录会话——
// 否则 blocked 用户可持旧 token 继续调用用户控制面 API。
func TestBlockUserRevokesAllSessions(t *testing.T) {
	pool := requireDB(t)
	store, err := NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	dev := seedDev(t, store)

	if _, err := store.CreateUserSession(ctx, "tok-hash-1", dev.UserID, time.Now().UTC().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateUserSession(ctx, "tok-hash-2", dev.UserID, time.Now().UTC().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	other, err := store.EnsureAdminContext(ctx, 2, "other-user")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateUserSession(ctx, "tok-hash-3", other.UserID, time.Now().UTC().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.BlockUser(ctx, dev.UserID); err != nil {
		t.Fatalf("BlockUser: %v", err)
	}
	if _, err := store.GetUserSession(ctx, "tok-hash-1"); err == nil {
		t.Fatal("session 1 must be revoked after block")
	}
	if _, err := store.GetUserSession(ctx, "tok-hash-2"); err == nil {
		t.Fatal("session 2 must be revoked after block")
	}
	// 其他用户会话不受影响。
	if _, err := store.GetUserSession(ctx, "tok-hash-3"); err != nil {
		t.Fatalf("other user session must survive block: %v", err)
	}
}

// 审查 F2: delivery attempt fencing——旧 attempt 的 ack 携带过期 token 时
// 不得影响新 attempt 的状态。
func TestDeliveryAttemptTokenFencesStaleWrites(t *testing.T) {
	pool := requireDB(t)
	store, err := NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	dev := seedDev(t, store)

	task, err := store.SubmitTask(ctx, domain.SubmitTaskCommand{
		SessionKey: dev.SessionKey, RequesterUserID: 1,
		Source: "web", SourceInstanceID: "bot-f2", MessageID: "f2-1",
		Prompt: "delivery fencing", PersonaSnapshot: []string{"p"}, ToolPolicyVersion: "foundation.no-host-tools.v1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, err := store.ClaimNextTask(ctx, dev.SessionKey, "p1", time.Minute); err != nil || !ok {
		t.Fatalf("claim task: %v ok=%v", err, ok)
	}
	if _, err := store.CompleteFailedTerminal(ctx, task.ID, "p1", domain.TaskFailed, domain.DeliveryTaskFailed, "E", "m", ""); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	claimed, err := store.ClaimPendingDeliveries(ctx, 10, time.Minute, 5*time.Minute, now)
	if err != nil {
		t.Fatal(err)
	}
	var first *domain.Delivery
	for i := range claimed {
		if claimed[i].DeliveryType == domain.DeliveryTaskFailed {
			first = &claimed[i]
			break
		}
	}
	if first == nil {
		t.Fatalf("task_failed delivery must be claimable, got %+v", claimed)
	}
	// 模拟超时: 重置回 pending, 新 attempt 重新 claim。
	if _, err := store.ResetStaleSendingDeliveries(ctx, now.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	claimed2, err := store.ClaimPendingDeliveries(ctx, 10, time.Minute, 5*time.Minute, now.Add(2*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	var second *domain.Delivery
	for i := range claimed2 {
		if claimed2[i].DeliveryID == first.DeliveryID {
			second = &claimed2[i]
			break
		}
	}
	if second == nil {
		t.Fatalf("delivery %s not re-claimed", first.DeliveryID)
	}
	if second.AttemptToken == "" || second.AttemptToken == first.AttemptToken {
		t.Fatalf("attempt token must rotate: first=%q second=%q", first.AttemptToken, second.AttemptToken)
	}
	// 旧 attempt 用过期 token ack——必须 no-op(状态保持 sending)。
	if err := store.MarkDeliveryAcked(ctx, first.DeliveryID, first.AttemptToken, now.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	d, err := store.GetDelivery(ctx, second.TaskID, second.DeliveryType)
	if err != nil {
		t.Fatal(err)
	}
	if d.Status != domain.DeliverySending {
		t.Fatalf("stale ack must be fenced, status=%s", d.Status)
	}
	// 新 attempt 用正确 token ack——生效。
	if err := store.MarkDeliveryAcked(ctx, second.DeliveryID, second.AttemptToken, now.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	d, err = store.GetDelivery(ctx, second.TaskID, second.DeliveryType)
	if err != nil {
		t.Fatal(err)
	}
	if d.Status != domain.DeliveryAcked {
		t.Fatalf("fresh ack must apply, status=%s", d.Status)
	}
}

// 审查 D4: 全局 running-task 上限必须是跨实例原子门禁——两个并发 claim
// (不同 session)同时观察到 limit-1 时, 只有一个能成功, 不会超卖。
func TestRunningTaskLimitSerializesConcurrentClaims(t *testing.T) {
	pool := requireDB(t)
	store, err := NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	store.SetRunningTaskLimit(1)
	ctx := context.Background()
	dev := seedDev(t, store)

	// 第二个 workspace(不同 session key, 可独立 claim)。
	if _, err := pool.Exec(ctx, `
INSERT INTO workspaces (id, session_key, owner_user_id, kind, team_id, volume_id, bootstrap_marker)
VALUES ('00000000-0000-4000-8000-0000000000d4', 'personal:d4', 1, 'personal', NULL, 'vol-test-d4', NULL)
ON CONFLICT (session_key) DO NOTHING;
`); err != nil {
		t.Fatal(err)
	}
	for i, sk := range []string{dev.SessionKey, "personal:d4"} {
		if _, err := store.SubmitTask(ctx, domain.SubmitTaskCommand{
			SessionKey: sk, RequesterUserID: 1, Source: "web", SourceInstanceID: "d4",
			MessageID: fmt.Sprintf("d4-%d", i), Prompt: "p", PersonaSnapshot: []string{},
			ToolPolicyVersion: "foundation.no-host-tools.v1",
		}); err != nil {
			t.Fatal(err)
		}
	}
	// 并发 claim 两个不同 session: limit=1 时最多一个成功。
	var wg sync.WaitGroup
	okCount := make([]bool, 2)
	for i, sk := range []string{dev.SessionKey, "personal:d4"} {
		wg.Add(1)
		go func(i int, sk string) {
			defer wg.Done()
			_, ok, err := store.ClaimNextTask(ctx, sk, "p-d4", time.Minute)
			if err != nil {
				t.Errorf("claim %s: %v", sk, err)
				return
			}
			okCount[i] = ok
		}(i, sk)
	}
	wg.Wait()
	n := 0
	for _, ok := range okCount {
		if ok {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("exactly one claim must win with limit=1, got %d (ok=%v)", n, okCount)
	}
}

// 审查(platform-review-fixes): CreateUser 空密码(NULL password_hash)必须
// 正常返回, 不能因 scanUser 扫 NULL 进 string 崩溃。
func TestCreateUserEmptyPasswordHash(t *testing.T) {
	pool := requireDB(t)
	store, err := NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	u, err := store.CreateUser(ctx, "empty-pw-user", "")
	if err != nil {
		t.Fatalf("CreateUser with empty password must succeed: %v", err)
	}
	if u.ID <= 0 || u.PasswordHash != "" || u.Status != domain.UserPending {
		t.Fatalf("unexpected user: %+v", u)
	}
}

// TestDeliveryAdminRequeueDeadLetter 验证 admin 死信查询/重投(2026-08-14
// 审查 E2): 死信行可重投为 pending(attempt 预算归零), 非死信行拒绝。
func TestDeliveryAdminRequeueDeadLetter(t *testing.T) {
	pool := requireDB(t)
	store, err := NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	dev := seedDev(t, store)

	task, err := store.SubmitTask(ctx, domain.SubmitTaskCommand{
		SessionKey: dev.SessionKey, RequesterUserID: 1,
		Source: "web", SourceInstanceID: "bot-ord", MessageID: "ord-1",
		Prompt: "ordering", PersonaSnapshot: []string{"p"}, ToolPolicyVersion: "foundation.no-host-tools.v1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, err := store.ClaimNextTask(ctx, dev.SessionKey, "p1", time.Minute); err != nil || !ok {
		t.Fatalf("claim task: %v ok=%v", err, ok)
	}
	if _, err := store.CompleteFailedTerminal(ctx, task.ID, "p1", domain.TaskFailed, domain.DeliveryTaskFailed, "E", "m", ""); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	dels, err := store.ClaimPendingDeliveries(ctx, 10, time.Minute, 5*time.Minute, now)
	if err != nil || len(dels) != 1 {
		t.Fatalf("claim terminal delivery: %v n=%d", err, len(dels))
	}
	d := dels[0]
	if err := store.MarkDeliveryDeadLetter(ctx, d.DeliveryID, d.AttemptToken, "SEND_FAILED", "boom", now); err != nil {
		t.Fatal(err)
	}

	// 列表: 死信可见。
	rows, err := store.ListDeliveries(ctx, "dead_letter", 10)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, r := range rows {
		if r.DeliveryID == d.DeliveryID {
			found = true
			if r.Status != "dead_letter" || r.ErrorCode != "SEND_FAILED" || r.AttemptCount < 1 {
				t.Fatalf("dead letter row mismatch: %+v", r)
			}
		}
	}
	if !found {
		t.Fatalf("dead letter delivery %s not listed", d.DeliveryID)
	}

	// 重投: 死信行 → pending, attempt_count 归零。
	requeued, err := store.RequeueDeadLetterDelivery(ctx, d.DeliveryID, now.Add(time.Minute))
	if err != nil || !requeued {
		t.Fatalf("requeue: %v ok=%v", err, requeued)
	}
	var status string
	var attempts int
	if err := pool.QueryRow(ctx, `SELECT status, attempt_count FROM task_deliveries WHERE delivery_id = $1`, d.DeliveryID).Scan(&status, &attempts); err != nil {
		t.Fatal(err)
	}
	if status != "pending" || attempts != 0 {
		t.Fatalf("requeued row: status=%s attempts=%d, want pending/0", status, attempts)
	}

	// 再次重投(pending 行): 拒绝(fail-closed)。
	requeued, err = store.RequeueDeadLetterDelivery(ctx, d.DeliveryID, now.Add(2*time.Minute))
	if err != nil || requeued {
		t.Fatalf("requeue of pending row must be refused: %v ok=%v", err, requeued)
	}

	// 不存在 id: found=false。
	requeued, err = store.RequeueDeadLetterDelivery(ctx, "no-such-delivery", now.Add(2*time.Minute))
	if err != nil || requeued {
		t.Fatalf("requeue of unknown row must be refused: %v ok=%v", err, requeued)
	}
}

// TestDeliveryRequeueSurvivesExpiredRetryWindow 验证 P1 修复(2026-08-14
// 子代理复审): 事故后数小时(窗口已过)重投死信, 行必须仍可被 claim, 而不是
// 被 DeadLetterExpiredDeliveries 原样打回——requeued_at 开启新 30min 窗口,
// 窗口锚点 = GREATEST(tasks.terminal_at, requeued_at)。
func TestDeliveryRequeueSurvivesExpiredRetryWindow(t *testing.T) {
	pool := requireDB(t)
	store, err := NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	dev := seedDev(t, store)

	task, err := store.SubmitTask(ctx, domain.SubmitTaskCommand{
		SessionKey: dev.SessionKey, RequesterUserID: 1,
		Source: "web", SourceInstanceID: "bot-ord", MessageID: "ord-1",
		Prompt: "ordering", PersonaSnapshot: []string{"p"}, ToolPolicyVersion: "foundation.no-host-tools.v1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, err := store.ClaimNextTask(ctx, dev.SessionKey, "p1", time.Minute); err != nil || !ok {
		t.Fatalf("claim task: %v ok=%v", err, ok)
	}
	if _, err := store.CompleteFailedTerminal(ctx, task.ID, "p1", domain.TaskFailed, domain.DeliveryTaskFailed, "E", "m", ""); err != nil {
		t.Fatal(err)
	}
	// 任务完成于 2 小时前(模拟事故后数小时才发现死信, 30min 窗口早过)。
	old := time.Now().UTC().Add(-2 * time.Hour)
	if _, err := pool.Exec(ctx, `UPDATE tasks SET terminal_at = $1 WHERE id = $2`, old, task.ID); err != nil {
		t.Fatal(err)
	}
	dels, err := store.ClaimPendingDeliveries(ctx, 10, time.Minute, 30*time.Minute, old)
	if err != nil || len(dels) != 1 {
		t.Fatalf("claim terminal delivery: %v n=%d", err, len(dels))
	}
	d := dels[0]
	if err := store.MarkDeliveryDeadLetter(ctx, d.DeliveryID, d.AttemptToken, "SEND_FAILED", "boom", old); err != nil {
		t.Fatal(err)
	}

	now := time.Now().UTC()
	requeued, err := store.RequeueDeadLetterDelivery(ctx, d.DeliveryID, now)
	if err != nil || !requeued {
		t.Fatalf("requeue: %v ok=%v", err, requeued)
	}

	// 重投后窗口已刷新: 死信清扫不得打回(旧实现会在这里原样打回)。
	if n, err := store.DeadLetterExpiredDeliveries(ctx, 30*time.Minute, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	} else if n != 0 {
		t.Fatalf("requeued delivery must survive expired-window sweep, got %d dead-lettered", n)
	}

	// 且可被 claim(窗口内)。
	dels, err = store.ClaimPendingDeliveries(ctx, 10, time.Minute, 30*time.Minute, now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if len(dels) != 1 || dels[0].DeliveryID != d.DeliveryID {
		t.Fatalf("requeued delivery must be claimable after window refresh, got %+v", dels)
	}
	if dels[0].RequeuedAt == nil || dels[0].RequeuedAt.Sub(now) > time.Second || now.Sub(*dels[0].RequeuedAt) > time.Second {
		t.Fatalf("requeued_at must be ~now on claim, got %v (want ~%v)", dels[0].RequeuedAt, now)
	}
}
