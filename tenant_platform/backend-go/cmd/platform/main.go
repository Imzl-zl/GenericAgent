// Command platform is the loopback-only foundation control plane.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/api"
	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/application"
	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/checkpoint"
	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/policy"
	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/postgres"
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
		configRoot   = flag.String("config-root", "", "GA_CONFIG_ROOT for worker mykey fixture")
		legacyRoot   = flag.String("legacy-root", "", "GA_LEGACY_ROOT")
		workerPython = flag.String("worker-python", "", "python interpreter for worker")
		workerSrc    = flag.String("worker-src", "", "path to worker-python/src")
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

	server, err := api.NewServer(api.ServerConfig{
		Service:    svc,
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
