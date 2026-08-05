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
// 任务即进程(决策 D1)下容器只活一个任务, 证书 TTL 只需覆盖任务时长;
// 24h 为保守上限(覆盖任何受支持任务墙钟), 短期性由任务终态容器销毁 +
// generation 递增保证, 证书签发后不轮换。
const minRunnerCertTTL = 24 * time.Hour

// managerControlTimeout 是 cleanup 中 Manager 控制面调用与 lease 释放的
// 单次超时(round12 审查 I3): 半开连接不得让任务收尾/Platform 关闭无限阻塞。
const managerControlTimeout = 10 * time.Second

// RunnerLeaseStore 是 Platform 侧持久 Runner lease 的最小接口
// (实现: postgres.Store; 方案 §7: generation fencing 与重启恢复)。
type RunnerLeaseStore interface {
	AcquireRunnerLease(ctx context.Context, runnerKey, owner string, leaseTTL time.Duration, maxActive int64) (domain.RunnerLease, bool, error)
	RenewRunnerLease(ctx context.Context, runnerKey, owner string, generation uint64, leaseTTL time.Duration) error
	AttachRunnerContainer(ctx context.Context, runnerKey, containerID string, generation uint64, owner string) error
	ReleaseRunnerLease(ctx context.Context, runnerKey, owner string, generation uint64) error
	// ExpireRunnerLease 仅归还容量、保留容器引用: 失败路径(销毁旧容器失败/
	// 拨号失败/attach 失败)使用, 让下一轮接管能定向销毁仍挂载 workspace 的
	// 旧容器; 只有确认销毁成功才用 ReleaseRunnerLease 清空引用。
	ExpireRunnerLease(ctx context.Context, runnerKey, owner string, generation uint64) error
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
	// DialControl 覆盖控制面拨号(round12 审查 I3 测试注入): 为空时使用
	// mTLS 拨号(生产); 测试注入 bufconn 使 Start 成功路径可测。
	DialControl func(ctx context.Context, endpoint string, clientCert CertMaterial) (*grpc.ClientConn, error)
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

// Start 为任务创建全新 Runner 容器并拨号其 mTLS 控制端点(任务即进程,
// 决策 D1)。Generation fencing: Runner lease generation 持久化于
// runner_leases 并随重建单调递增; 崩溃恢复路径(Platform 重启/接管)按
// stale_container_id 销毁旧 generation 容器, 防旧容器复活。
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
	// 否则多工作区各失败一次即可占满全局容量直到 TTL 到期。
	// round13 审查(D3): 失败路径只归还容量(ExpireRunnerLease), 不清空
	// container_id/stale_container_id——销毁失败/未销毁时容器仍可能挂载
	// workspace, 引用必须保留给下一轮接管定向销毁, 否则新 generation 直接
	// 创建容器造成双写。context 带超时, 防止 DB 半开时无限阻塞。
	releaseOnFailure := func() {
		expireCtx, cancel := context.WithTimeout(context.Background(), managerControlTimeout)
		defer cancel()
		_ = r.cfg.LeaseStore.ExpireRunnerLease(expireCtx, workspaceKey, r.cfg.PlatformInstanceID, generation)
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

	// 4. 创建 Runner(Manager 独占 Docker)。任务即进程语义下正常路径每次
	//    都是新 generation 新容器; EnsureRunner 的"已存在同 generation 容器"
	//    分支只出现在崩溃恢复场景(Platform 重启后同 lease 续租), 此时复用
	//    是幂等安全的。
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
	var conn *grpc.ClientConn
	if r.cfg.DialControl != nil {
		conn, err = r.cfg.DialControl(ctx, endpoint, clientCert)
	} else {
		conn, err = r.dialMTLS(ctx, endpoint, clientCert)
	}
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
	// round12 审查(I3): renewCtx 使 cleanup 能取消在途续租调用——修复前
	// RenewRunnerLease 用 context.Background(), 卡在 DB 调用时 cleanup 的
	// <-renewDone 无限阻塞任务收尾与 Platform 关闭。
	renewStop := make(chan struct{})
	renewDone := make(chan struct{})
	renewCtx, renewCancel := context.WithCancel(context.Background())
	var cleanupOnce sync.Once
	// round9 审查: cleanup 无条件销毁容器(不再依赖 created)——复用容器在
	// 续租失败/取消/idle 回收时同样必须销毁, 否则 lease 已释放但容器继续
	// 挂载工作区并可能执行旧任务; 销毁按确定性容器名幂等, 失败不阻塞
	// lease 释放(下次 acquire 以 generation+1 重建, 旧容器由 Manager 扫描
	// 或下次接管兜底清理)。
	cleanup := func(capabilityJTI string) {
		cleanupOnce.Do(func() {
			renewCancel()
			close(renewStop)
			// round12 审查(I3): 带超时等待续租 goroutine 退出; 超时只记录
			// 日志, 收尾继续(容器销毁与 lease 释放是确定性动作, 不依赖
			// 续租 goroutine 是否已退出)。
			select {
			case <-renewDone:
			case <-time.After(workerShutdownTimeout):
				slogWarn("sandbox runtime: renewer did not stop in time; continuing shutdown", "runner_key", workspaceKey)
			}
			shutCtx, cancel := context.WithTimeout(context.Background(), workerShutdownTimeout)
			// 控制面身份 fencing(方案 §7): Shutdown 绑定 workspace/generation,
			// 并携带当前凭据集 JTI(审查 C1/I7: 生产会话有活跃 JTI 集时,
			// 空 JTI 会被 Worker 拒绝, 优雅关闭必然失败)。
			_ = client.Shutdown(shutCtx, workspaceKey, "scheduler-stop", generation, capabilityJTI)
			cancel()
			_ = conn.Close()
			// round12 审查(I3): 控制面与 DB 调用带明确超时, 防止半开连接
			// 让清理无限阻塞。
			destroyCtx, destroyCancel := context.WithTimeout(context.Background(), managerControlTimeout)
			destroyErr := r.cfg.Manager.Destroy(destroyCtx, runner.Name)
			destroyCancel()
			// round13 审查(D3): 容器引用清除必须与"确认销毁"绑定——Destroy
			// 成功才清空引用(ReleaseRunnerLease); 销毁失败保留引用并归还
			// 容量(ExpireRunnerLease), 下一轮 acquire 以 generation+1 接管时
			// 把 container_id 移入 stale_container_id 并先销毁再创建, 防止
			// 旧容器继续挂载 workspace 与新 generation 双写。
			releaseCtx, releaseCancel := context.WithTimeout(context.Background(), managerControlTimeout)
			if destroyErr != nil {
				slogWarn("sandbox runtime: destroy runner on cleanup failed; retaining container ref for takeover cleanup", "runner", runner.Name, "error", destroyErr)
				_ = r.cfg.LeaseStore.ExpireRunnerLease(releaseCtx, workspaceKey, r.cfg.PlatformInstanceID, generation)
			} else {
				// 释放 lease: 下次 acquire 以 generation+1 重建, 防旧容器复活。
				// 带 generation 条件, 旧 cleanup 无法释放新 generation 的 lease(审查 C6)。
				_ = r.cfg.LeaseStore.ReleaseRunnerLease(releaseCtx, workspaceKey, r.cfg.PlatformInstanceID, generation)
			}
			releaseCancel()
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
			case <-renewCtx.Done():
				return
			case <-ticker.C:
				if err := r.cfg.LeaseStore.RenewRunnerLease(renewCtx, workspaceKey, r.cfg.PlatformInstanceID, generation, r.cfg.RunnerLeaseTTL); err != nil {
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
