// Command document-manager is the only deployment unit allowed to hold the
// container runtime permission for secure document jobs.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/application"
	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/infrastructure/document"
	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/infrastructure/postgres"
	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/infrastructure/systemd"
)

type documentManagerOptions struct {
	databaseURL         string
	owner               string
	workRoot            string
	runtimeBinary       string
	image               string
	seccompProfile      string
	uid                 int
	gid                 int
	memoryBytes         int64
	cpuPeriod           int64
	cpuQuota            int64
	pidsLimit           int64
	tmpfsBytes          int64
	deploymentMaxActive int
	claimLease          time.Duration
	heartbeatInterval   time.Duration
	pollInterval        time.Duration
	commandPollInterval time.Duration
	shutdownTimeout     time.Duration
	allowRootfulRuntime bool
	allowMutableImage   bool
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "document-manager: %v\n", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	opts, err := parseDocumentManagerArgs(args)
	if err != nil {
		return err
	}
	managerCfg, runtimeCfg, err := buildDocumentManagerConfig(opts)
	if err != nil {
		return err
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	pool, err := pgxpool.New(ctx, opts.databaseURL)
	if err != nil {
		return fmt.Errorf("connect postgres: %w", err)
	}
	defer pool.Close()
	if err := postgres.EnsureSchema(ctx, pool, postgres.DefaultMigrationPath()); err != nil {
		return fmt.Errorf("ensure schema: %w", err)
	}
	store, err := postgres.NewStore(pool, postgres.WithDocumentPoolDeploymentMaxActive(opts.deploymentMaxActive))
	if err != nil {
		return fmt.Errorf("create postgres store: %w", err)
	}
	runtime, err := document.NewDockerCLI(runtimeCfg)
	if err != nil {
		return fmt.Errorf("create document runtime: %w", err)
	}
	if err := runtime.VerifyHost(ctx); err != nil {
		return fmt.Errorf("verify document runtime host: %w", err)
	}
	managerCfg.Store = store
	managerCfg.Runtime = runtime
	manager, err := application.NewDocumentManager(managerCfg)
	if err != nil {
		return fmt.Errorf("create document manager: %w", err)
	}

	serve := func() error {
		fmt.Fprintf(os.Stderr, "document-manager: owner=%s runtime=%s work_root=%s image=%s\n", opts.owner, opts.runtimeBinary, opts.workRoot, opts.image)
		if err := manager.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
			return err
		}
		return nil
	}
	return systemd.ReadyAndServe(ctx, serve)
}

func parseDocumentManagerArgs(args []string) (documentManagerOptions, error) {
	var opts documentManagerOptions
	allowRootfulRuntime, err := envBool("DOCUMENT_MANAGER_ALLOW_ROOTFUL_RUNTIME", false)
	if err != nil {
		return opts, err
	}
	allowMutableImage, err := envBool("DOCUMENT_MANAGER_ALLOW_MUTABLE_IMAGE", false)
	if err != nil {
		return opts, err
	}
	opts.allowRootfulRuntime = allowRootfulRuntime
	opts.allowMutableImage = allowMutableImage
	fs := flag.NewFlagSet("document-manager", flag.ContinueOnError)
	fs.StringVar(&opts.databaseURL, "database-url", firstNonEmpty(os.Getenv("DATABASE_URL"), ""), "PostgreSQL URL (or DATABASE_URL)")
	fs.StringVar(&opts.owner, "owner", firstNonEmpty(os.Getenv("DOCUMENT_MANAGER_OWNER"), "document-manager"), "document manager owner/fencing identity")
	fs.StringVar(&opts.workRoot, "work-root", os.Getenv("DOCUMENT_MANAGER_WORK_ROOT"), "canonical manager-owned document slot root")
	fs.StringVar(&opts.runtimeBinary, "runtime-binary", firstNonEmpty(os.Getenv("DOCUMENT_MANAGER_RUNTIME_BINARY"), "docker"), "container runtime binary: docker or podman")
	fs.StringVar(&opts.image, "image", os.Getenv("DOCUMENT_MANAGER_IMAGE"), "fixed document image repository@sha256 digest")
	fs.StringVar(&opts.seccompProfile, "seccomp-profile", firstNonEmpty(os.Getenv("DOCUMENT_MANAGER_SECCOMP_PROFILE"), "builtin"), "confined seccomp profile: builtin or absolute file path")
	fs.IntVar(&opts.uid, "uid", envInt("DOCUMENT_MANAGER_UID", 1000), "non-root container UID")
	fs.IntVar(&opts.gid, "gid", envInt("DOCUMENT_MANAGER_GID", 1000), "non-root container GID")
	fs.Int64Var(&opts.memoryBytes, "memory-bytes", envInt64("DOCUMENT_MANAGER_MEMORY_BYTES", 128<<20), "container memory.max bytes")
	fs.Int64Var(&opts.cpuPeriod, "cpu-period", envInt64("DOCUMENT_MANAGER_CPU_PERIOD", 100000), "container CPU CFS period")
	fs.Int64Var(&opts.cpuQuota, "cpu-quota", envInt64("DOCUMENT_MANAGER_CPU_QUOTA", 50000), "container CPU CFS quota")
	fs.Int64Var(&opts.pidsLimit, "pids-limit", envInt64("DOCUMENT_MANAGER_PIDS_LIMIT", 64), "container pids.max")
	fs.Int64Var(&opts.tmpfsBytes, "tmpfs-bytes", envInt64("DOCUMENT_MANAGER_TMPFS_BYTES", 64<<20), "container /tmp tmpfs bytes")
	fs.IntVar(&opts.deploymentMaxActive, "deployment-max-active", envInt("DOCUMENT_POOL_MAX_ACTIVE_HARD", 1), "deployment hard ceiling for document pool max_active")
	fs.DurationVar(&opts.claimLease, "claim-lease", envDuration("DOCUMENT_MANAGER_CLAIM_LEASE", time.Minute), "document job claim lease")
	fs.DurationVar(&opts.heartbeatInterval, "heartbeat-interval", envDuration("DOCUMENT_MANAGER_HEARTBEAT_INTERVAL", 10*time.Second), "document job heartbeat interval")
	fs.DurationVar(&opts.pollInterval, "poll-interval", envDuration("DOCUMENT_MANAGER_POLL_INTERVAL", time.Second), "manager reconciliation poll interval")
	fs.DurationVar(&opts.commandPollInterval, "command-poll-interval", envDuration("DOCUMENT_MANAGER_COMMAND_POLL_INTERVAL", 250*time.Millisecond), "per-job command poll interval")
	fs.DurationVar(&opts.shutdownTimeout, "shutdown-timeout", envDuration("DOCUMENT_MANAGER_SHUTDOWN_TIMEOUT", 15*time.Second), "bounded shutdown timeout")
	fs.BoolVar(&opts.allowRootfulRuntime, "allow-rootful-runtime", opts.allowRootfulRuntime, "allow a rootful Docker daemon; intended only for the Compose test profile")
	fs.BoolVar(&opts.allowMutableImage, "allow-mutable-image", opts.allowMutableImage, "allow genericagent-document-tool:<tag>; intended only for the Compose test profile")
	if err := fs.Parse(args); err != nil {
		return opts, err
	}
	return opts, nil
}

func buildDocumentManagerConfig(opts documentManagerOptions) (application.ManagerConfig, document.DockerConfig, error) {
	if strings.TrimSpace(opts.databaseURL) == "" {
		return application.ManagerConfig{}, document.DockerConfig{}, fmt.Errorf("database URL is required")
	}
	if strings.TrimSpace(opts.owner) == "" {
		return application.ManagerConfig{}, document.DockerConfig{}, fmt.Errorf("owner is required")
	}
	if opts.deploymentMaxActive <= 0 {
		return application.ManagerConfig{}, document.DockerConfig{}, fmt.Errorf("deployment max_active must be positive")
	}
	if opts.claimLease <= 0 {
		return application.ManagerConfig{}, document.DockerConfig{}, fmt.Errorf("claim lease must be positive")
	}
	if opts.heartbeatInterval <= 0 || opts.claimLease <= opts.heartbeatInterval {
		return application.ManagerConfig{}, document.DockerConfig{}, fmt.Errorf("claim lease must be greater than heartbeat interval")
	}
	if opts.pollInterval <= 0 || opts.commandPollInterval <= 0 || opts.shutdownTimeout <= 0 {
		return application.ManagerConfig{}, document.DockerConfig{}, fmt.Errorf("poll, command poll, and shutdown intervals must be positive")
	}

	runtimeCfg := document.DockerConfig{
		Binary:              opts.runtimeBinary,
		Image:               opts.image,
		WorkRoot:            opts.workRoot,
		SeccompProfile:      opts.seccompProfile,
		UID:                 opts.uid,
		GID:                 opts.gid,
		MemoryBytes:         opts.memoryBytes,
		CPUPeriod:           opts.cpuPeriod,
		CPUQuota:            opts.cpuQuota,
		PIDsLimit:           opts.pidsLimit,
		TmpfsBytes:          opts.tmpfsBytes,
		Command:             []string{"/usr/local/bin/ga-document-tool", "idle"},
		AllowRootfulRuntime: opts.allowRootfulRuntime,
		AllowMutableImage:   opts.allowMutableImage,
	}
	if _, err := document.NewDockerCLI(runtimeCfg); err != nil {
		return application.ManagerConfig{}, document.DockerConfig{}, err
	}
	managerCfg := application.ManagerConfig{
		Owner:               strings.TrimSpace(opts.owner),
		Compiler:            application.FixedDocumentOperationCompiler{},
		WorkRoot:            opts.workRoot,
		ClaimLease:          opts.claimLease,
		PollInterval:        opts.pollInterval,
		HeartbeatInterval:   opts.heartbeatInterval,
		CommandPollInterval: opts.commandPollInterval,
		ShutdownTimeout:     opts.shutdownTimeout,
	}
	return managerCfg, runtimeCfg, nil
}

func firstNonEmpty(a, b string) string {
	if strings.TrimSpace(a) != "" {
		return a
	}
	return b
}

func envInt(name string, fallback int) int {
	value, ok := parseEnvInt64(name)
	if !ok {
		return fallback
	}
	return int(value)
}

func envInt64(name string, fallback int64) int64 {
	value, ok := parseEnvInt64(name)
	if !ok {
		return fallback
	}
	return value
}

func parseEnvInt64(name string) (int64, bool) {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return 0, false
	}
	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return 0, false
	}
	return value, true
}

func envBool(name string, fallback bool) (bool, error) {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return fallback, nil
	}
	value, err := strconv.ParseBool(raw)
	if err != nil {
		return false, fmt.Errorf("%s must be true or false: %w", name, err)
	}
	return value, nil
}

func envDuration(name string, fallback time.Duration) time.Duration {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return fallback
	}
	value, err := time.ParseDuration(raw)
	if err != nil {
		return fallback
	}
	return value
}
