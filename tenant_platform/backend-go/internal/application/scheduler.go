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
	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/infrastructure/transport"
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
	// Streaming 是可选 IM 流式转发端口(IM_STREAMING_DELIVERY §4.2):
	// 支持流式回复的 transport 实现 StreamingSender(生产=ILinkAdapter 代理
	// poller /send stream_*; loopback/测试=LoopbackTransport)。nil = 关闭
	// (只发终态结果, 与现状一致)。
	Streaming transport.StreamingSender
	// Bots 解析流式回复目标渠道配置(与 delivery 同款接口)。nil 时流式
	// 转发关闭(转发判定 fail-closed)。
	Bots ChannelResolverByOwner
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
	// RuntimeConfigDir 返回某 session 在某 Runner generation 下的运行时配置
	// 目录(credential 刷新写卷共享 config/g<generation>, 与容器挂载一致,
	// 审查 C4/C1-I6: config 按 generation 隔离后写入必须落在容器实际挂载
	// 的 g<gen> 子目录, 否则 Runner 读不到刷新后的 token)。
	// nil 时回退 configDirFor(Platform 本地 ConfigRoot, loopback/测试)。
	RuntimeConfigDir func(sessionKey string, generation uint64) string
	// RuntimeRoot is the parent directory for checkpoint/runtime data.
	RuntimeRoot string
	// Optional injected Worker factory for unit tests. Deprecated: prefer
	// passing a worker.StaticRuntime as Runtime. When set and Runtime is nil,
	// the scheduler wraps it in a static runtime.
	// DialWorker 兼容 legacy loopback 拨号(审查 C1/I7: cleanup 接受 JTI
	// 参数; loopback 路径忽略)。
	DialWorker func(ctx context.Context, sessionKey string) (workerclient.WorkerClient, func(string), error)
	// LLM Proxy capability issuance. Required for real Worker paths.
	TokenIssuer               *llmproxy.Issuer
	CapabilityStore           CapabilityStore
	RevocationCleanupInterval time.Duration
	Audit                     AuditRecorder
	LLMProxyAddr              string
	// SophubProxyBaseURL 是 Platform 的 Worker Sophub proxy 地址(方案 §5.2);
	// 非空时向 Worker 下发 _platform_sophub capability。
	SophubProxyBaseURL string
	// MCPProxyBaseURL 是 Platform 的 Worker MCP proxy 地址(Runner 无公网出口,
	// MCP 一律经 Platform 受控代理); 非空且 MCP 快照含 server 时下发
	// _platform_mcp.proxy capability。
	MCPProxyBaseURL string
	ModelPolicyVersion string
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
	// StartSessionTimeout 是 StartSession RPC 的独立超时(round11 审查 C3):
	// Worker 冷启动/快照恢复慢或 RPC 卡住时, 只终止本 session 的派发,
	// 不阻塞其他工作区(StartSession/CancelTask 互斥已收窄到 per-entry 锁)。
	// 零值使用 DefaultStartSessionTimeout。
	StartSessionTimeout time.Duration

	// MaxRunningTasks caps the global number of simultaneously starting/running
	// tasks. Zero disables the check (dev/test only). Production should set
	// this to a value derived from host capacity testing.
	MaxRunningTasks int
	// PerRequesterRunningLimit caps the number of simultaneously starting/running
	// tasks per requester (across all their sessions). Zero disables the check.
	PerRequesterRunningLimit int
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
	// heartbeats, but heartbeat is a *progress* signal (审查 C1/I8): the drain
	// loop only emits it while the agent recently produced display events, so
	// a stalled agent stops refreshing last_activity_at and gets reaped.
	// Zero disables idle reaping (dev/test only).
	IdleTimeout time.Duration
}

// LLMProviderSource resolves active routing order and individual live revisions.
type LLMProviderSource interface {
	ListActiveProviders(ctx context.Context) ([]domain.LLMProvider, error)
	GetProvider(ctx context.Context, id int64) (domain.LLMProvider, error)
}

type MCPServerSource interface {
	ListEnabledMCPServers(ctx context.Context) ([]domain.MCPServer, error)
	// MCPQuotaAvailable 判断某用户对某 server 是否仍有可用配额
	// (无限额行 = 默认放行; 任一周期耗尽 = 不可用)。调度签发 MCP
	// capability 前按用户过滤, 耗尽 server 不下发(任务内不可见)。
	MCPQuotaAvailable(ctx context.Context, ownerKey, serverID string) (bool, error)
}

type CapabilityStore interface {
	RevokeCapability(ctx context.Context, jti string, expiresAt time.Time) error
	DeleteExpiredCapabilityRevocations(ctx context.Context, before time.Time) (int64, error)
}

// AgentRuntimeSettings resolves the live turn budget used when starting a
// Worker session. Admin updates apply to subsequent tasks.
type AgentRuntimeSettings interface {
	GetAgentMaxTurns(ctx context.Context) (int, error)
	// GetIMStreamingMode 解析 IM 流式输出开关(off|final_only|streaming)。
	// 读失败时调用方 fail-closed(不转发, 终态 delivery 兜底)。
	GetIMStreamingMode(ctx context.Context) (domain.IMStreamingMode, error)
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
	// DefaultStartSessionTimeout 覆盖 Worker 冷启动 + 恢复快照读取的最慢
	// 合法场景; 超过即视为 Worker 不可用, 由后续重试/重建兜底。
	DefaultStartSessionTimeout = 90 * time.Second
	// workerControlTimeout 是控制 RPC(CancelTask/Shutdown)的单次超时:
	// 卡住的控制调用不得长期占用 per-entry 锁阻塞同 session 后续派发。
	workerControlTimeout  = 30 * time.Second
	workerShutdownTimeout = defaultWorkerShutdownSecs * time.Second
)

// ErrLeaseExpired is re-exported from domain for callers in the application
// layer. The postgres store returns this when a heartbeat updates 0 rows.
var ErrLeaseExpired = domain.ErrLeaseExpired

type scheduler struct {
	cfg        SchedulerConfig
	mu         sync.Mutex
	wake       chan struct{}
	workers    map[string]*workerEntry // session_key -> dedicated worker
	cancelOnce sync.Map                // taskID -> *cancelCall
	// lastCheckpointSweep 记录上次 checkpoint 残留清理时间(节流, 审查 R4-I12)。
	lastCheckpointSweep time.Time
	// dispatchInFlight 是 dispatch goroutine 的进程内 in-flight gate
	// (taskID -> struct{}): tick 对 WorkerDispatchStartedAt==nil 的任务每轮
	// 都会派发, 而 dispatch 在 MarkDispatchStarted 前有昂贵的初始化
	// (lease/证书/容器/StartSession)。gate 防止同一任务被并发派发两次、
	// 两个副本互相销毁 Worker 或重复创建 Runner(审查 R4-C1)。
	dispatchInFlight      sync.Map
	lastRevocationCleanup time.Time
	// pendingFinalize 是终态化写库失败的任务意图(taskID -> finalizeIntent,
	// round12 审查 I2): tick 每轮重试 CompleteFailedTerminal, 防止任务因
	// 瞬时 DB 错误永久卡在 starting/running。进程崩溃时由 claim 过期 +
	// RecoverAfterRestart 兜底。
	pendingFinalize sync.Map
	// sessionDraining 标记正在销毁 Worker 的 session(审查 D2): 成功路径
	// 在 CompleteSucceeded 提交终态(释放串行槽)与 destroyTaskWorkerLocked
	// 之间, 同 session 下一任务可能被 claim 并复用同 generation 容器,
	// 随后旧 entry cleanup 销毁该容器杀死新任务。draining 期间 tick 跳过
	// 该 session 的 claim; destroy 完成后清除。跨实例接管必然 generation+1
	// 新容器, 旧容器销毁语义不变, 无需跨实例协调。
	sessionDraining sync.Map
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

// countActiveTasks 返回当前任务并发占用(starting+running 任务数)。
// 任务并发(MaxRunningTasks)与 Runner 容量(GA_RUNNER_MAX_ACTIVE)是
// 两个独立不变量(审查 C3): Runner 容量只在新建 lease 时由 DB 原子校验
// (AcquireRunnerLease), 已有活跃 lease 的 runner_key 复用不占新容量;
// 调度 tick 只按任务并发门控 claim, 绝不以活跃 lease 数饿死复用 Runner。
func (s *scheduler) countActiveTasks(ctx context.Context) (int64, error) {
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
			// 审查 I-3: tick 内 DB 故障(列表/claim/recover 失败)必须可见——
			// 静默丢弃会让 Postgres 故障时每 PollInterval 空转且无任何告警。
			slog.ErrorContext(ctx, "scheduler: tick failed", "error", err)
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
		// round11 审查(C2): 对账回收确定孤儿 committed/result 文件——提交结果
		// 不确定时保留的文件最终在此按 DB 引用回收, 避免不确定窗口误删恢复点
		// 与磁盘永久泄漏两个方向的契约破坏。
		if n, err := s.cfg.Coordinator.ReconcileOrphanCommittedFiles(ctx); err != nil {
			slog.ErrorContext(ctx, "scheduler: reconcile orphan committed files failed", "error", err)
		} else if n > 0 {
			slog.InfoContext(ctx, "scheduler: removed orphan committed files", "count", n)
		}
		// round12 审查(M2): 对账回收无 writing 引用的孤儿 staging 文件
		// (Commit 后删除失败/提交期间崩溃的残留)。
		if n, err := s.cfg.Coordinator.ReconcileOrphanStagingFiles(ctx); err != nil {
			slog.ErrorContext(ctx, "scheduler: reconcile orphan staging files failed", "error", err)
		} else if n > 0 {
			slog.InfoContext(ctx, "scheduler: removed orphan staging files", "count", n)
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
			_ = s.terminateTask(ctx, t, domain.TaskFailed, domain.DeliveryTaskFailed,
				"LEASE_EXPIRED", "claim lease expired or lost during heartbeat", "")
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
	// 已 owned 的 starting 任务: 异步派发(允许多 Runner 并发, 方案 §7
	// GA_RUNNER_MAX_ACTIVE); running 任务由各自的 dispatch goroutine 持有,
	// tick 不再阻塞等待单个任务完成。
	// round12 审查(I2): 心跳续租之后重试未落库的终态化意图。
	s.drainPendingFinalize(ctx)
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
			running, err := s.countActiveTasks(ctx)
			if err != nil {
				return err
			}
			if running >= int64(s.cfg.MaxRunningTasks) {
				return nil
			}
		}
		keys, err := s.cfg.Store.ListClaimableSessionKeys(ctx, 16, s.cfg.PerRequesterRunningLimit)
		if err != nil {
			return err
		}
		if len(keys) == 0 {
			return nil
		}
		claimed := false
		for _, sk := range keys {
			// 审查 D2: 正在销毁 Worker 的 session 不得 claim 新任务——
			// 成功路径终态提交后到销毁完成前, 同 generation 容器仍被旧
			// entry 引用, 新任务复用后会被旧 cleanup 销毁。
			if _, draining := s.sessionDraining.Load(sk); draining {
				continue
			}
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
