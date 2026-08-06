package application

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/domain"
)

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
	// round12 审查(I3): 先取消再等待——heartbeat 可能在 DB 调用中阻塞,
	// 先等 done 后 cancel 会让 Stop 无限等待; ctx 取消后所有 DB 调用返回,
	// goroutine 经 select 的 ctx.Done 分支退出。等待带超时兜底。
	h.cancel()
	select {
	case <-h.done:
	case <-time.After(heartbeatStopTimeout):
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.err
}

// heartbeatStopTimeout 是 Stop 等待 heartbeat goroutine 退出的上限
// (round12 审查 I3): 正常情况下 ctx 取消后 goroutine 立即退出, 超时仅
// 防止极端情况(goroutine 不响应取消)无限阻塞任务收尾。
const heartbeatStopTimeout = 5 * time.Second

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
					// 预期。round9 审查: 不能直接静默退出——任务可能被恢复流程/
					// 新 owner 终态化而本 dispatch 仍在执行(旧进程暂停后恢复),
					// 必须取消派发上下文让 ExecuteTask 中断并销毁 Worker, 防止与
					// 接管者重叠执行。正常完成路径 terminal 已收到, cancel 无
					// 副作用; 不 setError, 避免误触发 fallback 终态化覆盖他方状态。
					if current, getErr := s.cfg.Store.GetTask(ctx, task.ID); getErr == nil && current.Status.IsTerminal() {
						cancel()
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
							cancel()
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
						cancel()
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
