package worker

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/infrastructure/sandbox"
	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/infrastructure/workerclient"
)

// SandboxConfig carries the deployment-level inputs for the Runner runtime.
type SandboxConfig struct {
	Manager         sandbox.RunnerCLI
	CA              *PlatformCA
	WorkspaceRoot   string
	MemoryTemplate  string
	Image           string
	ContainerPrefix string
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
	if strings.TrimSpace(cfg.WorkspaceRoot) == "" {
		return nil, fmt.Errorf("SandboxConfig.WorkspaceRoot is required")
	}
	if cfg.ControlDialTimeout <= 0 {
		cfg.ControlDialTimeout = 60 * time.Second
	}
	return &SandboxWorkerRuntime{cfg: cfg}, nil
}

// Start creates (or reuses) the Runner for the session's workspace and dials
// its mTLS control endpoint. Generation fencing: the Runner lease generation
// is derived per workspace recreation; the workerEntry cache keyed by
// session_key (== workspace key) keeps one Runner per workspace.
func (r *SandboxWorkerRuntime) Start(ctx context.Context, req StartRequest) (*Instance, error) {
	workspaceKey := req.SessionKey // personal:<uid> / team:<tid> (spec §3)
	generation, err := r.resolveGeneration(ctx, workspaceKey)
	if err != nil {
		return nil, fmt.Errorf("resolve runner generation: %w", err)
	}

	runner, created, err := r.cfg.Manager.EnsureRunner(ctx, workspaceKey, generation)
	if err != nil {
		return nil, fmt.Errorf("ensure runner %s: %w", workspaceKey, err)
	}

	// 每 Runner 短期服务证书(绑 workspace_key + generation, 方案 §7)。
	// V1: 证书材料经 Manager 受控 identity 输入注入容器固定路径(任务 4 行 2
	// 的 SandboxManagerCreateRequest 字段), 此处先保证生成链路可用。
	_, err = r.cfg.CA.IssueRunnerCert(workspaceKey, generation, 24*time.Hour)
	if err != nil {
		return nil, fmt.Errorf("issue runner cert: %w", err)
	}
	clientCert, err := r.cfg.CA.IssuePlatformClientCert(24 * time.Hour)
	if err != nil {
		_ = r.cfg.Manager.Destroy(ctx, runner.Name)
		return nil, fmt.Errorf("issue platform client cert: %w", err)
	}

	// Runner 服务端通过容器内 runner-state 挂载读取证书(由 Manager 在创建时写入)。
	// 控制面监听 runner-control 网络内的固定地址。
	endpoint := fmt.Sprintf("%s:9443", runner.Name)
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
	}
	return &Instance{Client: client, InstID: fmt.Sprintf("runner-%s-g%d", workspaceKey, generation), Cleanup: cleanup}, nil
}

// resolveGeneration returns the current Runner lease generation for the
// workspace. V1: 1 until the persistent lease store is wired (task 5 wires
// generation from runner_leases; keep a stable mapping here so the Worker
// cache and certs agree).
func (r *SandboxWorkerRuntime) resolveGeneration(ctx context.Context, workspaceKey string) (uint64, error) {
	return 1, nil
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

// ensure net import used by helper signatures.
var _ = net.JoinHostPort
