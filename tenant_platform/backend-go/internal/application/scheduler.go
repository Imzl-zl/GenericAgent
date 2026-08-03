package application

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/domain"
	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/infrastructure/checkpoint"
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
	// SessionFiles 供成功事务前捕获 [FILE:...] 输出文件快照(审查 R5-I3);
	// nil(loopback 未接线)时跳过捕获, 成功事务不绑定文件内容。
	SessionFiles SessionFiles
	// Runtime creates a Worker instance for a session. Required.
	Runtime worker.WorkerRuntime
	// ConfigRoot holds the token-only runtime JSON and fixed mykey.py loader.
	// Production uses a hashed session-scoped subdirectory per Worker.
	ConfigRoot string
	// SessionScopedConfig writes each session under a SHA-256 directory and
	// passes only that directory to the Worker runtime.
	SessionScopedConfig bool
	// RuntimeConfigDir 返回某 session 的运行时配置目录(credential 刷新写卷
	// 目标, 审查 C4)。生产 Runner 模式返回 workspace 共享卷的 config/ 目录
	// (Runner 以 /ga/runner-config 只读挂载); nil 时回退 configDirFor
	// (Platform 本地 ConfigRoot, loopback/测试)。
	RuntimeConfigDir func(sessionKey string) string
	// RuntimeRoot is the parent directory for checkpoint/runtime data.
	RuntimeRoot string
	// Optional injected Worker factory for unit tests. Deprecated: prefer
	// passing a worker.StaticRuntime as Runtime. When set and Runtime is nil,
	// the scheduler wraps it in a static runtime.
	DialWorker func(ctx context.Context, sessionKey string) (workerclient.WorkerClient, func(), error)
	// LLM Proxy capability issuance. Required for real Worker paths.
	TokenIssuer               *llmproxy.Issuer
	CapabilityStore           CapabilityStore
	RevocationCleanupInterval time.Duration
	Audit                     AuditRecorder
	LLMProxyAddr              string
	// SophubProxyBaseURL 是 Platform 的 Worker Sophub proxy 地址(方案 §5.2);
	// 非空时向 Worker 下发 _platform_sophub capability。
	SophubProxyBaseURL string
	ModelPolicyVersion        string
	// LLMProvider resolves an immutable provider routing snapshot per Worker.
	LLMProvider LLMProviderSource
	// MCPServer resolves the administrator-enabled global MCP catalog.
	MCPServer MCPServerSource
	// TokenTTL must cover a complete task plus the pre-dispatch refresh skew.
	TokenTTL         time.Duration
	TokenRefreshSkew time.Duration
	MaxTaskWallClock time.Duration
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
	// RuntimeSettings supplies administrator-managed Agent execution limits.
	// Nil uses the production default.
	RuntimeSettings AgentRuntimeSettings
	// IdleTimeout enables Temporal-HeartbeatTimeout-style idle detection.
	// When a running task's last_activity_at is older than now()-IdleTimeout,
	// the reaper finalizes it as failed (WORKER_IDLE). This catches "Worker
	// alive but deadlocked" (LLM HTTP call hung, GIL deadlock, infinite loop)
	// — the scenario gRPC stream errors + heartbeat lease loss cannot catch.
	// Worker keeps last_activity_at fresh via chunk events + drain poll
	// heartbeats. Zero disables idle reaping (dev/test only).
	IdleTimeout time.Duration
	// WorkerIdleTTL sets how long a Worker process can stay resident after its
	// session becomes idle (no queued/starting/running task). When > 0, the
	// scheduler evicts idle Workers to reclaim memory. Pattern: Kubernetes pod
	// eviction + AWS Lambda container reuse window. Real deployments should
	// set 5-15 minutes to balance cold-start overhead vs memory pressure.
	// Zero keeps Workers resident indefinitely (dev/test or tiny fleets).
	WorkerIdleTTL time.Duration
}

// LLMProviderSource resolves active routing order and individual live revisions.
type LLMProviderSource interface {
	ListActiveProviders(ctx context.Context) ([]domain.LLMProvider, error)
	GetProvider(ctx context.Context, id int64) (domain.LLMProvider, error)
}

type MCPServerSource interface {
	ListEnabledMCPServers(ctx context.Context) ([]domain.MCPServer, error)
}

type CapabilityStore interface {
	RevokeCapability(ctx context.Context, jti string, expiresAt time.Time) error
	DeleteExpiredCapabilityRevocations(ctx context.Context, before time.Time) (int64, error)
}

// AgentRuntimeSettings resolves the live turn budget used when starting a
// Worker session. Admin updates apply to subsequent tasks.
type AgentRuntimeSettings interface {
	GetAgentMaxTurns(ctx context.Context) (int, error)
}

const (
	defaultMaxTurns           = domain.DefaultAgentMaxTurns
	defaultMaxHistoryBytes    = 256 * 1024
	defaultMaxWorkingBytes    = 64 * 1024
	defaultMaxOutputBytes     = 256 * 1024
	defaultWorkerShutdownSecs = 5

	DefaultTokenRefreshSkew          = 5 * time.Minute
	DefaultMaxTaskWallClock          = 45 * time.Minute
	defaultRevocationCleanupInterval = time.Minute
	workerShutdownTimeout            = defaultWorkerShutdownSecs * time.Second
)

// ErrLeaseExpired is re-exported from domain for callers in the application
// layer. The postgres store returns this when a heartbeat updates 0 rows.
var ErrLeaseExpired = domain.ErrLeaseExpired

type scheduler struct {
	cfg                   SchedulerConfig
	mu                    sync.Mutex
	workerCallMu          sync.Mutex
	wake                  chan struct{}
	workers               map[string]*workerEntry // session_key -> dedicated worker
	cancelOnce            sync.Map                // taskID -> *cancelCall
	// lastCheckpointSweep 记录上次 checkpoint 残留清理时间(节流, 审查 R4-I12)。
	lastCheckpointSweep time.Time
	// dispatchInFlight 是 dispatch goroutine 的进程内 in-flight gate
	// (taskID -> struct{}): tick 对 WorkerDispatchStartedAt==nil 的任务每轮
	// 都会派发, 而 dispatch 在 MarkDispatchStarted 前有昂贵的初始化
	// (lease/证书/容器/StartSession)。gate 防止同一任务被并发派发两次、
	// 两个副本互相销毁 Worker 或重复创建 Runner(审查 R4-C1)。
	dispatchInFlight      sync.Map
	lastRevocationCleanup time.Time
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
	// Real Worker paths MUST use the LLM Proxy, persistent capability
	// revocation, and session-scoped token-only runtime configuration.
	if cfg.DialWorker == nil {
		if cfg.TokenIssuer == nil {
			return nil, fmt.Errorf("SchedulerConfig.TokenIssuer is required for real Worker path")
		}
		if cfg.CapabilityStore == nil {
			return nil, fmt.Errorf("SchedulerConfig.CapabilityStore is required for real Worker path")
		}
		if cfg.Audit == nil {
			return nil, fmt.Errorf("SchedulerConfig.Audit is required for real Worker path")
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
	if cfg.MaxTaskWallClock == 0 && cfg.DialWorker != nil {
		cfg.MaxTaskWallClock = DefaultMaxTaskWallClock
	}
	if cfg.TokenRefreshSkew == 0 && cfg.DialWorker != nil {
		cfg.TokenRefreshSkew = DefaultTokenRefreshSkew
	}
	if cfg.TokenTTL == 0 && cfg.DialWorker != nil {
		cfg.TokenTTL = llmproxy.DefaultTokenTTL
	}
	if err := validateSchedulerCredentialTiming(cfg); err != nil {
		return nil, err
	}
	if cfg.RevocationCleanupInterval < 0 {
		return nil, fmt.Errorf("SchedulerConfig.RevocationCleanupInterval must not be negative")
	}
	if cfg.RevocationCleanupInterval == 0 && cfg.CapabilityStore != nil {
		cfg.RevocationCleanupInterval = defaultRevocationCleanupInterval
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


func validateSchedulerCredentialTiming(cfg SchedulerConfig) error {
	if cfg.MaxTaskWallClock <= 0 {
		return fmt.Errorf("SchedulerConfig.MaxTaskWallClock must be positive")
	}
	if cfg.TokenRefreshSkew < 0 {
		return fmt.Errorf("SchedulerConfig.TokenRefreshSkew must not be negative")
	}
	if cfg.TokenTTL < cfg.MaxTaskWallClock+cfg.TokenRefreshSkew {
		return fmt.Errorf("SchedulerConfig.TokenTTL must cover MaxTaskWallClock plus TokenRefreshSkew")
	}
	if cfg.TokenIssuer != nil && cfg.TokenIssuer.TTL() != cfg.TokenTTL {
		return fmt.Errorf("SchedulerConfig.TokenTTL must match TokenIssuer TTL")
	}
	return nil
}

// countActiveRunners 返回当前任务并发占用(starting+running 任务数)。
// 任务并发(MaxRunningTasks)与 Runner 容量(GA_RUNNER_MAX_ACTIVE)是
// 两个独立不变量(审查 C3): Runner 容量只在新建 lease 时由 DB 原子校验
// (AcquireRunnerLease), 已有活跃 lease 的 runner_key 复用不占新容量;
// 调度 tick 只按任务并发门控 claim, 绝不以活跃 lease 数饿死复用 Runner。
func (s *scheduler) countActiveRunners(ctx context.Context) (int64, error) {
	count, err := s.cfg.Store.CountRunningTasks(ctx)
	return int64(count), err
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
	if err := s.cleanupExpiredCapabilityRevocations(ctx, time.Now().UTC()); err != nil {
		slog.ErrorContext(ctx, "scheduler: cleanup expired capability revocations failed", "error", err)
	}
	// 审查 R4-I12: 定期清理 Prepare 后未 Commit 的 checkpoint 残留
	// (writing snapshot + staging 文件), 防止失败任务永久占用 DB 行与磁盘。
	// 节流到每 5 分钟一次, 避免每个 tick 都扫表。
	if s.cfg.Coordinator != nil && time.Since(s.lastCheckpointSweep) > 5*time.Minute {
		s.lastCheckpointSweep = time.Now()
		if n, err := s.cfg.Coordinator.SweepExpiredCheckpoints(ctx); err != nil {
			slog.ErrorContext(ctx, "scheduler: sweep expired checkpoints failed", "error", err)
		} else if n > 0 {
			slog.InfoContext(ctx, "scheduler: quarantined expired checkpoint snapshots", "count", n)
		}
	}
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
			// 审查: lease 丢失意味着旧 Worker 已与持久 lease 脱节(可能被其他
			// 实例接管/销毁), 立即销毁本地 entry, 防止复用已 fence 的进程。
			s.evictWorkerAfterFailure(t.SessionKey)
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
	//      windowed RecordChunkEvent writes and RecordHeartbeat on drain poll)
	//      against TASK_IDLE_TIMEOUT_SECONDS. Legitimate long tasks keep
	//      producing chunks or heartbeats, so they are not reaped.
	if s.cfg.IdleTimeout > 0 {
		if err := s.reapIdleTasks(ctx, owned, s.cfg.IdleTimeout); err != nil {
			slog.ErrorContext(ctx, "scheduler: reap idle tasks failed", "error", err)
		}
	}
	// Reclaim memory from Workers whose session has been idle past the TTL
	// (architecture §8.3 WORKER_IDLE_TIMEOUT). Sessions with active owned
	// tasks are never touched; the next task cold-starts from the last
	// committed snapshot.
	s.evictIdleWorkers(owned)
	// 已 owned 的 starting 任务: 异步派发(允许多 Runner 并发, 方案 §7
	// GA_RUNNER_MAX_ACTIVE); running 任务由各自的 dispatch goroutine 持有,
	// tick 不再阻塞等待单个任务完成。
	for _, t := range owned {
		if t.Status == domain.TaskStarting && t.WorkerDispatchStartedAt == nil {
			go s.dispatch(ctx, t)
		}
	}
	// 容量内的新任务 claim + 异步派发, 直到达到全局任务并发上限
	// (MaxRunningTasks = starting+running 任务数)。Runner 容量
	// (GA_RUNNER_MAX_ACTIVE) 不在此门控: 已在 AcquireRunnerLease 的 DB
	// 事务内原子校验(审查 C3: 任务并发与 Runner 容量是两个不变量)。
	// 同 session key 的串行由 store 的活跃任务约束保证。
	for {
		if s.cfg.MaxRunningTasks > 0 {
			running, err := s.countActiveRunners(ctx)
			if err != nil {
				return err
			}
			if running >= int64(s.cfg.MaxRunningTasks) {
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
		claimed := false
		for _, sk := range keys {
			task, ok, err := s.cfg.Store.ClaimNextTask(ctx, sk, s.cfg.PlatformInstanceID, s.cfg.ClaimLease)
			if err != nil {
				return err
			}
			if !ok {
				continue
			}
			go s.dispatch(ctx, task)
			claimed = true
			break // 重新检查容量与可 claim 会话
		}
		if !claimed {
			return nil
		}
	}
}
