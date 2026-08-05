package application

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/domain"
	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/infrastructure/postgres"
)

// Round12 审查 I2: 终态化写库失败时任务不得永久卡 starting——dispatch 退出
// 后意图进入 pendingFinalize, 由 tick 每轮重试, claim 由 tick heartbeat 续租
// 保持有效; 进程崩溃时由 claim 过期 + RecoverAfterRestart 兜底。

// flakyTerminalStore 的 CompleteFailedTerminal 在 fail 置位时返回瞬时错误
// (模拟 DB 不可用), 清除后恢复正常。
type flakyTerminalStore struct {
	*postgres.Store
	fail atomic.Bool
}

func (s *flakyTerminalStore) CompleteFailedTerminal(ctx context.Context, taskID, owner string, status domain.TaskStatus, deliveryType domain.DeliveryType, code, message, traceID string) (domain.Task, error) {
	if s.fail.Load() {
		return domain.Task{}, errors.New("database unavailable")
	}
	return s.Store.CompleteFailedTerminal(ctx, taskID, owner, status, deliveryType, code, message, traceID)
}

func TestFinalizeFailureRegistersPendingRetryAndDrainCompletes(t *testing.T) {
	_, store, _, dev := serviceFixture(t)
	cleanup := &cleanWorkerCleanup{worker: newControlledWorker()}
	flaky := &flakyTerminalStore{Store: store}
	flaky.fail.Store(true)
	sched := newLeakTestScheduler(t, flaky, cleanup, "pending-finalize", nil)
	claimed := claimedTaskFor(t, store, dev.SessionKey, "no-such-policy.v1", "pending-finalize")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := sched.dispatch(ctx, claimed); err == nil {
		t.Fatal("expected policy resolve error")
	}
	// 终态化失败: 任务未终态, 意图已注册。
	final, err := store.GetTask(ctx, claimed.ID)
	if err != nil {
		t.Fatal(err)
	}
	if final.Status.IsTerminal() {
		t.Fatalf("task terminalized despite DB failure (status=%s)", final.Status)
	}
	if _, ok := sched.pendingFinalize.Load(claimed.ID); !ok {
		t.Fatal("finalize intent not registered in pendingFinalize")
	}

	// DB 恢复: drain 重试成功, 任务终态化, 意图清除。
	flaky.fail.Store(false)
	sched.drainPendingFinalize(ctx)
	if _, ok := sched.pendingFinalize.Load(claimed.ID); ok {
		t.Fatal("finalize intent not cleared after successful retry")
	}
	final, err = store.GetTask(ctx, claimed.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !final.Status.IsTerminal() || final.TerminalErrorCode != "POLICY_RESOLVE_FAILED" {
		t.Fatalf("task status=%s code=%s want terminal POLICY_RESOLVE_FAILED", final.Status, final.TerminalErrorCode)
	}
}

func TestFinalizeRetryDropsIntentWhenTaskAlreadyTerminal(t *testing.T) {
	_, store, _, dev := serviceFixture(t)
	cleanup := &cleanWorkerCleanup{worker: newControlledWorker()}
	flaky := &flakyTerminalStore{Store: store}
	flaky.fail.Store(true)
	sched := newLeakTestScheduler(t, flaky, cleanup, "pending-other", nil)
	claimed := claimedTaskFor(t, store, dev.SessionKey, "no-such-policy.v1", "pending-other")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := sched.dispatch(ctx, claimed); err == nil {
		t.Fatal("expected policy resolve error")
	}
	if _, ok := sched.pendingFinalize.Load(claimed.ID); !ok {
		t.Fatal("finalize intent not registered")
	}
	// 另一路径终态化了任务(如恢复流程): drain 检测到已终态即清除意图,
	// 不再重试覆盖。
	flaky.fail.Store(false)
	if _, err := store.CompleteFailedTerminal(ctx, claimed.ID, "pending-other",
		domain.TaskFailed, domain.DeliveryTaskFailed, "OTHER_PATH", "terminalized elsewhere", ""); err != nil {
		t.Fatal(err)
	}
	sched.drainPendingFinalize(ctx)
	if _, ok := sched.pendingFinalize.Load(claimed.ID); ok {
		t.Fatal("finalize intent not dropped for already-terminal task")
	}
	final, err := store.GetTask(ctx, claimed.ID)
	if err != nil {
		t.Fatal(err)
	}
	if final.TerminalErrorCode != "OTHER_PATH" {
		t.Fatalf("task code=%s want OTHER_PATH (not overwritten)", final.TerminalErrorCode)
	}
}

func TestFinalizeRetryDropsIntentWhenClaimLost(t *testing.T) {
	_, store, _, dev := serviceFixture(t)
	cleanup := &cleanWorkerCleanup{worker: newControlledWorker()}
	flaky := &flakyTerminalStore{Store: store}
	flaky.fail.Store(true)
	sched := newLeakTestScheduler(t, flaky, cleanup, "pending-lost", nil)
	claimed := claimedTaskFor(t, store, dev.SessionKey, "no-such-policy.v1", "pending-lost")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := sched.dispatch(ctx, claimed); err == nil {
		t.Fatal("expected policy resolve error")
	}
	// 把 claim 移交给"另一个实例"(直接改 owner 模拟接管): drain 应删除
	// 意图, 由新 owner 负责终态化。
	flaky.fail.Store(false)
	if _, err := store.CompleteFailedTerminal(ctx, claimed.ID, "other-owner",
		domain.TaskFailed, domain.DeliveryTaskFailed, "TAKEN_OVER", "taken over", ""); err == nil {
		t.Fatal("expected ErrTaskNotOwned for foreign owner")
	}
	sched.drainPendingFinalize(ctx)
	if _, ok := sched.pendingFinalize.Load(claimed.ID); ok {
		t.Fatal("finalize intent not dropped after claim lost")
	}
}
