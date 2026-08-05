package application

import (
	"context"
	"testing"
	"time"
)

// round12 审查(M1): cancelOnce 条目随 Worker 销毁清理——否则按取消过的
// 任务数无界增长(Platform 长期运行内存泄漏)。

func TestCancelOnceEntryClearedOnWorkerDestroy(t *testing.T) {
	_, store, _, dev := serviceFixture(t)
	cleanup := &cleanWorkerCleanup{worker: newControlledWorker()}
	sched := newLeakTestScheduler(t, store, cleanup, "cancel-clear", nil)
	claimed := claimedTaskFor(t, store, dev.SessionKey, "foundation.no-host-tools.v1", "cancel-clear")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	// 模拟一次取消调用(entry 尚不存在时 CancelWorker 幂等成功, 但会创建条目)。
	if err := sched.CancelWorker(ctx, claimed); err != nil {
		t.Fatal(err)
	}
	if _, ok := sched.cancelOnce.Load(claimed.ID); !ok {
		t.Fatal("cancelOnce entry must exist after CancelWorker")
	}
	// 派发并销毁 Worker(任务终态路径)。
	cleanup.worker.succeed()
	_ = sched.dispatch(ctx, claimed)
	if _, ok := sched.cancelOnce.Load(claimed.ID); ok {
		t.Fatal("cancelOnce entry must be cleared after worker destroy")
	}
}
