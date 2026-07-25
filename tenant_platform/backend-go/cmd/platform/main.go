// Command platform is the loopback-only foundation control plane.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
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
	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/checkpoint"
	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/llmproxy"
	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/policy"
	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/postgres"
	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/transport"
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

// llmProxyConfig carries LLM Proxy startup parameters. The real upstream key
// is injected via host env (preferred) and never persisted.
type llmProxyConfig struct {
	externalAddr    string // when non-empty, use external Proxy (no in-process start)
	upstreamBaseURL string // real upstream OpenAI-compatible base URL
	upstreamAPIKey  string // real upstream API key (host env preferred)
	signingKey      string // HMAC signing key for capability_tokens (>=16 bytes)
}

// startLLMProxy starts the in-process LLM Proxy when externalAddr is empty,
// or validates the external addr. Returns the Proxy base URL the Worker will
// call (e.g. "http://127.0.0.1:port") and a shutdown function.
func startLLMProxy(ctx context.Context, cfg llmProxyConfig) (string, func(), error) {
	if cfg.externalAddr != "" {
		return strings.TrimRight(cfg.externalAddr, "/"), func() {}, nil
	}
	if cfg.upstreamBaseURL == "" {
		return "", nil, fmt.Errorf("LLM Proxy upstream base URL required (use --llm-proxy-upstream-baseurl or LLM_PROXY_UPSTREAM_BASEURL)")
	}
	if cfg.upstreamAPIKey == "" {
		return "", nil, fmt.Errorf("LLM Proxy upstream API key required (use --llm-proxy-upstream-apikey or LLM_PROXY_UPSTREAM_APIKEY)")
	}
	if len(cfg.signingKey) < llmproxy.MinSigningKeyLen {
		return "", nil, fmt.Errorf("capability signing key must be at least %d bytes (use --capability-signing-key or LLM_PROXY_CAPABILITY_SIGNING_KEY)", llmproxy.MinSigningKeyLen)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", nil, fmt.Errorf("llm-proxy listen: %w", err)
	}
	proxyCfg := llmproxy.Config{
		Listen:          ln.Addr().String(),
		UpstreamBaseURL: cfg.upstreamBaseURL,
		UpstreamAPIKey:  cfg.upstreamAPIKey,
		SigningKey:      []byte(cfg.signingKey),
		TokenTTL:        llmproxy.DefaultTokenTTL,
	}
	srv, err := llmproxy.NewServer(proxyCfg)
	if err != nil {
		_ = ln.Close()
		return "", nil, fmt.Errorf("llm-proxy server: %w", err)
	}
	httpSrv := &http.Server{Handler: srv.Handler()}
	go func() { _ = httpSrv.Serve(ln) }()
	addr := "http://" + ln.Addr().String()
	fmt.Fprintf(os.Stderr, "platform: in-process llm-proxy listen=%s upstream=%s\n", ln.Addr().String(), cfg.upstreamBaseURL)
	shutdown := func() {
		shutCtx, shutCancel := context.WithTimeout(ctx, 5*time.Second)
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
	var (
		policyFile   = flag.String("policy-file", "", "path to capability policy manifest (required)")
		claimLease   = flag.Duration("claim-lease", 0, "positive claim lease duration (required)")
		devLoopback  = flag.Bool("dev-loopback", false, "enable development loopback bootstrap and local coordinator")
		listen       = flag.String("listen", "127.0.0.1:8080", "loopback listen address")
		databaseURL  = flag.String("database-url", "", "PostgreSQL URL (or DATABASE_URL)")
		migration    = flag.String("migration", "", "path to 0001_foundation.sql")
		runtimeRoot  = flag.String("runtime-root", "", "GA_RUNTIME_DIR for local coordinator/worker")
		configRoot   = flag.String("config-root", "", "GA_CONFIG_ROOT for token-only mykey.py")
		legacyRoot   = flag.String("legacy-root", "", "GA_LEGACY_ROOT")
		workerPython = flag.String("worker-python", "", "python interpreter for worker")
		workerSrc    = flag.String("worker-src", "", "path to worker-python/src")
		llmProxyAddr         = flag.String("llm-proxy-addr", "", "external LLM Proxy addr (e.g. http://127.0.0.1:8081); empty = start in-process Proxy in dev-loopback")
		llmProxyUpstreamURL  = flag.String("llm-proxy-upstream-baseurl", "", "real upstream OpenAI-compatible base URL the Proxy forwards to (or LLM_PROXY_UPSTREAM_BASEURL)")
		llmProxyUpstreamKey  = flag.String("llm-proxy-upstream-apikey", "", "real upstream API key; host env preferred (or LLM_PROXY_UPSTREAM_APIKEY)")
		capabilitySigningKey = flag.String("capability-signing-key", "", "HMAC signing key for capability_tokens (or LLM_PROXY_CAPABILITY_SIGNING_KEY); >=16 bytes")
		modelPolicyVersion   = flag.String("model-policy-version", "foundation.no-host-tools.v1", "model_policy_version stamped into capability_tokens")
		devExtraUsers = flag.String("dev-extra-users", "", "comma-separated extra dev user IDs to bootstrap with personal workspaces")
		devTeam      = flag.String("dev-team", "", "bootstrap a dev team: format 'name:owner_id:member_id,member_id,...'")
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

	// LLM Proxy: the sole holder of the real upstream key. In dev-loopback,
	// when --llm-proxy-addr is empty, an in-process Proxy is started on a free
	// loopback port. The Worker only ever receives the Proxy addr + a
	// short-lived capability_token (never the real key).
	proxyAddr, proxyShutdown, err := startLLMProxy(ctx, llmProxyConfig{
		externalAddr:   strings.TrimSpace(*llmProxyAddr),
		upstreamBaseURL: strings.TrimSpace(firstNonEmpty(*llmProxyUpstreamURL, os.Getenv("LLM_PROXY_UPSTREAM_BASEURL"))),
		upstreamAPIKey:  strings.TrimSpace(firstNonEmpty(*llmProxyUpstreamKey, os.Getenv("LLM_PROXY_UPSTREAM_APIKEY"))),
		signingKey:      firstNonEmpty(*capabilitySigningKey, os.Getenv("LLM_PROXY_CAPABILITY_SIGNING_KEY")),
	})
	if err != nil {
		return err
	}
	defer proxyShutdown()

	signingKey := firstNonEmpty(*capabilitySigningKey, os.Getenv("LLM_PROXY_CAPABILITY_SIGNING_KEY"))
	issuer, err := llmproxy.NewIssuer([]byte(signingKey), llmproxy.DefaultTokenTTL)
	if err != nil {
		return fmt.Errorf("capability token issuer: %w", err)
	}
	revoker := application.NewHTTPTokenRevoker(proxyAddr)

	sched, err := application.NewScheduler(application.SchedulerConfig{
		PlatformInstanceID: instanceID,
		ClaimLease:         *claimLease,
		PollInterval:       500 * time.Millisecond,
		Store:              store,
		Registry:           reg,
		Coordinator:        coord,
		PolicyFile:         resolvedPolicyFile,
		ConfigRoot:         boot.ConfigRoot,
		LegacyRoot:         boot.LegacyRoot,
		RuntimeRoot:        boot.RuntimeRoot,
		WorkerPython:       boot.WorkerPython,
		WorkerSrc:          boot.WorkerSrc,
		LLMProxyAddr:        proxyAddr,
		TokenIssuer:         issuer,
		TokenRevoker:        revoker,
		ModelPolicyVersion:  strings.TrimSpace(*modelPolicyVersion),
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

	bindingSvc, err := application.NewBindingService(application.BindingServiceConfig{
		Store: store,
	})
	if err != nil {
		return err
	}

	// LoopbackTransport: in-process mock for the foundation slice. The real
	// iLink BotTransportAdapter is deferred to Slice 3b (spec §7.3).
	loopback := transport.NewLoopbackTransport()
	routerSvc, err := application.NewRouter(application.RouterConfig{
		Store:          store,
		Binding:        bindingSvc,
		Tasks:          svc,
		Transport:      loopback,
		ToolPolicy:     strings.TrimSpace(*modelPolicyVersion),
		SourceInstance: instanceID,
	})
	if err != nil {
		return err
	}

	server, err := api.NewServer(api.ServerConfig{
		Service:    svc,
		Users:      userSvc,
		Binding:    bindingSvc,
		Router:     routerSvc,
		Registry:   reg,
		DevToken:   boot.DevToken,
		DevUserID:  devCtx.UserID,
		SessionKey: devCtx.SessionKey,
	})
	if err != nil {
		return err
	}

	schedulerDone := make(chan error, 1)
	go func() {
		schedulerDone <- sched.Run(ctx)
	}()

	fmt.Fprintf(os.Stderr, "platform: instance_id=%s listen=%s session=%s policy_digest=%s\n",
		instanceID, *listen, devCtx.SessionKey, reg.Digest())

	serveErr := api.ServeContext(ctx, *listen, server.Handler())
	cancel()
	return finishPlatform(serveErr, schedulerDone, 15*time.Second)
}
