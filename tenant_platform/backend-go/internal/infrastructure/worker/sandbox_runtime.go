package worker

import (
	"context"
	"crypto/tls"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/domain"
	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/infrastructure/sandbox"
	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/infrastructure/workerclient"
)

// minRunnerCertTTL 是 Runner mTLS 证书的最短有效期(round11 审查 M1):
// lease TTL 可能只有几十秒且无限续租, 证书签发后不轮换——长会话一旦
// gRPC 重连会因证书过期失败。mTLS 短期性由容器销毁重建 + generation 递增
// 保证, 证书 TTL 只需覆盖会话最长寿命(24h 足够覆盖任何受支持任务时长)。
const minRunnerCertTTL = 24 * time.Hour

// RunnerLeaseStore 是 Platform 侧持久 Runner lease 的最小接口
// (实现: postgres.Store; 方案 §7: generation fencing 与重启恢复)。
type RunnerLeaseStore interface {
	AcquireRunnerLease(ctx context.Context, runnerKey, owner string, leaseTTL time.Duration, maxActive int64) (domain.RunnerLease, bool, error)
	RenewRunnerLease(ctx context.Context, runnerKey, owner string, generation uint64, leaseTTL time.Duration) error
	AttachRunnerContainer(ctx context.Context, runnerKey, containerID string, generation uint64, owner string) error
	ReleaseRunnerLease(ctx context.Context, runnerKey, owner string, generation uint64) error
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
	// MaxActiveRunners 是全局活跃 Runner lease 上限(0 = 不限制)。
	// 创建新 lease 时在 DB 事务内原子校验(方案 §7 容量排队)。
	MaxActiveRunners int64
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
		ctx, workspaceKey, r.cfg.PlatformInstanceID, r.cfg.RunnerLeaseTTL, r.cfg.MaxActiveRunners)
	if err != nil {
		return nil, fmt.Errorf("acquire runner lease: %w", err)
	}
	generation := lease.Generation
	// fail-closed(审查): 已占用的容量在后续任何步骤失败时都必须立即释放,
	// 否则多工作区各失败一次即可占满全局容量直到 TTL 到期。释放带 generation
	// 条件, 不影响更新 generation 的 lease。
	releaseOnFailure := func() {
		_ = r.cfg.LeaseStore.ReleaseRunnerLease(context.Background(), workspaceKey, r.cfg.PlatformInstanceID, generation)
	}

	// 2. 旧 generation 容器接管清理: lease 过期被本实例接管时, 旧容器按
	//    lease 记录的 stale_container_id 销毁(best-effort; 找不到按孤儿由
	//    Manager 回收)。接管事务把旧 container_id 移入 stale_container_id,
	//    Platform 重启生成新 CA 后旧容器无法拨号, 必须销毁重建(审查 C6)。
	//    round9 审查: 销毁条件不再依赖 created——scheduler 先 ResolveGeneration
	//    (接管, created=true) 再调用 Start(同 owner 续租, created=false) 时,
	//    二次获取会丢失接管标记并跳过销毁, 旧容器继续挂载同一工作区产生
	//    双写; stale_container_id 在接管时写入且同 owner 续租不清理, 因此
	//    只要非空就无条件销毁(幂等, 对不存在容器成功), 失败 fail-closed。
	staleContainer := lease.StaleContainerID
	if staleContainer != "" {
		if err := r.cfg.Manager.Destroy(ctx, staleContainer); err != nil {
			// 审查 R5-C3: 旧 generation 容器销毁失败必须 fail-closed——旧
			// Runner 仍挂载同一 workspace 且可能写穿 memory/temp/state, 继续
			// 创建新容器会让两代并发写, 破坏串行执行与 generation fencing。
			// Destroy 对不存在容器幂等成功, 此处的错误即真实故障; 释放 lease
			// 归还容量, 由调度层将任务退回重试(下一轮重新接管/清理)。
			releaseOnFailure()
			return nil, fmt.Errorf("destroy stale runner %s: %w", staleContainer, err)
		}
	}

	// 3. 证书与容器名: 容器名由 workspace hash + generation 确定性推导,
	//    服务端证书 SAN 绑定该 DNS 名(runner-control 网络内拨号地址)。
	// round11 审查(M1): 证书 TTL 不得等于 lease TTL——lease 无限续租, 而
	// 证书签发后不轮换; 长会话(>lease TTL)一旦 gRPC 重连会因证书过期失败。
	// mTLS 的短期性由"容器销毁重建 + generation 递增"保证, 证书 TTL 只需
	// 覆盖会话最长寿命。
	runnerName := r.runnerName(workspaceKey, generation)
	certTTL := r.cfg.RunnerLeaseTTL
	if certTTL < minRunnerCertTTL {
		certTTL = minRunnerCertTTL
	}
	serverCert, err := r.cfg.CA.IssueRunnerCert(workspaceKey, generation, certTTL, runnerName)
	if err != nil {
		releaseOnFailure()
		return nil, fmt.Errorf("issue runner cert: %w", err)
	}
	clientCert, err := r.cfg.CA.IssuePlatformClientCert(certTTL)
	if err != nil {
		releaseOnFailure()
		return nil, fmt.Errorf("issue platform client cert: %w", err)
	}
	policy, err := os.ReadFile(r.cfg.PolicyFile)
	if err != nil {
		releaseOnFailure()
		return nil, fmt.Errorf("read policy file: %w", err)
	}
	configFiles := map[string][]byte{
		"server.crt":  serverCert.CertPEM,
		"server.key":  serverCert.KeyPEM,
		"ca.crt":      r.cfg.CA.CertPEM,
		"policy.json": policy,
	}
	// 运行时配置(mykey.runtime.json 等)随控制面材料一并注入容器 config/:
	// Runner 的 load_runtime_metadata 从 GA_CONFIG_ROOT 读取(方案 §7)。
	for name, data := range req.RuntimeConfigFiles {
		configFiles[name] = data
	}

	// 4. 创建/复用 Runner(Manager 独占 Docker)。
	runner, created, err := r.cfg.Manager.EnsureRunner(ctx, sandbox.EnsureRunnerRequest{
		WorkspaceKey: workspaceKey,
		Generation:   generation,
		Env:          r.cfg.Env,
		ConfigFiles:  configFiles,
	})
	if err != nil {
		releaseOnFailure()
		return nil, fmt.Errorf("ensure runner %s: %w", workspaceKey, err)
	}
	if created {
		// 绑定不可变容器 ID 到 lease(generation 条件更新, 防止旧容器名覆盖
		// 新 lease; 审查: lease 记录容器 ID 而非可推导容器名, 供定向清理)。
		// 审查 R4-I5: attach 失败必须 fail-closed——lease 已不存在、已被
		// 接管或 DB 故障时, 该容器无法被 lease 定向清理且可能属于旧
		// generation, 继续执行会让旧 Runner 拨号后写穿工作区。
		// 立即销毁容器并释放 lease, 不再继续。
		if err := r.cfg.LeaseStore.AttachRunnerContainer(ctx, workspaceKey, runner.ContainerID, generation, r.cfg.PlatformInstanceID); err != nil {
			destroyErr := r.cfg.Manager.Destroy(ctx, runner.Name)
			releaseOnFailure()
			if destroyErr != nil {
				slogWarn("sandbox runtime: destroy runner after attach failure", "runner", runner.Name, "error", destroyErr)
			}
			return nil, fmt.Errorf("attach runner container to lease: %w", err)
		}
	}

	// 5. 拨号控制面 mTLS(容器名:9443, runner-control 网络内 DNS)。
	endpoint := fmt.Sprintf("%s:%d", runner.Name, sandbox.RunnerControlPort)
	conn, err := r.dialMTLS(ctx, endpoint, clientCert)
	if err != nil {
		_ = r.cfg.Manager.Destroy(ctx, runner.Name)
		releaseOnFailure()
		return nil, fmt.Errorf("dial runner %s: %w", runner.Name, err)
	}
	client, err := workerclient.New(conn)
	if err != nil {
		_ = conn.Close()
		_ = r.cfg.Manager.Destroy(ctx, runner.Name)
		releaseOnFailure()
		return nil, err
	}

	// 续租 goroutine: 活跃期间周期刷新 lease(方案 §7), 防止 TTL 到期被
	// reaper 或其他实例接管; cleanup 时停止并等待退出。续租失败视为 lease
	// 丢失: 立即 fence(停止 Runner 并释放 lease), 防止已过期 Runner 继续
	// 执行任务(方案 §7 generation fencing; checkpoint 提交另有 lease 期限校验)。
	renewStop := make(chan struct{})
	renewDone := make(chan struct{})
	var cleanupOnce sync.Once
	// round9 审查: cleanup 无条件销毁容器(不再依赖 created)——复用容器在
	// 续租失败/取消/idle 回收时同样必须销毁, 否则 lease 已释放但容器继续
	// 挂载工作区并可能执行旧任务; 销毁按确定性容器名幂等, 失败不阻塞
	// lease 释放(下次 acquire 以 generation+1 重建, 旧容器由 Manager 扫描
	// 或下次接管兜底清理)。
	cleanup := func(capabilityJTI string) {
		cleanupOnce.Do(func() {
			close(renewStop)
			<-renewDone
			shutCtx, cancel := context.WithTimeout(context.Background(), workerShutdownTimeout)
			// 控制面身份 fencing(方案 §7): Shutdown 绑定 workspace/generation,
			// 并携带当前凭据集 JTI(审查 C1/I7: 生产会话有活跃 JTI 集时,
			// 空 JTI 会被 Worker 拒绝, 优雅关闭必然失败)。
			_ = client.Shutdown(shutCtx, workspaceKey, "scheduler-stop", generation, capabilityJTI)
			cancel()
			_ = conn.Close()
			if err := r.cfg.Manager.Destroy(context.Background(), runner.Name); err != nil {
				slogWarn("sandbox runtime: destroy runner on cleanup failed", "runner", runner.Name, "error", err)
			}
			// 释放 lease: 下次 acquire 以 generation+1 重建, 防旧容器复活。
			// 带 generation 条件, 旧 cleanup 无法释放新 generation 的 lease(审查 C6)。
			_ = r.cfg.LeaseStore.ReleaseRunnerLease(context.Background(), workspaceKey, r.cfg.PlatformInstanceID, generation)
		})
	}
	go func() {
		defer close(renewDone)
		ticker := time.NewTicker(r.cfg.RunnerLeaseTTL / 3)
		defer ticker.Stop()
		for {
			select {
			case <-renewStop:
				return
			case <-ticker.C:
				if err := r.cfg.LeaseStore.RenewRunnerLease(context.Background(), workspaceKey, r.cfg.PlatformInstanceID, generation, r.cfg.RunnerLeaseTTL); err != nil {
					slogWarn("sandbox runtime: renew runner lease failed; fencing runner", "runner_key", workspaceKey, "error", err)
					go cleanup("")
					return
				}
			}
		}
	}()
	return &Instance{Client: client, InstID: fmt.Sprintf("runner-%s-g%d", workspaceKey, generation), Cleanup: cleanup, RunnerGeneration: generation}, nil
}

// runnerName 与 Manager 的容器名推导保持一致。
func (r *SandboxWorkerRuntime) runnerName(workspaceKey string, generation uint64) string {
	hash, err := sandbox.WorkspaceDirHash(workspaceKey)
	if err != nil {
		// 调用方已用同一校验通过（Start 入口），此处兜底：空 hash 生成的名字
		// 不会匹配任何合法容器，后续 dial/attach 会失败并 fail-closed。
		return fmt.Sprintf("%s-invalid-%d", r.cfg.ContainerPrefix, generation)
	}
	return fmt.Sprintf("%s-%s-g%d", r.cfg.ContainerPrefix, hash[:12], generation)
}

// ResolveGeneration 由持久 lease 提供(方案 §7 generation fencing);
// 供 scheduler 在签发 per-task capability 前获取当前 generation。
func (r *SandboxWorkerRuntime) ResolveGeneration(ctx context.Context, workspaceKey string) (uint64, error) {
	lease, _, err := r.cfg.LeaseStore.AcquireRunnerLease(
		ctx, workspaceKey, r.cfg.PlatformInstanceID, r.cfg.RunnerLeaseTTL, r.cfg.MaxActiveRunners)
	if err != nil {
		return 0, err
	}
	return lease.Generation, nil
}

// ReleaseRunnerLease 释放本实例持有的 lease(审查: scheduler 初始化失败
// 路径归还容量)。generation 条件防止误释放更新 generation。
func (r *SandboxWorkerRuntime) ReleaseRunnerLease(ctx context.Context, workspaceKey string, generation uint64) error {
	return r.cfg.LeaseStore.ReleaseRunnerLease(ctx, workspaceKey, r.cfg.PlatformInstanceID, generation)
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
