package application

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/domain"
	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/infrastructure/postgres"
)

// round13 根本性收拢: "任务终态 ⇔ 全部资源恰好释放一次"必须由结构强制——
// terminateTask 单一收尾 API(destroy 先于 finalize) + workerEntry destroyed
// 状态 + 泄漏不变量测试。以下测试先于实现(RED)。

// destroyBeforeFinalizeStore 在 finalize 时断言 worker 已销毁。
type destroyBeforeFinalizeStore struct {
	*postgres.Store
	cleaned              *atomic.Bool
	finalizedAfterCleanup atomic.Bool
}

func (s *destroyBeforeFinalizeStore) CompleteFailedTerminal(ctx context.Context, taskID, owner string, status domain.TaskStatus, deliveryType domain.DeliveryType, code, message, traceID string) (domain.Task, error) {
	if s.cleaned.Load() {
		s.finalizedAfterCleanup.Store(true)
	}
	return s.Store.CompleteFailedTerminal(ctx, taskID, owner, status, deliveryType, code, message, traceID)
}

// TestTerminateTaskDestroysBeforeFinalize: 收尾顺序必须 destroy 先于
// finalize——终态提交前资源已释放, 替换窗口从结构上不可能出现。
func TestTerminateTaskDestroysBeforeFinalize(t *testing.T) {
	_, store, _, dev := serviceFixture(t)
	cleanup := &cleanWorkerCleanup{worker: newControlledWorker()}
	orderStore := &destroyBeforeFinalizeStore{Store: store, cleaned: &cleanup.cleaned}
	sched := newLeakTestScheduler(t, orderStore, cleanup, "term-order", nil)
	claimed := claimedTaskFor(t, store, dev.SessionKey, "foundation.no-host-tools.v1", "term-order")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	// 直接调用收尾 API: worker 已被 createTaskWorker 建立(模拟 dispatch 后段)。
	if _, _, err := sched.createTaskWorker(ctx, claimed); err != nil {
		t.Fatal(err)
	}
	sched.terminateTask(ctx, claimed, domain.TaskFailed, domain.DeliveryTaskFailed,
		"TERMINATED", "terminated", "")
	if !orderStore.finalizedAfterCleanup.Load() {
		t.Fatal("finalize must observe worker already destroyed (destroy-before-finalize order)")
	}
	final, err := store.GetTask(ctx, claimed.ID)
	if err != nil {
		t.Fatal(err)
	}
	if final.TerminalErrorCode != "TERMINATED" {
		t.Fatalf("task code=%s want TERMINATED", final.TerminalErrorCode)
	}
	assertNoWorkerLeaks(t, sched)
}

// TestTerminateTaskRegistersPendingRetry: 收尾 API 的 finalize 失败必须走
// pendingFinalize 重试(与既有 I2 语义一致)。
func TestTerminateTaskRegistersPendingRetry(t *testing.T) {
	_, store, _, dev := serviceFixture(t)
	cleanup := &cleanWorkerCleanup{worker: newControlledWorker()}
	flaky := &flakyTerminalStore{Store: store}
	flaky.fail.Store(true)
	sched := newLeakTestScheduler(t, flaky, cleanup, "term-retry", nil)
	claimed := claimedTaskFor(t, store, dev.SessionKey, "foundation.no-host-tools.v1", "term-retry")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, _, err := sched.createTaskWorker(ctx, claimed); err != nil {
		t.Fatal(err)
	}
	sched.terminateTask(ctx, claimed, domain.TaskFailed, domain.DeliveryTaskFailed,
		"TERMINATED", "terminated", "")
	if _, ok := sched.pendingFinalize.Load(claimed.ID); !ok {
		t.Fatal("finalize intent must be registered when terminateTask finalize fails")
	}
	if !cleanup.cleaned.Load() {
		t.Fatal("worker must be destroyed even when finalize fails")
	}
	flaky.fail.Store(false)
	sched.drainPendingFinalize(ctx)
	if _, ok := sched.pendingFinalize.Load(claimed.ID); ok {
		t.Fatal("finalize intent not cleared after retry")
	}
	final, err := store.GetTask(ctx, claimed.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !final.Status.IsTerminal() {
		t.Fatalf("task status=%s want terminal after retry", final.Status)
	}
}

// TestWorkerEntryDestroyedIsIdempotent: destroyed 状态保证同一 entry 恰好
// 清理一次(并发/重复销毁路径)。
func TestWorkerEntryDestroyedIsIdempotent(t *testing.T) {
	_, store, _, dev := serviceFixture(t)
	cleanup := &cleanWorkerCleanup{worker: newControlledWorker()}
	sched := newLeakTestScheduler(t, store, cleanup, "term-idem", nil)

	entry := &workerEntry{sessionKey: dev.SessionKey, taskID: "task-idem"}
	var calls atomic.Int32
	entry.cleanup = func(_ string) { calls.Add(1) }
	sched.mu.Lock()
	sched.workers[dev.SessionKey] = entry
	sched.mu.Unlock()

	sched.destroyTaskWorkerEntry(dev.SessionKey, entry)
	sched.destroyTaskWorkerEntry(dev.SessionKey, entry)
	sched.destroyTaskWorker(dev.SessionKey)
	if calls.Load() != 1 {
		t.Fatalf("cleanup calls=%d, want exactly 1 (destroyed state must be idempotent)", calls.Load())
	}
	assertNoWorkerLeaks(t, sched)
}

// TestCancelWorkerSkipsDestroyedEntry: 销毁后的 entry 不再向容器发取消 RPC。
func TestCancelWorkerSkipsDestroyedEntry(t *testing.T) {
	_, store, _, dev := serviceFixture(t)
	cleanup := &cleanWorkerCleanup{worker: newControlledWorker()}
	sched := newLeakTestScheduler(t, store, cleanup, "term-cancel", nil)

	entry := &workerEntry{sessionKey: dev.SessionKey, taskID: "task-cancel"}
	entry.client = cleanup.worker
	entry.executing.Store(true)
	sched.mu.Lock()
	sched.workers[dev.SessionKey] = entry
	sched.mu.Unlock()

	sched.destroyTaskWorker(dev.SessionKey)
	if err := sched.CancelWorker(context.Background(), domain.Task{ID: "task-cancel", SessionKey: dev.SessionKey}); err != nil {
		t.Fatal(err)
	}
	if got := cleanup.worker.cancelCalls.Load(); got != 0 {
		t.Fatalf("cancel RPC calls=%d, want 0 for destroyed entry", got)
	}
}

// assertNoWorkerLeaks 是"任务终态即销毁"不变量的测试强制(round13 L3):
// 任何 dispatch/收尾测试结束后调用, 残留 entry 立即红。
func assertNoWorkerLeaks(t *testing.T, sched *scheduler) {
	t.Helper()
	sched.mu.Lock()
	n := len(sched.workers)
	sched.mu.Unlock()
	if n != 0 {
		t.Fatalf("%d worker entries leaked: task-terminal ⇒ destroy invariant broken", n)
	}
}
