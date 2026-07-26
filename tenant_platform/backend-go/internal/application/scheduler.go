package application

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/infrastructure/checkpoint"
	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/domain"
	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/infrastructure/llmproxy"
	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/infrastructure/policy"
	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/infrastructure/worker"
	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/infrastructure/workerclient"
)

// Scheduler is the P0 single-session claim/dispatch loop.
type Scheduler interface {
	Run(ctx context.Context) error
	KickSession(ctx context.Context, sessionKey string) error
	Recover(ctx context.Context, platformInstanceID string) error
	CancelWorker(ctx context.Context, task domain.Task) error
}

// SchedulerConfig carries process-lifetime identity and lease.
type SchedulerConfig struct {
	PlatformInstanceID string
	ClaimLease         time.Duration
	PollInterval       time.Duration
	Store              TaskStore
	Registry           policy.Registry
	Coordinator        checkpoint.Coordinator
	// Runtime creates a Worker instance for a session. Required.
	Runtime worker.WorkerRuntime
	// ConfigRoot is where the platform writes the token-only mykey.py for the
	// Worker. It may be global in loopback dev mode or session-scoped in
	// container mode.
	ConfigRoot string
	// SessionScopedConfig, when true, writes mykey.py under
	// ConfigRoot/<session-key> and passes that path as ConfigDir to the runtime.
	// Required for container mode so each container mounts only its own config.
	SessionScopedConfig bool
	// RuntimeRoot is the parent directory for checkpoint/runtime data.
	RuntimeRoot string
	// Optional injected Worker factory for unit tests. Deprecated: prefer
	// passing a worker.StaticRuntime as Runtime. When set and Runtime is nil,
	// the scheduler wraps it in a static runtime.
	DialWorker func(ctx context.Context, sessionKey string) (workerclient.WorkerClient, func(), error)
	// LLM Proxy capability_token issuance. Required for real Worker path: the
	// platform issues a short-lived, session-bound token and writes a token-only
	// mykey.py (no real upstream key).
	TokenIssuer        *llmproxy.Issuer
	TokenRevoker       TokenRevoker
	LLMProxyAddr       string // e.g. "http://127.0.0.1:8081"
	ModelPolicyVersion string
	// LLMProvider is the admin-configured upstream provider. Required when
	// TokenIssuer is set so the scheduler can stamp provider/model into the
	// capability_token and write the matching mykey.py variable.
	LLMProvider LLMProviderSource
	// MaxBundleBytes for checkpoint prepare.
	MaxBundleBytes uint64
	// MaxRunningTasks caps the global number of simultaneously starting/running
	// tasks. Zero disables the check (dev/test only). Production should set
	// this to a value derived from host capacity testing.
	MaxRunningTasks int
	// PerTenantRunningLimit caps the number of simultaneously starting/running
	// tasks per requester (across all their sessions). Zero disables the check.
	PerTenantRunningLimit int
	// TaskTimeoutSeconds is passed to the Worker as RuntimePolicy.TaskTimeoutSeconds
	// for its internal soft timer (e.g. cancelling a single hung LLM call). The
	// platform does NOT use it as a hard wall-clock kill switch — legitimate
	// tasks may run many times longer than a single LLM call. Stuck-task
	// detection relies on gRPC stream errors and heartbeat lease loss instead.
	// Zero disables the Worker soft timer (dev/test).
	TaskTimeoutSeconds int
	// IdleTimeout enables Temporal-HeartbeatTimeout-style idle detection.
	// When a running task's last_activity_at is older than now()-IdleTimeout,
	// the reaper finalizes it as failed (WORKER_IDLE). This catches "Worker
	// alive but deadlocked" (LLM HTTP call hung, GIL deadlock, infinite loop)
	// — the scenario gRPC stream errors + heartbeat lease loss cannot catch.
	// Worker keeps last_activity_at fresh via chunk events + drain poll
	// heartbeats. Zero disables idle reaping (dev/test only).
	IdleTimeout time.Duration
}

// LLMProviderSource returns the platform's current default LLM provider.
type LLMProviderSource interface {
	GetDefaultProvider(ctx context.Context) (domain.LLMProvider, error)
}

const (
	defaultMaxTurns           = 6
	defaultMaxHistoryBytes    = 256 * 1024
	defaultMaxWorkingBytes    = 64 * 1024
	defaultMaxOutputBytes     = 256 * 1024
	defaultWorkerShutdownSecs = 5

	workerShutdownTimeout = defaultWorkerShutdownSecs * time.Second
)

// ErrLeaseExpired is re-exported from domain for callers in the application
// layer. The postgres store returns this when a heartbeat updates 0 rows.
var ErrLeaseExpired = domain.ErrLeaseExpired

type scheduler struct {
	cfg          SchedulerConfig
	mu           sync.Mutex
	workerCallMu sync.Mutex
	wake         chan struct{}
	workers      map[string]*workerEntry // session_key -> dedicated worker
	cancelOnce   sync.Map                // taskID -> *cancelCall
}

// NewScheduler validates config and constructs the scheduler.
func NewScheduler(cfg SchedulerConfig) (Scheduler, error) {
	if strings.TrimSpace(cfg.PlatformInstanceID) == "" {
		return nil, fmt.Errorf("SchedulerConfig.PlatformInstanceID is required")
	}
	if cfg.ClaimLease <= 0 {
		return nil, fmt.Errorf("SchedulerConfig.ClaimLease must be positive")
	}
	if cfg.Store == nil || cfg.Registry == nil {
		return nil, fmt.Errorf("store and registry are required")
	}
	if cfg.Runtime == nil && cfg.DialWorker != nil {
		cfg.Runtime = worker.NewStaticRuntime(cfg.DialWorker)
	}
	if cfg.Runtime == nil {
		return nil, fmt.Errorf("SchedulerConfig.Runtime is required")
	}
	// Real Worker path (no injected DialWorker) MUST go through the LLM Proxy:
	// token issuer + revoker + Proxy address + config root for token-only
	// mykey.py. This is the spec §7.1 security red line.
	if cfg.DialWorker == nil {
		if cfg.TokenIssuer == nil {
			return nil, fmt.Errorf("SchedulerConfig.TokenIssuer is required for real Worker path")
		}
		if cfg.TokenRevoker == nil {
			return nil, fmt.Errorf("SchedulerConfig.TokenRevoker is required for real Worker path")
		}
		if strings.TrimSpace(cfg.LLMProxyAddr) == "" {
			return nil, fmt.Errorf("SchedulerConfig.LLMProxyAddr is required for real Worker path")
		}
		if strings.TrimSpace(cfg.ConfigRoot) == "" {
			return nil, fmt.Errorf("SchedulerConfig.ConfigRoot is required for real Worker path")
		}
		if cfg.LLMProvider == nil {
			return nil, fmt.Errorf("SchedulerConfig.LLMProvider is required for real Worker path")
		}
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = time.Second
	}
	if cfg.MaxBundleBytes == 0 {
		cfg.MaxBundleBytes = 2 * 1024 * 1024
	}
	return &scheduler{
		cfg:     cfg,
		wake:    make(chan struct{}, 1),
		workers: make(map[string]*workerEntry),
	}, nil
}

func (s *scheduler) KickSession(ctx context.Context, sessionKey string) error {
	_ = ctx
	_ = sessionKey
	select {
	case s.wake <- struct{}{}:
	default:
	}
	return nil
}

func (s *scheduler) Recover(ctx context.Context, platformInstanceID string) error {
	if platformInstanceID == "" {
		platformInstanceID = s.cfg.PlatformInstanceID
	}
	_, err := s.cfg.Store.RecoverAfterRestart(ctx, platformInstanceID)
	return err
}

func (s *scheduler) Run(ctx context.Context) error {
	ticker := time.NewTicker(s.cfg.PollInterval)
	defer ticker.Stop()
	for {
		if err := s.tick(ctx); err != nil && !errors.Is(err, context.Canceled) {
			// Keep running; surface via stderr-like return only on ctx done.
			_ = err
		}
		select {
		case <-ctx.Done():
			shutCtx, shutCancel := context.WithTimeout(context.Background(), workerShutdownTimeout*3)
			defer shutCancel()
			s.shutdownAllWorkers(shutCtx)
			return ctx.Err()
		case <-s.wake:
		case <-ticker.C:
		}
	}
}

func (s *scheduler) tick(ctx context.Context) error {
	// Recover newly expired foreign-owner work opportunistically.
	if _, err := s.cfg.Store.RecoverAfterRestart(ctx, s.cfg.PlatformInstanceID); err != nil {
		return err
	}
	// Heartbeat owned claims.
	owned, err := s.cfg.Store.ListOwnedActiveTasks(ctx, s.cfg.PlatformInstanceID)
	if err != nil {
		return err
	}
	for _, t := range owned {
		hbErr := s.cfg.Store.HeartbeatClaim(ctx, t.ID, s.cfg.PlatformInstanceID, s.cfg.ClaimLease)
		switch {
		case hbErr == nil:
			// ok
		case errors.Is(hbErr, domain.ErrLeaseExpired):
			// Lease lost (expired or stolen by RecoverAfterRestart on another
			// instance). The dispatch goroutine — if any — will observe ctx
			// cancel via the heartbeat's own context and exit. Finalize the
			// task so the running slot is released; otherwise it would block
			// MaxRunningTasks forever.
			slog.ErrorContext(ctx, "scheduler: heartbeat lost lease; finalizing task",
				"task_id", t.ID,
				"session_key", t.SessionKey,
				"status", string(t.Status))
			_ = s.finalizeOrFail(ctx, t, domain.TaskFailed, domain.DeliveryTaskFailed,
				"LEASE_EXPIRED", "claim lease expired or lost during heartbeat", "")
			_ = s.KickSession(ctx, t.SessionKey)
			continue
		default:
			// DB connectivity error; don't finalize — retry next tick.
			slog.ErrorContext(ctx, "scheduler: heartbeat failed (transient)",
				"task_id", t.ID,
				"session_key", t.SessionKey,
				"error", hbErr)
		}
		// Drive cancel if requested and dispatch started.
		if t.CancelRequestedAt != nil && t.WorkerDispatchStartedAt != nil {
			s.maybeCancelWorker(ctx, t)
		}
	}
	// Stuck-task reaping uses idle detection (Temporal HeartbeatTimeout pattern):
	//   1. gRPC stream error/close — Worker process crashed or network died.
	//      The dispatch loop's streamErr path finalizes the task immediately.
	//   2. Heartbeat lease loss — dispatch goroutine died (panic, Goexit) so
	//      HeartbeatClaim stopped being called. The tick loop's ErrLeaseExpired
	//      path finalizes the task on the next tick.
	//   3. Idle timeout — Worker alive but deadlocked (LLM HTTP call hung, GIL
	//      deadlock, infinite loop). Reaper checks last_activity_at (updated by
	//      RecordChunkEvent on every chunk and RecordHeartbeat on drain poll)
	//      against TASK_IDLE_TIMEOUT_SECONDS. Legitimate long tasks keep
	//      producing chunks or heartbeats, so they are not reaped.
	if s.cfg.IdleTimeout > 0 {
		if err := s.reapIdleTasks(ctx, owned, s.cfg.IdleTimeout); err != nil {
			slog.ErrorContext(ctx, "scheduler: reap idle tasks failed", "error", err)
		}
	}
	// If we already own a non-terminal starting/running task, continue dispatch if needed.
	for _, t := range owned {
		if t.Status == domain.TaskStarting && t.WorkerDispatchStartedAt == nil {
			if err := s.dispatch(ctx, t); err != nil {
				return err
			}
			return nil
		}
		if t.Status == domain.TaskStarting && t.WorkerDispatchStartedAt != nil {
			// In-flight; wait for completion path (dispatch holds the stream).
			return nil
		}
		if t.Status == domain.TaskRunning {
			return nil
		}
	}
	keys, err := s.cfg.Store.ListClaimableSessionKeys(ctx, 16, s.cfg.PerTenantRunningLimit)
	if err != nil {
		return err
	}
	if len(keys) == 0 {
		return nil
	}
	// Global running cap: don't claim new tasks when at capacity. The check
	// happens after ListClaimableSessionKeys so the cap doesn't mask session
	// discovery errors. Zero means disabled (dev/test).
	if s.cfg.MaxRunningTasks > 0 {
		running, err := s.cfg.Store.CountRunningTasks(ctx)
		if err != nil {
			return err
		}
		if running >= s.cfg.MaxRunningTasks {
			return nil
		}
	}
	for _, sk := range keys {
		task, ok, err := s.cfg.Store.ClaimNextTask(ctx, sk, s.cfg.PlatformInstanceID, s.cfg.ClaimLease)
		if err != nil {
			return err
		}
		if !ok {
			continue
		}
		return s.dispatch(ctx, task)
	}
	return nil
}
