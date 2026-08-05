package application

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/domain"
)

// ---------------------------------------------------------------------------
// round11 审查 I2: CancelTask RPC 瞬时失败后不得被 sync.Once 永久缓存——
// cancel_requested_at 已持久, 后续 tick 的 maybeCancelWorker 必须能重试。
// 并发取消仍合并为单次 RPC。
// ---------------------------------------------------------------------------

func newCancelHarness(worker *controlledWorker) *scheduler {
	entry := &workerEntry{
		client: worker, sessionKey: "personal:1",
		started: true, credentials: workerCredentialSet{},
	}
	entry.executing.Store(true)
	return &scheduler{
		cfg:     SchedulerConfig{},
		workers: map[string]*workerEntry{"personal:1": entry},
	}
}

func cancelTask() domain.Task {
	return domain.Task{ID: "task-cancel-1", SessionKey: "personal:1"}
}

// TestCancelWorkerRetriesAfterTransientFailure verifies a failed cancel RPC is
// not permanently cached: the next CancelWorker call issues a fresh RPC.
func TestCancelWorkerRetriesAfterTransientFailure(t *testing.T) {
	worker := newControlledWorker()
	worker.cancelTaskErr = errors.New("transient network failure")
	s := newCancelHarness(worker)

	err := s.CancelWorker(context.Background(), cancelTask())
	if err == nil {
		t.Fatal("expected transient failure")
	}
	if got := worker.cancelCalls.Load(); got != 1 {
		t.Fatalf("cancel calls=%d want 1", got)
	}

	// 失败未被缓存: 第二次调用必须重试。
	worker.cancelTaskErr = nil
	if err := s.CancelWorker(context.Background(), cancelTask()); err != nil {
		t.Fatalf("retry after transient failure failed: %v", err)
	}
	if got := worker.cancelCalls.Load(); got != 2 {
		t.Fatalf("cancel calls=%d want 2 (must retry after failure)", got)
	}

	// 成功后缓存: 第三次不再发 RPC。
	if err := s.CancelWorker(context.Background(), cancelTask()); err != nil {
		t.Fatal(err)
	}
	if got := worker.cancelCalls.Load(); got != 2 {
		t.Fatalf("cancel calls=%d want 2 (success must be cached)", got)
	}
}

// TestCancelWorkerMergesConcurrentCalls verifies that concurrent cancel
// requests for the same task share one in-flight RPC (no storm).
func TestCancelWorkerMergesConcurrentCalls(t *testing.T) {
	worker := newControlledWorker()
	worker.cancelTaskEntered = make(chan struct{})
	worker.releaseCancelTask = make(chan struct{})
	s := newCancelHarness(worker)

	first := make(chan error, 1)
	go func() { first <- s.CancelWorker(context.Background(), cancelTask()) }()
	select {
	case <-worker.cancelTaskEntered:
	case <-time.After(2 * time.Second):
		t.Fatal("cancel RPC did not start")
	}

	second := make(chan error, 1)
	go func() { second <- s.CancelWorker(context.Background(), cancelTask()) }()
	select {
	case err := <-second:
		t.Fatalf("concurrent cancel must wait for the in-flight RPC, returned early: %v", err)
	case <-time.After(200 * time.Millisecond):
		// expected: merged
	}

	close(worker.releaseCancelTask)
	if err := <-first; err != nil {
		t.Fatalf("first cancel failed: %v", err)
	}
	select {
	case err := <-second:
		if err != nil {
			t.Fatalf("merged cancel failed: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("merged cancel did not complete")
	}
	if got := worker.cancelCalls.Load(); got != 1 {
		t.Fatalf("cancel RPCs=%d want 1 (concurrent calls must merge)", got)
	}
}

// TestCancelWorkerSkipsRPCWhenTaskNotExecuting preserves the round11 C3
// contract: no RPC when the worker is not executing the task (dispatch will
// handle the durable cancel request itself).
func TestCancelWorkerSkipsRPCWhenTaskNotExecuting(t *testing.T) {
	worker := newControlledWorker()
	entry := &workerEntry{client: worker, sessionKey: "personal:1", started: true}
	s := &scheduler{
		cfg:     SchedulerConfig{},
		workers: map[string]*workerEntry{"personal:1": entry},
	}
	if err := s.CancelWorker(context.Background(), cancelTask()); err != nil {
		t.Fatalf("cancel before execution must succeed silently: %v", err)
	}
	if got := worker.cancelCalls.Load(); got != 0 {
		t.Fatalf("cancel RPCs=%d want 0 before execution", got)
	}
}
