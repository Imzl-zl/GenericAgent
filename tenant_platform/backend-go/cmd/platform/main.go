// Command platform is the loopback-only foundation control plane.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/api"
	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/application"
	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/infrastructure/checkpoint"
	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/infrastructure/ilink"
	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/infrastructure/llmproxy"
	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/infrastructure/logging"
	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/infrastructure/policy"
	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/infrastructure/poller"
	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/infrastructure/postgres"
	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/infrastructure/processguard"
	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/infrastructure/sandbox"
	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/infrastructure/secret"
	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/infrastructure/sophub"
	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/infrastructure/systemd"
	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/infrastructure/transport"
	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/infrastructure/worker"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "platform: %v\n", err)
		os.Exit(1)
	}
}

func resolvePolicyPath(path string) (string, error) {
	resolved, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("resolve policy path: %w", err)
	}
	return resolved, nil
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// envInt reads an integer from the named env var, returning fallback when
// unset or unparsable. Used for quota tunables so operators can set them
// without touching flags.
func envInt(name string, fallback int) int {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return fallback
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return fallback
	}
	return n
}

// envIntEither reads the first non-empty of two environment variables;
// GA_RUNNER_MAX_ACTIVE is the new deployment name, MAX_RUNNING_TASKS the
// legacy one. The first set variable wins.
func envIntEither(primary, legacy string, fallback int) int {
	for _, name := range []string{primary, legacy} {
		raw := strings.TrimSpace(os.Getenv(name))
		if raw == "" {
			continue
		}
		n, err := strconv.Atoi(raw)
		if err != nil {
			continue
		}
		return n
	}
	return fallback
}

// buildWorkerRuntime constructs the production Worker runtime.
// GA_WORKER_EXECUTION_MODE=user_workspace_runner(默认)使用 Sandbox 工作区
// Runner; loopback 仅显式用于开发降级(方案 §7: 不作静默回退)。
func buildWorkerRuntime(
	boot application.DevBootstrapConfig,
	store *postgres.Store,
	instanceID, policyFile, llmProxyAddr, sophubProxyAddr string,
) (worker.WorkerRuntime, error) {
	mode := strings.TrimSpace(os.Getenv("GA_WORKER_EXECUTION_MODE"))
	if mode == "" {
		mode = "user_workspace_runner"
	}
	if mode == "loopback" {
		runtime, err := worker.NewLoopback(worker.LoopbackConfig{
			Python:     boot.WorkerPython,
			WorkerSrc:  boot.WorkerSrc,
			LegacyRoot: boot.LegacyRoot,
			PolicyFile: boot.PolicyFile,
		})
		if err != nil {
			return nil, fmt.Errorf("loopback runtime: %w", err)
		}
		return runtime, nil
	}
	if mode != "user_workspace_runner" {
		return nil, fmt.Errorf("unknown GA_WORKER_EXECUTION_MODE %q (user_workspace_runner|loopback)", mode)
	}
	if strings.TrimSpace(boot.WorkspacesRoot) == "" {
		return nil, fmt.Errorf("GA_WORKSPACES_ROOT is required for user_workspace_runner mode")
	}
	// Platform 不持有 Docker socket: 所有容器操作经认证的 Manager 控制面
	// (方案 §7)。GA_MANAGER_ADDR/GA_MANAGER_SECRET 为必填。
	managerAddr := strings.TrimSpace(os.Getenv("GA_MANAGER_ADDR"))
	managerSecret := strings.TrimSpace(os.Getenv("GA_MANAGER_SECRET"))
	if managerAddr == "" || managerSecret == "" {
		return nil, fmt.Errorf("GA_MANAGER_ADDR and GA_MANAGER_SECRET are required for user_workspace_runner mode")
	}
	manager, err := sandbox.NewManagerClient(managerAddr, managerSecret, "ga-runner")
	if err != nil {
		return nil, fmt.Errorf("sandbox manager client: %w", err)
	}
	ca, err := worker.NewPlatformCA()
	if err != nil {
		return nil, fmt.Errorf("runner control CA: %w", err)
	}
	var env []string
	if strings.TrimSpace(llmProxyAddr) != "" {
		env = append(env, "GA_LLM_PROXY_ADDR="+strings.TrimSpace(llmProxyAddr))
	}
	if strings.TrimSpace(sophubProxyAddr) != "" {
		env = append(env, "GA_SOPHUB_PROXY_ADDR="+strings.TrimSpace(sophubProxyAddr))
	}
	return worker.NewSandbox(worker.SandboxConfig{
		Manager:            manager,
		CA:                 ca,
		LeaseStore:         store,
		PlatformInstanceID: instanceID,
		WorkspaceRoot:      boot.WorkspacesRoot,
		MemoryTemplate:     boot.MemoryTemplate,
		Image:              boot.RunnerImage,
		ContainerPrefix:    "ga-runner",
		PolicyFile:         policyFile,
		Env:                env,
	})
}

// sandboxCLI 构建固定 profile 的 Docker CLI(仅 Manager 使用)。
// sophubProxyBaseURL 返回 Platform 的 Worker Sophub proxy 地址(方案 §5.2)。
// 默认走 Platform 自身的 /v1/worker/sophub 端点(与 API 同地址)。
func sophubProxyBaseURL() string {
	return strings.TrimSpace(os.Getenv("GA_SOPHUB_PROXY_ADDR"))
}

// runnerLeaseReaper 周期清理本实例已过期的 Runner lease: 销毁 lease 记录的
// 容器并释放 lease(方案 §7: 持久 lease 驱动的重启恢复/孤儿回收)。
// Worker idle eviction 的正常路径由 scheduler cleanup 直接销毁容器。
func runRunnerLeaseReaper(
	ctx context.Context,
	store *postgres.Store,
	manager sandbox.RunnerCLI,
	instanceID string,
	interval time.Duration,
) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			leases, err := store.ListExpiredRunnerLeases(ctx, time.Now().UTC())
			if err != nil {
				slog.ErrorContext(ctx, "platform: list expired runner leases failed", "error", err)
				continue
			}
			for _, lease := range leases {
				if lease.Owner != instanceID {
					continue // 其他实例的过期 lease 由对应实例接管
				}
				if lease.ContainerID != "" {
					if err := manager.Destroy(ctx, lease.ContainerID); err != nil {
						slog.WarnContext(ctx, "platform: destroy expired runner failed",
							"runner_key", lease.RunnerKey, "container", lease.ContainerID, "error", err)
						continue
					}
				}
				if err := store.ReleaseRunnerLease(ctx, lease.RunnerKey, instanceID); err != nil {
					slog.WarnContext(ctx, "platform: release expired runner lease failed",
						"runner_key", lease.RunnerKey, "error", err)
				}
			}
		}
	}
}

// llmProxyConfig carries LLM Proxy startup parameters. The real upstream key
// is fetched from the admin-configured provider store and decrypted with the
// cipher; it is never part of this static config.
type llmProxyConfig struct {
	externalAddr         string // when non-empty, use external Proxy (no in-process start)
	signingKey           string // HMAC signing key for capability JWTs (>=32 bytes)
	providerSource       llmproxy.ProviderSource
	cipher               llmproxy.TokenCipher
	revocations          llmproxy.CapabilityRevocationSource
	allowedUpstreamCIDRs []string
	allowedHTTPHosts     []string
}

func startLLMProxy(ctx context.Context, cfg llmProxyConfig) (string, func(), error) {
	if cfg.externalAddr != "" {
		return strings.TrimRight(cfg.externalAddr, "/"), func() {}, nil
	}
	if len(cfg.signingKey) < llmproxy.MinSigningKeyLen {
		return "", nil, fmt.Errorf("capability signing key must be at least %d bytes (use --capability-signing-key or LLM_PROXY_CAPABILITY_SIGNING_KEY)", llmproxy.MinSigningKeyLen)
	}
	if cfg.providerSource == nil {
		return "", nil, fmt.Errorf("LLM Proxy provider source is required")
	}
	if cfg.cipher == nil {
		return "", nil, fmt.Errorf("LLM Proxy cipher is required")
	}
	if cfg.revocations == nil {
		return "", nil, fmt.Errorf("LLM Proxy revocation source is required")
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", nil, fmt.Errorf("llm-proxy listen: %w", err)
	}
	proxyCfg := llmproxy.Config{
		Listen:               ln.Addr().String(),
		SigningKey:           []byte(cfg.signingKey),
		TokenTTL:             llmproxy.DefaultTokenTTL,
		ProviderSource:       cfg.providerSource,
		Cipher:               cfg.cipher,
		Revocations:          cfg.revocations,
		AllowedUpstreamCIDRs: cfg.allowedUpstreamCIDRs,
		AllowedHTTPHosts:     cfg.allowedHTTPHosts,
	}
	srv, err := llmproxy.NewServer(proxyCfg)
	if err != nil {
		_ = ln.Close()
		return "", nil, fmt.Errorf("llm-proxy server: %w", err)
	}
	httpSrv := llmproxy.NewHTTPServer("", srv.Handler())
	go func() { _ = httpSrv.Serve(ln) }()
	addr := "http://" + ln.Addr().String()
	fmt.Fprintf(os.Stderr, "platform: in-process llm-proxy listen=%s provider_source=store\n", ln.Addr().String())
	shutdown := func() {
		// Derive from Background, not ctx: by the time shutdown runs, ctx is
		// already cancelled (signal handler triggered), so WithTimeout(ctx)
		// would yield an immediately-expired context and hard-cut in-flight
		// requests. We want a graceful 5s window for handlers to finish.
		shutCtx, shutCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer shutCancel()
		_ = httpSrv.Shutdown(shutCtx)
	}
	return addr, shutdown, nil
}

// parseAndEnsureDevTeam parses the --dev-team flag and bootstraps the team.
// Format: "name:owner_id:member_id,member_id,..." or "name" (owner defaults to
// the primary dev user, no extra members).
func parseAndEnsureDevTeam(ctx context.Context, store *postgres.Store, spec string, defaultOwner int64) (postgres.TeamContext, error) {
	parts := strings.SplitN(spec, ":", 3)
	name := strings.TrimSpace(parts[0])
	if name == "" {
		return postgres.TeamContext{}, fmt.Errorf("--dev-team: name is required")
	}
	owner := defaultOwner
	if len(parts) > 1 {
		ownerStr := strings.TrimSpace(parts[1])
		if ownerStr != "" {
			parsed, err := strconv.ParseInt(ownerStr, 10, 64)
			if err != nil || parsed <= 0 {
				return postgres.TeamContext{}, fmt.Errorf("--dev-team: invalid owner id %q", ownerStr)
			}
			owner = parsed
		}
	}
	var members []int64
	if len(parts) > 2 {
		memberStr := strings.TrimSpace(parts[2])
		if memberStr != "" {
			for _, raw := range strings.Split(memberStr, ",") {
				raw = strings.TrimSpace(raw)
				if raw == "" {
					continue
				}
				mid, err := strconv.ParseInt(raw, 10, 64)
				if err != nil || mid <= 0 {
					return postgres.TeamContext{}, fmt.Errorf("--dev-team: invalid member id %q", raw)
				}
				members = append(members, mid)
			}
		}
	}
	teamID := uuid.New()
	return application.EnsureDevTeam(ctx, store, application.DevTeamConfig{
		TeamID:    teamID,
		TeamName:  name,
		OwnerID:   owner,
		MemberIDs: members,
	})
}

func finishPlatform(serveErr error, schedulerDone <-chan error, timeout time.Duration) error {
	select {
	case schedulerErr := <-schedulerDone:
		if schedulerErr != nil && !errors.Is(schedulerErr, context.Canceled) {
			return fmt.Errorf("scheduler shutdown: %w", schedulerErr)
		}
	case <-time.After(timeout):
		return fmt.Errorf("scheduler shutdown timed out after %s", timeout)
	}
	if errors.Is(serveErr, context.Canceled) {
		return nil
	}
	return serveErr
}

func run() error {
	if err := processguard.DisablePeerInspection(); err != nil {
		return fmt.Errorf("harden platform process: %w", err)
	}
	// Initialize structured logging before configuration parsing so early
	// failures produce JSON. LOG_LEVEL controls verbosity.
	logging.Init()

	var (
		policyFile            = flag.String("policy-file", "", "path to capability policy manifest (required)")
		claimLease            = flag.Duration("claim-lease", 0, "positive claim lease duration (required)")
		devLoopback           = flag.Bool("dev-loopback", false, "enable development loopback bootstrap and local coordinator")
		listen                = flag.String("listen", "127.0.0.1:8080", "loopback listen address")
		databaseURL           = flag.String("database-url", "", "PostgreSQL URL (or DATABASE_URL)")
		migration             = flag.String("migration", "", "path to 0001_foundation.sql")
		runtimeRoot           = flag.String("runtime-root", "", "GA_RUNTIME_DIR for local coordinator/worker")
		configRoot            = flag.String("config-root", "", "GA_CONFIG_ROOT for session-scoped token-only runtime configuration")
		legacyRoot            = flag.String("legacy-root", "", "GA_LEGACY_ROOT")
		workerPython          = flag.String("worker-python", "", "python interpreter for worker")
		workerSrc             = flag.String("worker-src", "", "path to worker-python/src")
		llmProxyAddr          = flag.String("llm-proxy-addr", "", "external LLM Proxy addr (e.g. http://127.0.0.1:8081); empty = start in-process Proxy in dev-loopback")
		capabilitySigningKey  = flag.String("capability-signing-key", "", "HMAC signing key for capability_tokens (or LLM_PROXY_CAPABILITY_SIGNING_KEY); >=32 bytes")
		modelPolicyVersion    = flag.String("model-policy-version", "foundation.no-host-tools.v1", "model_policy_version stamped into capability_tokens")
		devExtraUsers         = flag.String("dev-extra-users", "", "comma-separated extra dev user IDs to bootstrap with personal workspaces")
		devTeam               = flag.String("dev-team", "", "bootstrap a dev team: format 'name:owner_id:member_id,member_id,...'")
		botTokenKey           = flag.String("bot-token-key", os.Getenv("BOT_TOKEN_KEY"), "AES-256-GCM hex key for encrypting bot tokens (or BOT_TOKEN_KEY)")
		ilinkBaseURL          = flag.String("ilink-base-url", os.Getenv("ILINK_BASE_URL"), "iLink API base URL (or ILINK_BASE_URL); empty = loopback transport")
		ilinkAppID            = flag.String("ilink-app-id", firstNonEmpty(os.Getenv("ILINK_APP_ID"), "bot"), "iLink App-Id header")
		ilinkClientVersion    = flag.String("ilink-client-version", firstNonEmpty(os.Getenv("ILINK_CLIENT_VERSION"), "2.1.1"), "iLink App-ClientVersion header")
		botPollerURL          = flag.String("bot-poller-url", os.Getenv("BOT_POLLER_URL"), "Bot Poller HTTP base URL (or BOT_POLLER_URL); empty = loopback transport")
		botPollerAPISecret    = flag.String("bot-poller-api-secret", os.Getenv("BOT_POLLER_API_SECRET"), "HMAC-SHA256 secret for authenticating requests to Bot Poller /start /send /stop (or BOT_POLLER_API_SECRET); empty = unauthenticated (INSECURE - dev/test only)")
		platformWebhookURL    = flag.String("platform-webhook-url", os.Getenv("PLATFORM_WEBHOOK_URL"), "platform /v1/im/webhook URL told to the Bot Poller (or PLATFORM_WEBHOOK_URL)")
		webhookSecret         = flag.String("webhook-secret", os.Getenv("PLATFORM_WEBHOOK_SECRET"), "HMAC-SHA256 secret shared with the Bot Poller to authenticate /v1/im/webhook (or PLATFORM_WEBHOOK_SECRET); empty = unauthenticated (dev/test only)")
		maxRunningTasks       = flag.Int("max-running-tasks", envIntEither("GA_RUNNER_MAX_ACTIVE", "MAX_RUNNING_TASKS", 0), "global cap on simultaneously starting/running tasks (or GA_RUNNER_MAX_ACTIVE/MAX_RUNNING_TASKS); 0 = disabled (dev/test)")
		perTenantRunningLimit = flag.Int("per-tenant-running-limit", envInt("PER_TENANT_RUNNING_LIMIT", 0), "per-requester cap on simultaneously starting/running tasks across all sessions (or PER_TENANT_RUNNING_LIMIT); 0 = disabled (dev/test)")
		perUserQueueLimit     = flag.Int("per-user-queue-limit", envInt("PER_USER_QUEUE_LIMIT", 0), "per-requester cap on queued tasks (or PER_USER_QUEUE_LIMIT); 0 = disabled (dev/test)")
		taskTimeoutSeconds    = flag.Int("task-timeout-seconds", envInt("TASK_TIMEOUT_SECONDS", 0), "Worker-side wall-clock deadline for a whole task (or TASK_TIMEOUT_SECONDS); 0 = disabled (recommended; stuck detection uses gRPC stream errors + heartbeat lease loss instead). Set only when you want a hard task cap.")
		maxTaskWallClockSec   = flag.Int("max-task-wall-clock-seconds", envInt("MAX_TASK_WALL_CLOCK_SECONDS", 2700), "hard task wall-clock limit; capability TTL must cover this plus refresh skew")
		taskIdleTimeoutSec    = flag.Int("task-idle-timeout-seconds", envInt("TASK_IDLE_TIMEOUT_SECONDS", 300), "Idle reaper threshold (or TASK_IDLE_TIMEOUT_SECONDS). Default 300s (5min). A running task whose last_activity_at is older than this is finalized as WORKER_IDLE. Covers 'Worker alive but deadlocked' (GIL/hung I/O) — the scenario stream errors + lease loss cannot catch. Worker keeps last_activity_at fresh via chunk events + 30s heartbeats. 0 = disabled (dev/test only).")
		workerIdleTTLSec      = flag.Int("worker-idle-ttl-seconds", envInt("WORKER_IDLE_TTL_SECONDS", 600), "Worker eviction threshold (or WORKER_IDLE_TTL_SECONDS). Default 600s (10min). Idle Worker processes (no active task) older than this are torn down to reclaim memory (pattern: Kubernetes pod eviction + AWS Lambda container reuse window). Next task cold-starts from last snapshot. 0 = keep Workers resident indefinitely (dev/test or tiny fleets).")
	)
	flag.Parse()

	if strings.TrimSpace(*policyFile) == "" {
		return fmt.Errorf("--policy-file is required")
	}
	if *claimLease <= 0 {
		return fmt.Errorf("--claim-lease must be a positive duration")
	}
	resolvedPolicyFile, err := resolvePolicyPath(*policyFile)
	if err != nil {
		return fmt.Errorf("resolve --policy-file: %w", err)
	}

	// Generate platform instance id exactly once before opening PostgreSQL.
	instanceID, err := application.NewPlatformInstanceID()
	if err != nil {
		return fmt.Errorf("platform instance id: %w", err)
	}
	if instanceID == "" {
		return fmt.Errorf("platform instance id generation returned empty id")
	}

	dbURL := strings.TrimSpace(*databaseURL)
	if dbURL == "" {
		dbURL = strings.TrimSpace(os.Getenv("DATABASE_URL"))
	}
	if dbURL == "" {
		return fmt.Errorf("database URL required via --database-url or DATABASE_URL")
	}

	reg, err := policy.LoadRegistry(resolvedPolicyFile)
	if err != nil {
		return fmt.Errorf("load policy: %w", err)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		return fmt.Errorf("connect postgres: %w", err)
	}
	defer pool.Close()

	mig := *migration
	if mig == "" {
		mig = postgres.DefaultMigrationPath()
	}
	if err := postgres.EnsureSchema(ctx, pool, mig); err != nil {
		return err
	}

	store, err := postgres.NewStore(pool)
	if err != nil {
		return err
	}
	imInboundCoalesceWindowMS, err := store.GetIMInboundCoalesceWindowMS(ctx)
	if err != nil {
		return fmt.Errorf("load im inbound coalesce window: %w", err)
	}
	agentMaxTurns, err := store.GetAgentMaxTurns(ctx)
	if err != nil {
		return fmt.Errorf("load agent max turns: %w", err)
	}
	// Resource quotas: enforced by scheduler (global running cap) and store
	// (per-user queued cap). Zero disables (dev/test).
	store.SetPerUserQueueLimit(*perUserQueueLimit)
	if *maxRunningTasks > 0 || *taskTimeoutSeconds > 0 || *taskIdleTimeoutSec > 0 || *workerIdleTTLSec > 0 {
		fmt.Fprintf(os.Stderr, "platform: quota max_running_tasks=%d per_user_queue_limit=%d worker_task_timeout=%ds idle_reaper=%ds worker_idle_ttl=%ds\n",
			*maxRunningTasks, *perUserQueueLimit, *taskTimeoutSeconds, *taskIdleTimeoutSec, *workerIdleTTLSec)
	} else {
		fmt.Fprintf(os.Stderr, "platform: quotas disabled (max_running_tasks=0 per_user_queue_limit=0 worker_task_timeout=0 idle_reaper=0 worker_idle_ttl=0); stuck detection via gRPC stream errors + heartbeat lease loss\n")
	}

	boot, err := application.LoadDevBootstrapFromEnv()
	if err != nil {
		return err
	}
	boot.Enabled = *devLoopback
	boot.DatabaseURL = dbURL
	boot.PolicyFile = resolvedPolicyFile
	if *runtimeRoot != "" {
		boot.RuntimeRoot = *runtimeRoot
	}
	if *configRoot != "" {
		boot.ConfigRoot = *configRoot
	}
	if *legacyRoot != "" {
		boot.LegacyRoot = *legacyRoot
	}
	if *workerPython != "" {
		boot.WorkerPython = *workerPython
	}
	if *workerSrc != "" {
		boot.WorkerSrc = *workerSrc
	}
	if boot.RuntimeRoot == "" {
		boot.RuntimeRoot = strings.TrimSpace(os.Getenv("GA_RUNTIME_DIR"))
	}
	if boot.ConfigRoot == "" {
		boot.ConfigRoot = strings.TrimSpace(os.Getenv("GA_CONFIG_ROOT"))
	}
	if boot.LegacyRoot == "" {
		boot.LegacyRoot = strings.TrimSpace(os.Getenv("GA_LEGACY_ROOT"))
	}
	boot.WorkspacesRoot = strings.TrimSpace(os.Getenv("GA_WORKSPACES_ROOT"))
	boot.MemoryTemplate = strings.TrimSpace(os.Getenv("GA_MEMORY_TEMPLATE"))
	boot.RunnerImage = strings.TrimSpace(os.Getenv("GA_RUNNER_IMAGE"))
	if boot.RunnerImage == "" {
		boot.RunnerImage = "ga-runner:local"
	}

	var devCtx postgres.DevelopmentContext
	var coord checkpoint.Coordinator
	if *devLoopback {
		if boot.RuntimeRoot == "" || boot.ConfigRoot == "" || boot.LegacyRoot == "" {
			return fmt.Errorf("--dev-loopback requires GA_RUNTIME_DIR, GA_CONFIG_ROOT, GA_LEGACY_ROOT")
		}
		devCtx, err = application.EnsureDevelopmentContext(ctx, store, boot)
		if err != nil {
			return err
		}
		// Bootstrap additional dev users for multi-session testing (Bug D fix).
		if extra := strings.TrimSpace(*devExtraUsers); extra != "" {
			for _, raw := range strings.Split(extra, ",") {
				uidStr := strings.TrimSpace(raw)
				if uidStr == "" {
					continue
				}
				uid, parseErr := strconv.ParseInt(uidStr, 10, 64)
				if parseErr != nil || uid <= 0 {
					return fmt.Errorf("invalid --dev-extra-users entry %q: %v", uidStr, parseErr)
				}
				extraBoot := boot
				extraBoot.UserID = uid
				extraBoot.Username = fmt.Sprintf("dev-user-%d", uid)
				if _, err := application.EnsureDevelopmentContext(ctx, store, extraBoot); err != nil {
					return fmt.Errorf("bootstrap extra user %d: %w", uid, err)
				}
			}
		}
		// Bootstrap a minimal dev team for team-session testing.
		if teamSpec := strings.TrimSpace(*devTeam); teamSpec != "" {
			teamCtx, err := parseAndEnsureDevTeam(ctx, store, teamSpec, boot.UserID)
			if err != nil {
				return err
			}
			fmt.Fprintf(os.Stderr, "platform: dev team %s session=%s\n", teamCtx.TeamName, teamCtx.SessionKey)
		}
		local, err := checkpoint.NewLocalCoordinator(checkpoint.LocalConfig{
			RuntimeRoot:        boot.RuntimeRoot,
			PlatformInstanceID: instanceID,
			Store:              store,
		})
		if err != nil {
			return err
		}
		coord = local
	} else {
		// 生产路径(方案 §5/§7): staging/committed 落在共享工作区卷的 state/ 内,
		// 由 Sandbox Runner 读写; 要求 GA_WORKSPACES_ROOT 提供共享卷根。
		if strings.TrimSpace(boot.WorkspacesRoot) == "" {
			return fmt.Errorf("GA_WORKSPACES_ROOT is required for production checkpoint coordinator")
		}
		if boot.UserID <= 0 {
			return fmt.Errorf("PLATFORM_DEV_USER_ID is required as the platform admin user id in production")
		}
		if strings.TrimSpace(boot.DevToken) == "" {
			return fmt.Errorf("PLATFORM_DEV_TOKEN is required as the admin API token in production")
		}
		// 平台管理员身份(管理端 persona/policy/invite 的 actor)。
		adminCtx, err := store.EnsurePlatformAdminUser(ctx, boot.UserID, boot.Username)
		if err != nil {
			return fmt.Errorf("ensure platform admin user: %w", err)
		}
		devCtx = adminCtx
		workspaceCoord, err := checkpoint.NewWorkspaceCoordinator(checkpoint.WorkspaceConfig{
			WorkspacesRoot:     boot.WorkspacesRoot,
			PlatformInstanceID: instanceID,
			Store:              store,
			RunnerStateMount:   "/ga/runner-state",
		})
		if err != nil {
			return fmt.Errorf("workspace checkpoint coordinator: %w", err)
		}
		coord = workspaceCoord
	}

	// Bot token cipher: required for bot registration, iLink transport, and
	// LLM provider API key encryption. The key is injected via env/flag and
	// never committed to source.
	var cipher secret.TokenCipher
	if keyHex := strings.TrimSpace(*botTokenKey); keyHex != "" {
		c, err := secret.NewStaticKeyCipherFromHex(keyHex)
		if err != nil {
			return fmt.Errorf("bot token cipher: %w", err)
		}
		cipher = c
	}

	// LLM Proxy: the sole holder of the real upstream key. In dev-loopback,
	// when --llm-proxy-addr is empty, an in-process Proxy is started on a free
	// loopback port. The Worker only ever receives the Proxy addr + a
	// short-lived capability_token (never the real key).
	signingKey := firstNonEmpty(*capabilitySigningKey, os.Getenv("LLM_PROXY_CAPABILITY_SIGNING_KEY"))
	proxyAddr, proxyShutdown, err := startLLMProxy(ctx, llmProxyConfig{
		externalAddr:         strings.TrimSpace(*llmProxyAddr),
		signingKey:           signingKey,
		providerSource:       store,
		cipher:               cipher,
		revocations:          store,
		allowedUpstreamCIDRs: llmproxy.ParseNetworkPolicyList(os.Getenv("LLM_PROXY_ALLOWED_UPSTREAM_CIDRS")),
		allowedHTTPHosts:     llmproxy.ParseNetworkPolicyList(os.Getenv("LLM_PROXY_ALLOW_HTTP_HOSTS")),
	})
	if err != nil {
		return err
	}
	defer proxyShutdown()

	issuer, err := llmproxy.NewIssuer([]byte(signingKey), llmproxy.DefaultTokenTTL)
	if err != nil {
		return fmt.Errorf("capability token issuer: %w", err)
	}

	runtime, err := buildWorkerRuntime(boot, store, instanceID, resolvedPolicyFile, proxyAddr, sophubProxyBaseURL())
	if err != nil {
		return err
	}
	sessionScopedConfig := true

	sched, err := application.NewScheduler(application.SchedulerConfig{
		PlatformInstanceID:    instanceID,
		ClaimLease:            *claimLease,
		PollInterval:          500 * time.Millisecond,
		Store:                 store,
		Registry:              reg,
		Coordinator:           coord,
		Runtime:               runtime,
		ConfigRoot:            boot.ConfigRoot,
		SessionScopedConfig:   sessionScopedConfig,
		RuntimeRoot:           boot.RuntimeRoot,
		LLMProxyAddr:          proxyAddr,
		SophubProxyBaseURL:    sophubProxyBaseURL(),
		TokenIssuer:           issuer,
		CapabilityStore:       store,
		Audit:                 store,
		ModelPolicyVersion:    strings.TrimSpace(*modelPolicyVersion),
		LLMProvider:           store,
		MCPServer:             store,
		TokenTTL:              llmproxy.DefaultTokenTTL,
		TokenRefreshSkew:      application.DefaultTokenRefreshSkew,
		MaxTaskWallClock:      time.Duration(*maxTaskWallClockSec) * time.Second,
		MaxRunningTasks:       *maxRunningTasks,
		PerTenantRunningLimit: *perTenantRunningLimit,
		TaskTimeoutSeconds:    *taskTimeoutSeconds,
		RuntimeSettings:       store,
		IdleTimeout:           time.Duration(*taskIdleTimeoutSec) * time.Second,
		WorkerIdleTTL:         time.Duration(*workerIdleTTLSec) * time.Second,
	})
	if err != nil {
		return err
	}

	// Recovery before accepting HTTP traffic.
	if err := sched.Recover(ctx, instanceID); err != nil {
		return fmt.Errorf("recover: %w", err)
	}

	svc, err := application.NewTaskService(application.TaskServiceConfig{
		Store:              store,
		Registry:           reg,
		Coordinator:        coord,
		PlatformInstanceID: instanceID,
		ClaimLease:         *claimLease,
		PerUserQueueLimit:  *perUserQueueLimit,
		Kick: func(ctx context.Context, sessionKey string) {
			_ = sched.KickSession(ctx, sessionKey)
		},
		CancelWorker: sched.CancelWorker,
	})
	if err != nil {
		return err
	}

	userSvc, err := application.NewUserService(application.UserServiceConfig{
		Store:        store,
		CancelWorker: sched.CancelWorker,
	})
	if err != nil {
		return err
	}

	botSvc, err := application.NewBotService(store)
	if err != nil {
		return err
	}

	var wechatBindingSvc application.WechatQRBindingService
	if ilinkBaseURL := strings.TrimSpace(*ilinkBaseURL); ilinkBaseURL != "" {
		if cipher == nil {
			return fmt.Errorf("--ilink-base-url requires --bot-token-key/BOT_TOKEN_KEY")
		}
		ilinkClient, err := ilink.NewClient(ilink.ClientConfig{
			BaseURL:       ilinkBaseURL,
			AppID:         *ilinkAppID,
			ClientVersion: *ilinkClientVersion,
		})
		if err != nil {
			return fmt.Errorf("ilink client: %w", err)
		}
		wechatBindingSvc, err = application.NewWechatQRBindingService(application.WechatQRBindingConfig{
			Store:       store,
			BotStore:    store,
			ILinkClient: ilinkClient,
			Cipher:      cipher,
		})
		if err != nil {
			return fmt.Errorf("wechat qr binding service: %w", err)
		}
		fmt.Fprintf(os.Stderr, "platform: wechat qr binding enabled base_url=%s\n", ilinkBaseURL)
	}

	inviteSvc, err := application.NewInviteService(application.InviteServiceConfig{
		Store: store,
		Users: store,
	})
	if err != nil {
		return err
	}

	personaSvc, err := application.NewPersonaService(store)
	if err != nil {
		return err
	}

	var sophubSvc application.SophubService
	if cipher != nil {
		sophubSvc, err = application.NewSophubService(application.SophubServiceConfig{
			Store: store, Client: sophub.NewClient(), Cipher: cipher,
		})
		if err != nil {
			return fmt.Errorf("Sophub service: %w", err)
		}
	}

	// Bot transport + lifecycle: when iLink is configured, the Go platform
	// delegates all iLink protocol I/O to the Python Bot Poller (which reuses
	// GA Core's verified WxBotClient). Go owns encryption + persistence; the
	// Poller owns getupdates/sendmessage. Without iLink, an in-process
	// loopback transport is used for dev/test.
	var botTransport transport.BotTransportAdapter
	var botLifecycle application.BotLifecycleService
	var botPollerClient *poller.Client
	if pollerURL := strings.TrimSpace(*botPollerURL); pollerURL != "" {
		if cipher == nil {
			return fmt.Errorf("--bot-poller-url requires --bot-token-key/BOT_TOKEN_KEY")
		}
		webhookURL := strings.TrimSpace(*platformWebhookURL)
		if webhookURL == "" {
			webhookURL = fmt.Sprintf("http://%s/v1/im/webhook", *listen)
		}
		botPollerClient, err = poller.NewClient(pollerURL, strings.TrimSpace(*botPollerAPISecret))
		if err != nil {
			return fmt.Errorf("poller client: %w", err)
		}
		if err := botPollerClient.ConfigureInboundCoalescing(ctx, imInboundCoalesceWindowMS); err != nil {
			return fmt.Errorf("configure poller inbound coalescing: %w", err)
		}
		ilinkAdapter, err := transport.NewILinkAdapter(transport.ILinkAdapterConfig{
			Poller: botPollerClient,
		})
		if err != nil {
			return fmt.Errorf("ilink adapter: %w", err)
		}
		botTransport = ilinkAdapter
		botLifecycle, err = application.NewBotLifecycleService(application.BotLifecycleConfig{
			Store:              store,
			Cipher:             cipher,
			Poller:             botPollerClient,
			WebhookURL:         webhookURL,
			RestoreConcurrency: 4,
		})
		if err != nil {
			return fmt.Errorf("bot lifecycle service: %w", err)
		}
		fmt.Fprintf(os.Stderr, "platform: bot poller transport url=%s webhook=%s\n", pollerURL, webhookURL)
	} else {
		botTransport = transport.NewLoopbackTransport()
	}

	teamSvc, err := application.NewTeamService(store)
	if err != nil {
		return fmt.Errorf("team service: %w", err)
	}

	relaySvc, err := application.NewRelayService(application.RelayServiceConfig{
		Store:     store,
		Transport: botTransport,
		Audit:     store, // audit_events table (migration 0001) for metadata-only audit
	})
	if err != nil {
		return fmt.Errorf("relay service: %w", err)
	}

	var sessionFiles application.SessionFiles
	if *devLoopback {
		if strings.TrimSpace(boot.RuntimeRoot) != "" {
			sessionFiles, err = application.NewSessionFiles(boot.RuntimeRoot)
			if err != nil {
				return fmt.Errorf("session files: %w", err)
			}
		}
	} else {
		// 生产: 附件/输出经共享工作区卷 temp/ 与 Runner 互通(方案 §6)。
		sessionFiles, err = application.NewWorkspaceSessionFiles(boot.WorkspacesRoot)
		if err != nil {
			return fmt.Errorf("workspace session files: %w", err)
		}
	}

	routerSvc, err := application.NewRouter(application.RouterConfig{
		Store:           store,
		Tasks:           svc,
		Transport:       botTransport,
		Commands:        store, // DB-driven command registry (migration 0004)
		Messages:        store, // messages table (migration 0013)
		SessionFiles:    sessionFiles,
		ToolPolicy:      strings.TrimSpace(*modelPolicyVersion),
		SourceInstance:  instanceID,
		Teams:           teamSvc,  // P1 team lifecycle (migration 0016)
		Relay:           relaySvc, // P1 @username relay (migration 0017)
		ChannelBindings: store,    // channel_bindings canonical identity (migration 0037)
	})
	if err != nil {
		return err
	}

	server, err := api.NewServer(api.ServerConfig{
		Service:         svc,
		Users:           userSvc,
		WechatBinding:   wechatBindingSvc,
		BotService:      botSvc,
		Invite:          inviteSvc,
		Personas:        personaSvc,
		Router:          routerSvc,
		Registry:        reg,
		Policies:        store, // admin command/policy management (migration 0004)
		RuntimeSettings: store,
		Sophub:          sophubSvc,
		SophubValidator: func(ctx context.Context, token string) (llmproxy.CapabilityClaims, error) {
			sv, err := llmproxy.NewSophubValidator([]byte(signingKey), store)
			if err != nil {
				return llmproxy.CapabilityClaims{}, err
			}
			return sv.Validate(ctx, token)
		},
		Bots:         store,
		LLMProviders: store, // admin LLM provider management (migration 0007)
		MCPServers:   store, // global MCP server management (migration 0029)
		BotLifecycle: botLifecycle,
		TaskStats:    store,
		RuntimeProfile: api.RuntimeProfile{
			ClaimLeaseSeconds:         int((*claimLease) / time.Second),
			TokenTTLSeconds:           int(llmproxy.DefaultTokenTTL / time.Second),
			TokenRefreshSkewSeconds:   int(application.DefaultTokenRefreshSkew / time.Second),
			MaxTaskWallClockSeconds:   *maxTaskWallClockSec,
			TaskTimeoutSeconds:        *taskTimeoutSeconds,
			TaskIdleTimeoutSeconds:    *taskIdleTimeoutSec,
			WorkerIdleTTLSeconds:      *workerIdleTTLSec,
			MaxRunningTasks:           *maxRunningTasks,
			PerTenantRunningLimit:     *perTenantRunningLimit,
			PerUserQueueLimit:         *perUserQueueLimit,
			IMInboundCoalesceWindowMS: imInboundCoalesceWindowMS,
			AgentMaxTurns:             agentMaxTurns,
		},
		IMAggregationRuntime: botPollerClient,
		Cipher:               cipher,
		DevToken:             boot.DevToken,
		DevUserID:            devCtx.UserID,
		SessionKey:           devCtx.SessionKey,
		WebhookSecret:        strings.TrimSpace(*webhookSecret),
	})
	if err != nil {
		return err
	}

	// Delivery service: polls task terminal state and sends notifications.
	// It requires the cipher (to resolve/decrypt bot tokens) and a coordinator
	// that can read bounded result refs.
	var deliverySvc application.DeliveryService
	if cipher != nil && coord != nil {
		deliveryCfg := application.DeliveryServiceConfig{
			Store:        store,
			Tasks:        store,
			Bots:         store,
			Transport:    botTransport,
			Results:      coord,
			Messages:     store, // audit outbound replies (migration 0013)
			SessionFiles: sessionFiles,
			PollInterval: 2 * time.Second,
			ClaimLease:   30 * time.Second,
			RetryWindow:  5 * time.Minute,
		}
		deliverySvc, err = application.NewDeliveryService(deliveryCfg)
		if err != nil {
			return fmt.Errorf("delivery service: %w", err)
		}
	}

	schedulerDone := make(chan error, 1)
	go func() {
		schedulerDone <- sched.Run(ctx)
	}()
	if !*devLoopback {
		managerClient, clientErr := sandbox.NewManagerClient(
			strings.TrimSpace(os.Getenv("GA_MANAGER_ADDR")),
			strings.TrimSpace(os.Getenv("GA_MANAGER_SECRET")),
			"ga-runner",
		)
		if clientErr != nil {
			return fmt.Errorf("runner lease reaper manager client: %w", clientErr)
		}
		go runRunnerLeaseReaper(ctx, store, managerClient, instanceID, time.Minute)
	}
	if deliverySvc != nil {
		go func() {
			if err := deliverySvc.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
				slog.ErrorContext(ctx, "delivery service error", "error", err)
			}
		}()
	}

	// Re-register every active bound bot with the Bot Poller so inbound
	// message polling resumes after a platform restart. Failures are logged
	// inside the lifecycle service; one bad bot does not block startup.
	if botLifecycle != nil {
		if err := botLifecycle.RestoreActiveBots(ctx); err != nil {
			slog.ErrorContext(ctx, "bot lifecycle restore error", "error", err)
		}
	}

	fmt.Fprintf(os.Stderr, "platform: instance_id=%s listen=%s session=%s policy_digest=%s\n",
		instanceID, *listen, devCtx.SessionKey, reg.Digest())

	// Wrap the HTTP server with sd_notify so systemd Type=notify + WatchdogSec
	// can supervise this process. When not running under systemd (NOTIFY_SOCKET
	// unset), the wrapper is a pass-through that just calls serve().
	serveErr := systemd.ReadyAndServe(ctx, func() error {
		return api.ServeContext(ctx, *listen, server.Handler())
	})
	cancel()
	return finishPlatform(serveErr, schedulerDone, 15*time.Second)
}
