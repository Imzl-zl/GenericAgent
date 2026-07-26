package application

import (
	"context"
	"sync"
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
type dispatchHeartbeat struct {
	ctx      context.Context
	cancel   context.CancelFunc
	stop     chan struct{}
	done     chan struct{}
	stopOnce sync.Once
	mu       sync.Mutex
	err      error
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
		for {
			select {
			case <-heartbeat.stop:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := s.cfg.Store.HeartbeatClaim(ctx, task.ID, s.cfg.PlatformInstanceID, s.cfg.ClaimLease); err != nil {
					current, getErr := s.cfg.Store.GetTask(ctx, task.ID)
					if getErr == nil && current.Status.IsTerminal() {
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
