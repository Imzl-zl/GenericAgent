package application

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/infrastructure/postgres"
)

// Round12 审查 I3: dispatchHeartbeat.Stop 必须可取消——先 cancel 再等待
// goroutine 退出(带超时), 否则卡在 DB 调用的 heartbeat 会让任务收尾/
// Platform 关闭无限阻塞。

// blockHeartbeatStore 的 HeartbeatClaim 在 block 置位后阻塞到 ctx 取消
// (模拟 DB 半开)。
type blockHeartbeatStore struct {
	*postgres.Store
	block atomic.Bool
}

func (s *blockHeartbeatStore) HeartbeatClaim(ctx context.Context, taskID, platformInstanceID string, claimLease time.Duration) error {
	if s.block.Load() {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(10 * time.Second):
			return context.DeadlineExceeded
		}
	}
	return s.Store.HeartbeatClaim(ctx, taskID, platformInstanceID, claimLease)
}

func TestDispatchHeartbeatStopReturnsWhenHeartbeatBlocked(t *testing.T) {
	_, store, _, dev := serviceFixture(t)
	blocking := &blockHeartbeatStore{Store: store}
	sched := newLeakTestScheduler(t, blocking, &cleanWorkerCleanup{worker: newControlledWorker()}, "hb-stop", nil)
	claimed := claimedTaskFor(t, store, dev.SessionKey, "foundation.no-host-tools.v1", "hb-stop")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	heartbeat, err := sched.startDispatchHeartbeat(ctx, claimed)
	if err != nil {
		t.Fatal(err)
	}
	// 下一轮心跳进入阻塞调用。
	blocking.block.Store(true)
	// 等待 goroutine 至少进入一次 ticker 循环(interval = ClaimLease/3 ≈ 333ms)。
	time.Sleep(700 * time.Millisecond)

	stopDone := make(chan error, 1)
	go func() { stopDone <- heartbeat.Stop() }()
	select {
	case <-stopDone:
		// Stop 返回即通过: 修复前先 <-done 后 cancel, 阻塞的 heartbeat
		// 永不退出, Stop 永不返回。
	case <-time.After(3 * time.Second):
		t.Fatal("heartbeat.Stop blocked forever while heartbeat call stuck")
	}
}
