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
	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/infrastructure/documentgateway"
	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/infrastructure/ilink"
	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/infrastructure/llmproxy"
	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/infrastructure/logging"
	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/infrastructure/policy"
	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/infrastructure/poller"
	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/infrastructure/postgres"
	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/infrastructure/processguard"
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

// buildWorkerRuntime constructs the loopback Worker runtime. Podman/container
// mode was abandoned (single-instance multi-tenant deployment); loopback is
// the only supported runtime.
func buildWorkerRuntime(boot application.DevBootstrapConfig) (worker.WorkerRuntime, error) {
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

func buildDocumentGatewayIssuer(baseURL, signingKey string) (*documentgateway.Issuer, error) {
	if strings.TrimSpace(baseURL) == "" {
		return nil, nil
	}
	issuer, err := documentgateway.NewIssuer([]byte(signingKey), llmproxy.DefaultTokenTTL)
	if err != nil {
		return nil, fmt.Errorf("document gateway capability token issuer: %w", err)
	}
	return issuer, nil
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

// startLLMProxy starts the in-process LLM Proxy when externalAddr is empty,
// or validates the external addr. Returns the Proxy base URL the Worker will
// call (e.g. "http://127.0.0.1:port") and a shutdown function.
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
		policyFile                = flag.String("policy-file", "", "path to capability policy manifest (required)")
		claimLease                = flag.Duration("claim-lease", 0, "positive claim lease duration (required)")
		devLoopback               = flag.Bool("dev-loopback", false, "enable development loopback bootstrap and local coordinator")
		listen                    = flag.String("listen", "127.0.0.1:8080", "loopback listen address")
		databaseURL               = flag.String("database-url", "", "PostgreSQL URL (or DATABASE_URL)")
		migration                 = flag.String("migration", "", "path to 0001_foundation.sql")
		runtimeRoot               = flag.String("runtime-root", "", "GA_RUNTIME_DIR for local coordinator/worker")
		configRoot                = flag.String("config-root", "", "GA_CONFIG_ROOT for session-scoped token-only runtime configuration")
		legacyRoot                = flag.String("legacy-root", "", "GA_LEGACY_ROOT")
		workerPython              = flag.String("worker-python", "", "python interpreter for worker")
		workerSrc                 = flag.String("worker-src", "", "path to worker-python/src")
		llmProxyAddr              = flag.String("llm-proxy-addr", "", "external LLM Proxy addr (e.g. http://127.0.0.1:8081); empty = start in-process Proxy in dev-loopback")
		capabilitySigningKey      = flag.String("capability-signing-key", "", "HMAC signing key for capability_tokens (or LLM_PROXY_CAPABILITY_SIGNING_KEY); >=32 bytes")
		documentGatewayBaseURL    = flag.String("document-gateway-base-url", os.Getenv("DOCUMENT_GATEWAY_BASE_URL"), "loopback document gateway base URL (or DOCUMENT_GATEWAY_BASE_URL); empty disables Worker document capability")
		modelPolicyVersion        = flag.String("model-policy-version", "foundation.no-host-tools.v1", "model_policy_version stamped into capability_tokens")
		devExtraUsers             = flag.String("dev-extra-users", "", "comma-separated extra dev user IDs to bootstrap with personal workspaces")
		devTeam                   = flag.String("dev-team", "", "bootstrap a dev team: format 'name:owner_id:member_id,member_id,...'")
		botTokenKey               = flag.String("bot-token-key", os.Getenv("BOT_TOKEN_KEY"), "AES-256-GCM hex key for encrypting bot tokens (or BOT_TOKEN_KEY)")
		ilinkBaseURL              = flag.String("ilink-base-url", os.Getenv("ILINK_BASE_URL"), "iLink API base URL (or ILINK_BASE_URL); empty = loopback transport")
		ilinkAppID                = flag.String("ilink-app-id", firstNonEmpty(os.Getenv("ILINK_APP_ID"), "bot"), "iLink App-Id header")
		ilinkClientVersion        = flag.String("ilink-client-version", firstNonEmpty(os.Getenv("ILINK_CLIENT_VERSION"), "2.1.1"), "iLink App-ClientVersion header")
		botPollerURL              = flag.String("bot-poller-url", os.Getenv("BOT_POLLER_URL"), "Bot Poller HTTP base URL (or BOT_POLLER_URL); empty = loopback transport")
		botPollerAPISecret        = flag.String("bot-poller-api-secret", os.Getenv("BOT_POLLER_API_SECRET"), "HMAC-SHA256 secret for authenticating requests to Bot Poller /start /send /stop (or BOT_POLLER_API_SECRET); empty = unauthenticated (INSECURE - dev/test only)")
		platformWebhookURL        = flag.String("platform-webhook-url", os.Getenv("PLATFORM_WEBHOOK_URL"), "platform /v1/im/webhook URL told to the Bot Poller (or PLATFORM_WEBHOOK_URL)")
		webhookSecret             = flag.String("webhook-secret", os.Getenv("PLATFORM_WEBHOOK_SECRET"), "HMAC-SHA256 secret shared with the Bot Poller to authenticate /v1/im/webhook (or PLATFORM_WEBHOOK_SECRET); empty = unauthenticated (dev/test only)")
		maxRunningTasks           = flag.Int("max-running-tasks", envInt("MAX_RUNNING_TASKS", 0), "global cap on simultaneously starting/running tasks (or MAX_RUNNING_TASKS); 0 = disabled (dev/test)")
		perTenantRunningLimit     = flag.Int("per-tenant-running-limit", envInt("PER_TENANT_RUNNING_LIMIT", 0), "per-requester cap on simultaneously starting/running tasks across all sessions (or PER_TENANT_RUNNING_LIMIT); 0 = disabled (dev/test)")
		perUserQueueLimit         = flag.Int("per-user-queue-limit", envInt("PER_USER_QUEUE_LIMIT", 0), "per-requester cap on queued tasks (or PER_USER_QUEUE_LIMIT); 0 = disabled (dev/test)")
		taskTimeoutSeconds        = flag.Int("task-timeout-seconds", envInt("TASK_TIMEOUT_SECONDS", 0), "Worker-side wall-clock deadline for a whole task (or TASK_TIMEOUT_SECONDS); 0 = disabled (recommended; stuck detection uses gRPC stream errors + heartbeat lease loss instead). Set only when you want a hard task cap.")
		maxTaskWallClockSec       = flag.Int("max-task-wall-clock-seconds", envInt("MAX_TASK_WALL_CLOCK_SECONDS", 2700), "hard task wall-clock limit; capability TTL must cover this plus refresh skew")
		taskIdleTimeoutSec        = flag.Int("task-idle-timeout-seconds", envInt("TASK_IDLE_TIMEOUT_SECONDS", 300), "Idle reaper threshold (or TASK_IDLE_TIMEOUT_SECONDS). Default 300s (5min). A running task whose last_activity_at is older than this is finalized as WORKER_IDLE. Covers 'Worker alive but deadlocked' (GIL/hung I/O) — the scenario stream errors + lease loss cannot catch. Worker keeps last_activity_at fresh via chunk events + 30s heartbeats. 0 = disabled (dev/test only).")
		workerIdleTTLSec          = flag.Int("worker-idle-ttl-seconds", envInt("WORKER_IDLE_TTL_SECONDS", 600), "Worker eviction threshold (or WORKER_IDLE_TTL_SECONDS). Default 600s (10min). Idle Worker processes (no active task) older than this are torn down to reclaim memory (pattern: Kubernetes pod eviction + AWS Lambda container reuse window). Next task cold-starts from last snapshot. 0 = keep Workers resident indefinitely (dev/test or tiny fleets).")
		documentPoolMaxActiveHard = flag.Int("document-pool-max-active-hard", envInt("DOCUMENT_POOL_MAX_ACTIVE_HARD", 1), "deployment hard ceiling for administrator-managed document pool max_active")
	)
	flag.Parse()

	if strings.TrimSpace(*policyFile) == "" {
		return fmt.Errorf("--policy-file is required")
	}
	if *claimLease <= 0 {
		return fmt.Errorf("--claim-lease must be a positive duration")
	}
	if *documentPoolMaxActiveHard <= 0 {
		return fmt.Errorf("--document-pool-max-active-hard must be positive")
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

	store, err := postgres.NewStore(pool, postgres.WithDocumentPoolDeploymentMaxActive(*documentPoolMaxActiveHard))
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
	documentPoolSettings, err := store.GetDocumentPoolSettings(ctx)
	if err != nil {
		return fmt.Errorf("load document pool settings: %w", err)
	}
	documentPoolSettingsRuntime, err := application.NewDocumentPoolSettingsRuntime(documentPoolSettings, *documentPoolMaxActiveHard)
	if err != nil {
		return fmt.Errorf("initialize document pool settings runtime: %w", err)
	}
	documentPoolSettingsReconciler, err := application.NewDocumentPoolSettingsReconciler(
		store,
		documentPoolSettingsRuntime,
		application.DefaultDocumentPoolSettingsReconcileInterval,
	)
	if err != nil {
		return fmt.Errorf("initialize document pool settings reconciler: %w", err)
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
		// Normal startup rejects local coordinator and bootstrap.
		if boot.UserID != 0 && os.Getenv("PLATFORM_DEV_FORCE") == "" {
			// Still refuse EnsureDevelopmentContext path by not calling it.
		}
		return fmt.Errorf("foundation platform currently requires --dev-loopback (local coordinator); production path is out of scope for this slice")
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

	documentIssuer, err := buildDocumentGatewayIssuer(*documentGatewayBaseURL, signingKey)
	if err != nil {
		return err
	}
	var documentValidator *documentgateway.Validator
	var documentTools application.DocumentToolService
	if documentIssuer != nil {
		documentValidator, err = documentgateway.NewValidator([]byte(signingKey))
		if err != nil {
			return fmt.Errorf("document gateway capability validator: %w", err)
		}
		documentTools, err = application.NewDocumentToolService(application.DocumentToolServiceConfig{
			Store: store, Registry: reg,
		})
		if err != nil {
			return fmt.Errorf("document tool service: %w", err)
		}
	}

	runtime, err := buildWorkerRuntime(boot)
	if err != nil {
		return err
	}
	sessionScopedConfig := true

	sched, err := application.NewScheduler(application.SchedulerConfig{
		PlatformInstanceID:         instanceID,
		ClaimLease:                 *claimLease,
		PollInterval:               500 * time.Millisecond,
		Store:                      store,
		Registry:                   reg,
		Coordinator:                coord,
		Runtime:                    runtime,
		ConfigRoot:                 boot.ConfigRoot,
		SessionScopedConfig:        sessionScopedConfig,
		RuntimeRoot:                boot.RuntimeRoot,
		LLMProxyAddr:               proxyAddr,
		TokenIssuer:                issuer,
		CapabilityStore:            store,
		Audit:                      store,
		ModelPolicyVersion:         strings.TrimSpace(*modelPolicyVersion),
		LLMProvider:                store,
		MCPServer:                  store,
		DocumentGatewayBaseURL:     strings.TrimSpace(*documentGatewayBaseURL),
		DocumentGatewayTokenIssuer: documentIssuer,
		TokenTTL:                   llmproxy.DefaultTokenTTL,
		TokenRefreshSkew:           application.DefaultTokenRefreshSkew,
		MaxTaskWallClock:           time.Duration(*maxTaskWallClockSec) * time.Second,
		MaxRunningTasks:            *maxRunningTasks,
		PerTenantRunningLimit:      *perTenantRunningLimit,
		TaskTimeoutSeconds:         *taskTimeoutSeconds,
		RuntimeSettings:            store,
		IdleTimeout:                time.Duration(*taskIdleTimeoutSec) * time.Second,
		WorkerIdleTTL:              time.Duration(*workerIdleTTLSec) * time.Second,
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
	if strings.TrimSpace(boot.RuntimeRoot) != "" {
		sessionFiles, err = application.NewSessionFiles(boot.RuntimeRoot)
		if err != nil {
			return fmt.Errorf("session files: %w", err)
		}
	}

	routerSvc, err := application.NewRouter(application.RouterConfig{
		Store:          store,
		Tasks:          svc,
		Transport:      botTransport,
		Commands:       store, // DB-driven command registry (migration 0004)
		Messages:       store, // messages table (migration 0013)
		SessionFiles:   sessionFiles,
		ToolPolicy:     strings.TrimSpace(*modelPolicyVersion),
		SourceInstance: instanceID,
		Teams:          teamSvc,  // P1 team lifecycle (migration 0016)
		Relay:          relaySvc, // P1 @username relay (migration 0017)
	})
	if err != nil {
		return err
	}

	server, err := api.NewServer(api.ServerConfig{
		Service:                         svc,
		Users:                           userSvc,
		WechatBinding:                   wechatBindingSvc,
		BotService:                      botSvc,
		Invite:                          inviteSvc,
		Personas:                        personaSvc,
		Router:                          routerSvc,
		Registry:                        reg,
		Policies:                        store, // admin command/policy management (migration 0004)
		RuntimeSettings:                 store,
		DocumentPoolSettingsRuntime:     documentPoolSettingsRuntime,
		DocumentPoolStatus:              store,
		DocumentPoolDeploymentMaxActive: *documentPoolMaxActiveHard,
		DocumentTools:                   documentTools,
		DocumentCapabilityValidator:     documentValidator,
		Sophub:                          sophubSvc,
		Bots:                            store,
		LLMProviders:                    store, // admin LLM provider management (migration 0007)
		MCPServers:                      store, // global MCP server management (migration 0029)
		BotLifecycle:                    botLifecycle,
		TaskStats:                       store,
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
	go func() {
		if err := documentPoolSettingsReconciler.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
			slog.ErrorContext(ctx, "document pool settings reconciler stopped", "error", err)
		}
	}()

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
