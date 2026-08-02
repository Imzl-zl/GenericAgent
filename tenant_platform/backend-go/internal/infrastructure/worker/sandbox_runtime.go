package worker

import (
	"context"
	"crypto/tls"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/domain"
	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/infrastructure/sandbox"
	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/infrastructure/workerclient"
)

// RunnerLeaseStore 是 Platform 侧持久 Runner lease 的最小接口
// (实现: postgres.Store; 方案 §7: generation fencing 与重启恢复)。
type RunnerLeaseStore interface {
	AcquireRunnerLease(ctx context.Context, runnerKey, owner string, leaseTTL time.Duration) (domain.RunnerLease, bool, error)
	AttachRunnerContainer(ctx context.Context, runnerKey, containerID string) error
	ReleaseRunnerLease(ctx context.Context, runnerKey, owner string) error
}

// SandboxConfig carries the deployment-level inputs for the Runner runtime.
type SandboxConfig struct {
	Manager         sandbox.RunnerCLI
	CA              *PlatformCA
	LeaseStore      RunnerLeaseStore
	PlatformInstanceID string
	WorkspaceRoot   string
	MemoryTemplate  string
	Image           string
	ContainerPrefix string
	// PolicyFile 是 Platform 侧策略清单路径; 内容随证书一并注入容器 config/。
	PolicyFile string
	// Env 是控制面透传的额外容器环境(如 GA_LLM_PROXY_ADDR / GA_SOPHUB_PROXY_ADDR)。
	Env []string
	// RunnerLeaseTTL 是 Runner lease 时长(续租驱动 idle 回收)。
	RunnerLeaseTTL time.Duration
	// ControlDialTimeout bounds waiting for the Runner gRPC endpoint.
	ControlDialTimeout time.Duration
}

// SandboxWorkerRuntime creates a workspace Runner via the Sandbox Manager and
// dials its mTLS control endpoint (spec §7). It is the production replacement
// for LoopbackWorkerRuntime.
type SandboxWorkerRuntime struct {
	cfg SandboxConfig
}

// NewSandbox validates config and returns the production Runner runtime.
func NewSandbox(cfg SandboxConfig) (*SandboxWorkerRuntime, error) {
	if cfg.Manager == nil {
		return nil, fmt.Errorf("SandboxConfig.Manager is required")
	}
	if cfg.CA == nil {
		return nil, fmt.Errorf("SandboxConfig.CA is required")
	}
	if cfg.LeaseStore == nil {
		return nil, fmt.Errorf("SandboxConfig.LeaseStore is required")
	}
	if strings.TrimSpace(cfg.PlatformInstanceID) == "" {
		return nil, fmt.Errorf("SandboxConfig.PlatformInstanceID is required")
	}
	if strings.TrimSpace(cfg.WorkspaceRoot) == "" {
		return nil, fmt.Errorf("SandboxConfig.WorkspaceRoot is required")
	}
	if strings.TrimSpace(cfg.PolicyFile) == "" {
		return nil, fmt.Errorf("SandboxConfig.PolicyFile is required")
	}
	if cfg.ContainerPrefix == "" {
		cfg.ContainerPrefix = "ga-runner"
	}
	if cfg.RunnerLeaseTTL <= 0 {
		cfg.RunnerLeaseTTL = 30 * time.Minute
	}
	if cfg.ControlDialTimeout <= 0 {
		cfg.ControlDialTimeout = 60 * time.Second
	}
	return &SandboxWorkerRuntime{cfg: cfg}, nil
}

// Start creates (or reuses) the Runner for the session's workspace and dials
// its mTLS control endpoint. Generation fencing: the Runner lease generation
// is persisted in runner_leases and monotonically increases on recreation;
// the workerEntry cache keyed by session_key keeps one Runner per workspace.
func (r *SandboxWorkerRuntime) Start(ctx context.Context, req StartRequest) (*Instance, error) {
	workspaceKey := req.SessionKey // personal:<uid> / team:<tid> (spec §3)

	// 1. 持久 lease: 获取/续租 generation(owner = 本 Platform 实例)。
	lease, created, err := r.cfg.LeaseStore.AcquireRunnerLease(
		ctx, workspaceKey, r.cfg.PlatformInstanceID, r.cfg.RunnerLeaseTTL)
	if err != nil {
		return nil, fmt.Errorf("acquire runner lease: %w", err)
	}
	generation := lease.Generation

	// 2. 旧 generation 容器接管清理: lease 过期被本实例接管时, 旧容器按
	//    lease 记录的 container_id 销毁(best-effort; 找不到按孤儿由 Manager 回收)。
	if created && lease.ContainerID != "" {
		if err := r.cfg.Manager.Destroy(ctx, lease.ContainerID); err != nil {
			// 容器已不存在(重启后 sweep 已清理)属于预期, 不阻断创建。
			slogWarn("sandbox runtime: destroy stale runner best-effort", "container", lease.ContainerID, "error", err)
		}
	}

	// 3. 证书与容器名: 容器名由 workspace hash + generation 确定性推导,
	//    服务端证书 SAN 绑定该 DNS 名(runner-control 网络内拨号地址)。
	runnerName := r.runnerName(workspaceKey, generation)
	serverCert, err := r.cfg.CA.IssueRunnerCert(workspaceKey, generation, r.cfg.RunnerLeaseTTL, runnerName)
	if err != nil {
		return nil, fmt.Errorf("issue runner cert: %w", err)
	}
	clientCert, err := r.cfg.CA.IssuePlatformClientCert(r.cfg.RunnerLeaseTTL)
	if err != nil {
		return nil, fmt.Errorf("issue platform client cert: %w", err)
	}
	policy, err := os.ReadFile(r.cfg.PolicyFile)
	if err != nil {
		return nil, fmt.Errorf("read policy file: %w", err)
	}
	configFiles := map[string][]byte{
		"server.crt": serverCert.CertPEM,
		"server.key": serverCert.KeyPEM,
		"ca.crt":     r.cfg.CA.CertPEM,
		"policy.json": policy,
	}

	// 4. 创建/复用 Runner(Manager 独占 Docker)。
	runner, created, err := r.cfg.Manager.EnsureRunner(ctx, sandbox.EnsureRunnerRequest{
		WorkspaceKey: workspaceKey,
		Generation:   generation,
		Env:          r.cfg.Env,
		ConfigFiles:  configFiles,
	})
	if err != nil {
		return nil, fmt.Errorf("ensure runner %s: %w", workspaceKey, err)
	}
	if created {
		// 绑定容器名到 lease(重启接管时用于销毁旧容器)。
		if err := r.cfg.LeaseStore.AttachRunnerContainer(ctx, workspaceKey, runner.Name); err != nil {
			slogWarn("sandbox runtime: attach runner container to lease failed", "runner", runner.Name, "error", err)
		}
	}

	// 5. 拨号控制面 mTLS(容器名:9443, runner-control 网络内 DNS)。
	endpoint := fmt.Sprintf("%s:%d", runner.Name, sandbox.RunnerControlPort)
	conn, err := r.dialMTLS(ctx, endpoint, clientCert)
	if err != nil {
		_ = r.cfg.Manager.Destroy(ctx, runner.Name)
		return nil, fmt.Errorf("dial runner %s: %w", runner.Name, err)
	}
	client, err := workerclient.New(conn)
	if err != nil {
		_ = conn.Close()
		_ = r.cfg.Manager.Destroy(ctx, runner.Name)
		return nil, err
	}

	cleanup := func() {
		shutCtx, cancel := context.WithTimeout(context.Background(), workerShutdownTimeout)
		_ = client.Shutdown(shutCtx, "scheduler-stop")
		cancel()
		_ = conn.Close()
		if created {
			_ = r.cfg.Manager.Destroy(context.Background(), runner.Name)
		}
		// 释放 lease: 下次 acquire 以 generation+1 重建, 防旧容器复活。
		_ = r.cfg.LeaseStore.ReleaseRunnerLease(context.Background(), workspaceKey, r.cfg.PlatformInstanceID)
	}
	return &Instance{Client: client, InstID: fmt.Sprintf("runner-%s-g%d", workspaceKey, generation), Cleanup: cleanup, RunnerGeneration: generation}, nil
}

// runnerName 与 Manager 的容器名推导保持一致。
func (r *SandboxWorkerRuntime) runnerName(workspaceKey string, generation uint64) string {
	hash := sandbox.WorkspaceDirHash(workspaceKey)
	return fmt.Sprintf("%s-%s-g%d", r.cfg.ContainerPrefix, hash[:12], generation)
}

// resolveGeneration 由持久 lease 提供(方案 §7 generation fencing)。
// 保留方法以便调用方显式获取当前 generation(例如 task 下发)。
func (r *SandboxWorkerRuntime) resolveGeneration(ctx context.Context, workspaceKey string) (uint64, error) {
	lease, _, err := r.cfg.LeaseStore.AcquireRunnerLease(
		ctx, workspaceKey, r.cfg.PlatformInstanceID, r.cfg.RunnerLeaseTTL)
	if err != nil {
		return 0, err
	}
	return lease.Generation, nil
}

func (r *SandboxWorkerRuntime) dialMTLS(ctx context.Context, endpoint string, clientCert CertMaterial) (*grpc.ClientConn, error) {
	pair, err := tls.X509KeyPair(clientCert.CertPEM, clientCert.KeyPEM)
	if err != nil {
		return nil, fmt.Errorf("load client keypair: %w", err)
	}
	pool := r.cfg.CA.newCertPool()
	dialCtx, cancel := context.WithTimeout(ctx, r.cfg.ControlDialTimeout)
	defer cancel()
	conn, err := grpc.DialContext(dialCtx, endpoint,
		grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{
			Certificates: []tls.Certificate{pair},
			RootCAs:      pool,
			MinVersion:   tls.VersionTLS12,
		})),
		grpc.WithBlock(),
	)
	if err != nil {
		return nil, fmt.Errorf("dial %s: %w", endpoint, err)
	}
	return conn, nil
}

// slogWarn 精简日志封装(避免在关键路径上引入额外依赖)。
func slogWarn(msg string, keysAndValues ...any) {
	slog.Warn(msg, keysAndValues...)
}
