package application

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/domain"
)

// cancelCall serializes a single cancel RPC per task so concurrent cancel
// requests (e.g. /stop + scheduler tick) don't fire multiple Worker calls.
type cancelCall struct {
	once sync.Once
	err  error
}

// dispatchHeartbeat drives the claim lease heartbeat for one in-flight dispatch.
// The heartbeat extends claim_lease_until on an interval; if the lease is lost
// (ErrLeaseExpired) or the dispatch ctx is cancelled, the heartbeat stops and
// the dispatch goroutine observes the error via Stop().
// requeued 标记(审查 R5-I1): dispatch 因 Runner 容量满/foreign-owner 把任务
// 退回 queued 后, claim 已清空——此时 heartbeat 的 0 rows 是预期结果而非
// lease 丢失。标记后 ticker 的 ErrLeaseExpired 静默退出, 且 Stop() 不再把
// 错误上报给 deferred fallback(避免把已 requeue 的任务终态化)。
type dispatchHeartbeat struct {
	ctx      context.Context
	cancel   context.CancelFunc
	stop     chan struct{}
	done     chan struct{}
	stopOnce sync.Once
	mu       sync.Mutex
	err      error
	requeued atomic.Bool
}

// markRequeued 记录任务已被退回 queued(容量满/所有权瞬时错误路径)。
// 必须在 RequeueTask 提交之前调用, 使 ticker 与 Stop fallback 都看到标记。
func (h *dispatchHeartbeat) markRequeued() {
	h.requeued.Store(true)
}

func (h *dispatchHeartbeat) setError(err error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.err == nil {
		h.err = err
	}
}

func (h *dispatchHeartbeat) Stop() error {
	h.stopOnce.Do(func() { close(h.stop) })
	<-h.done
	h.cancel()
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.err
}

// startDispatchHeartbeat issues an immediate heartbeat (to verify the claim is
// still owned) then spawns a goroutine that extends the lease on an interval.
// The interval is ClaimLease/3 so the lease is refreshed well before expiry.
// If the immediate heartbeat fails, no goroutine is started and the error is
// returned so dispatch can finalize the task before attempting Worker RPC.
func (s *scheduler) startDispatchHeartbeat(parent context.Context, task domain.Task) (*dispatchHeartbeat, error) {
	ctx, cancel := context.WithCancel(parent)
	if err := s.cfg.Store.HeartbeatClaim(ctx, task.ID, s.cfg.PlatformInstanceID, s.cfg.ClaimLease); err != nil {
		cancel()
		return nil, err
	}
	heartbeat := &dispatchHeartbeat{
		ctx: ctx, cancel: cancel, stop: make(chan struct{}), done: make(chan struct{}),
	}
	interval := s.cfg.ClaimLease / 3
	if interval <= 0 {
		interval = s.cfg.ClaimLease
	}
	go func() {
		defer close(heartbeat.done)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		heartbeatOnce := func() error {
			return s.cfg.Store.HeartbeatClaim(ctx, task.ID, s.cfg.PlatformInstanceID, s.cfg.ClaimLease)
		}
		for {
			select {
			case <-heartbeat.stop:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := heartbeatOnce(); err != nil {
					// 任务已终态(正常完成/取消后 dispatch 尚未退出): 0 rows 是
					// 预期, 不记录错误, 静默退出(审查 F5——不能把正常完成的
					// 任务误报为 lease 丢失)。
					if current, getErr := s.cfg.Store.GetTask(ctx, task.ID); getErr == nil && current.Status.IsTerminal() {
						return
					}
					// lease 丢失(0 rows 且任务未终态)是确定性事件, 不重试(审查 F5)。
					if errors.Is(err, domain.ErrLeaseExpired) {
						// 审查 R5-I1: 任务已退回 queued(容量满/foreign-owner)——
						// 0 rows 是 requeue 的预期结果, 不是 lease 丢失, 静默退出。
						if heartbeat.requeued.Load() {
							return
						}
						heartbeat.setError(err)
						cancel()
						return
					}
					// 瞬时 DB 错误: 在短重试窗口内不放弃 dispatch(审查 F5)。
					// 一次抖动就取消 ctx 会让正在执行的任务被 lease 过期路径
					// 错误中断——正常任务不应因单次连接抖动失败。
					recovered := false
					for retry := 0; retry < maxHeartbeatRetries; retry++ {
						select {
						case <-heartbeat.stop:
							return
						case <-ctx.Done():
							return
						case <-time.After(interval / 2):
						}
						retryErr := heartbeatOnce()
						if retryErr == nil {
							recovered = true
							break
						}
						if current, getErr := s.cfg.Store.GetTask(ctx, task.ID); getErr == nil && current.Status.IsTerminal() {
							return
						}
						if errors.Is(retryErr, domain.ErrLeaseExpired) {
							if heartbeat.requeued.Load() {
								return
							}
							heartbeat.setError(retryErr)
							cancel()
							return
						}
					}
					if recovered {
						continue
					}
					current, getErr := s.cfg.Store.GetTask(ctx, task.ID)
					if getErr == nil && current.Status.IsTerminal() {
						return
					}
					if heartbeat.requeued.Load() {
						return
					}
					heartbeat.setError(err)
					cancel()
					return
				}
			}
		}
	}()
	return heartbeat, nil
}

// maxHeartbeatRetries 是瞬时 heartbeat 失败后的重试次数(审查 F5): 重试
// 窗口内不放弃正在执行的 dispatch, 避免单次 DB 抖动错误中断正常任务;
// 重试耗尽或 lease 丢失时由 dispatch 的 Stop() 错误路径终态化。
const maxHeartbeatRetries = 3
