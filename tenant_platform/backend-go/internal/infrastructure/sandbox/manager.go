package sandbox

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
)

// ContainerCLI 是 Manager 内部依赖的容器原语(docker create/start/rm/inspect)。
type ContainerCLI interface {
	CreateAndStart(ctx context.Context, spec RunnerSpec) (Runner, error)
	Destroy(ctx context.Context, name string) error
	Inspect(ctx context.Context, name string) error
	// IsRunnerContainer 校验容器 ID/名带 com.genericagent.runner=true label
	// (销毁前归属校验, 方案 §7: 控制面不能任意删除宿主任意容器)。
	IsRunnerContainer(ctx context.Context, idOrName string) (bool, error)
	// EnsureWorkspace 幂等地预置 workspace 目录布局(memory/temp/state/
	// config/attachments + session_files 子目录), 修复 ownership 与 setgid。
	// 供 Platform 在附件导入前经控制面调用(方案 §6: fresh workspace 首条
	// 带附件消息必须先有可写的共享卷目录)。
	EnsureWorkspace(ctx context.Context, workspaceHash string) error
}

// EnsureRunnerRequest 是受控的 Runner 创建请求。字段只来自认证的 Platform
// 控制面(workspace_key、generation、控制面环境与 mTLS 材料), 永不来自业务输入。
type EnsureRunnerRequest struct {
	WorkspaceKey string
	Generation   uint64
	// Env 是控制面透传环境(如 GA_LLM_PROXY_ADDR); 安全参数由 Manager 固定。
	Env []string
	// ConfigFiles 写入 workspace config/ 的文件(短期证书、策略清单)。
	ConfigFiles map[string][]byte
}

// RunnerCLI 是 SandboxWorkerRuntime(生产 Worker 创建路径)使用的完整生命周期面。
type RunnerCLI interface {
	ContainerCLI
	EnsureRunner(ctx context.Context, req EnsureRunnerRequest) (Runner, bool, error)
}

// ManagerConfig wires the Manager to its dependencies.
type ManagerConfig struct {
	CLI                 ContainerCLI
	WorkspaceRoot       string // host root containing workspaces/<hash>/
	MemoryTemplate      string // 镜像内只读模板路径(创建时传递给 CLI)
	ContainerNamePrefix string
	Image               string // 固定 digest
}

// Manager owns the Runner container lifecycle: create/reuse per generation,
// destroy, and post-create inspect. It never schedules business work and never
// accepts container parameters from business input (spec §6 组件边界).
type Manager struct {
	cfg ManagerConfig

	mu      sync.Mutex
	runners map[string]string      // workspaceHash -> containerName (活跃 Runner)
	locks   map[string]*sync.Mutex // workspaceHash -> per-workspace 创建锁
}

// NewManager builds a Manager.
func NewManager(cfg ManagerConfig) *Manager {
	if cfg.ContainerNamePrefix == "" {
		cfg.ContainerNamePrefix = "ga-runner"
	}
	return &Manager{cfg: cfg, runners: map[string]string{}, locks: map[string]*sync.Mutex{}}
}

// EnsureRunner returns the active Runner for a workspace generation, creating
// it on first use and replacing (destroying) the previous generation.
func (m *Manager) EnsureRunner(ctx context.Context, req EnsureRunnerRequest) (Runner, bool, error) {
	if req.Generation == 0 {
		return Runner{}, false, fmt.Errorf("runner generation must be positive")
	}
	if strings.TrimSpace(req.WorkspaceKey) == "" {
		return Runner{}, false, fmt.Errorf("workspace key is required")
	}
	hash := WorkspaceDirHash(req.WorkspaceKey)

	// per-workspace 锁: 防止并发 EnsureRunner 为同一工作区创建两个容器。
	m.mu.Lock()
	lock, ok := m.locks[hash]
	if !ok {
		lock = &sync.Mutex{}
		m.locks[hash] = lock
	}
	m.mu.Unlock()
	lock.Lock()
	defer lock.Unlock()

	m.mu.Lock()
	existing, hasExisting := m.runners[hash]
	m.mu.Unlock()

	if hasExisting && strings.HasSuffix(existing, "-g"+strconv.FormatUint(req.Generation, 10)) {
		// 同 generation 复用:校验后直接返回。
		if err := m.cfg.CLI.Inspect(ctx, existing); err != nil {
			return Runner{}, false, fmt.Errorf("inspect existing runner %s: %w", existing, err)
		}
		return Runner{Name: existing}, false, nil
	}

	// 内存 map 无该 workspace 记录(如 Manager 重启)时, 按确定性容器名前缀
	// 扫描同 workspace 的所有 Runner 容器(label 过滤), 恢复对旧 generation
	// 容器的认知(审查): 否则旧容器会继续存活并挂载同一工作区, 与新建容器
	// 并发读写 memory/temp/state, 破坏串行执行与 generation fencing。
	// 规则: 同 generation 校验后复用; 更新 generation 拒绝(回退防护);
	// 旧 generation 一律销毁(含 running, docker rm -f)。
	if !hasExisting {
		lister, ok := m.cfg.CLI.(interface {
			ListRunnerContainers(ctx context.Context, namePrefix string) ([]RunnerInfo, error)
		})
		if ok {
			prefix := m.cfg.ContainerNamePrefix + "-" + hash[:12] + "-"
			infos, listErr := lister.ListRunnerContainers(ctx, prefix)
			if listErr != nil {
				return Runner{}, false, fmt.Errorf("list runners for workspace %s: %w", hash, listErr)
			}
			for _, info := range infos {
				gen, parseErr := m.runnerGenerationOf(info.Name)
				if parseErr != nil {
					continue // 名称不匹配(理论上 label 过滤后不会出现)
				}
				switch {
				case gen == req.Generation:
					if err := m.cfg.CLI.Inspect(ctx, info.Name); err != nil {
						return Runner{}, false, fmt.Errorf("inspect existing runner %s: %w", info.Name, err)
					}
					m.mu.Lock()
					m.runners[hash] = info.Name
					m.mu.Unlock()
					return Runner{Name: info.Name}, false, nil
				case gen > req.Generation:
					return Runner{}, false, fmt.Errorf(
						"runner %s generation %d is newer than requested %d; refresh runner lease and retry",
						info.Name, gen, req.Generation)
				default:
					if err := m.cfg.CLI.Destroy(ctx, info.Name); err != nil {
						// 可能已被并发清理; 不阻断创建(创建后 inspect 兜底校验)。
						slog.WarnContext(ctx, "sandbox manager: destroy stale runner container failed",
							"name", info.Name, "error", err)
					}
				}
			}
		}
	}

	// 旧 generation 或首次:销毁旧的(如有),再创建新 Runner。
	if hasExisting {
		// generation 回退防护(方案 §7): 旧 lease 的迟到请求不得销毁
		// 更新的容器并回退 generation; 调用方应刷新 lease 后重试。
		if existingGen, err := m.runnerGenerationOf(existing); err == nil && existingGen > req.Generation {
			return Runner{}, false, fmt.Errorf(
				"runner %s generation %d is newer than requested %d; refresh runner lease and retry",
				existing, existingGen, req.Generation)
		}
		if err := m.cfg.CLI.Destroy(ctx, existing); err != nil {
			return Runner{}, false, fmt.Errorf("destroy stale runner %s: %w", existing, err)
		}
	}

	spec := RunnerSpec{
		WorkspaceKey:   req.WorkspaceKey,
		WorkspaceHash:  hash,
		Generation:     req.Generation,
		Image:          m.cfg.Image,
		MemoryTemplate: m.cfg.MemoryTemplate,
		Env:            req.Env,
		ConfigFiles:    req.ConfigFiles,
	}
	runner, err := m.cfg.CLI.CreateAndStart(ctx, spec)
	if err != nil {
		return Runner{}, false, err
	}
	if err := m.cfg.CLI.Inspect(ctx, runner.Name); err != nil {
		_ = m.cfg.CLI.Destroy(ctx, runner.Name)
		return Runner{}, false, fmt.Errorf("post-create inspect failed: %w", err)
	}

	m.mu.Lock()
	m.runners[hash] = runner.Name
	m.mu.Unlock()
	return runner, true, nil
}

// EnsureWorkspace 幂等地初始化工作区目录布局(供附件导入前置调用)。
// Manager 以 root 运行, 可 chown 目录到 Runner uid + 共享组并保留 setgid,
// 之后 Platform(组 10003 成员) 写入的附件/输出文件才能被 Runner 读取。
func (m *Manager) EnsureWorkspace(ctx context.Context, workspaceKey string) error {
	hash := WorkspaceDirHash(workspaceKey)
	if workspaceKey == "" {
		return fmt.Errorf("workspace key is required")
	}
	return m.cfg.CLI.EnsureWorkspace(ctx, hash)
}

// ListRunners 返回当前管理的 Runner 容器名(用于孤儿回收与状态巡检)。
func (m *Manager) ListRunners(ctx context.Context) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	names := make([]string, 0, len(m.runners))
	seen := map[string]bool{}
	for _, n := range m.runners {
		if !seen[n] {
			names = append(names, n)
			seen[n] = true
		}
	}
	return names, nil
}

// CreateAndStart 委托 CLI(实现 RunnerCLI 接口, 供 WorkerRuntime 使用)。
func (m *Manager) CreateAndStart(ctx context.Context, spec RunnerSpec) (Runner, error) {
	return m.cfg.CLI.CreateAndStart(ctx, spec)
}

// Inspect 委托 CLI(实现 RunnerCLI 接口)。
func (m *Manager) Inspect(ctx context.Context, name string) error {
	return m.cfg.CLI.Inspect(ctx, name)
}

// Destroy 委托 CLI(实现 RunnerCLI 接口)。
func (m *Manager) Destroy(ctx context.Context, name string) error {
	return m.DestroyRunner(ctx, name)
}

// DestroyRunner removes a Runner container by name or container ID.
// 归属校验: 必须匹配本 Manager 的命名模式, 或经 label 校验确认为 Runner。
func (m *Manager) DestroyRunner(ctx context.Context, name string) error {
	if !m.IsRunnerName(name) {
		ok, err := m.cfg.CLI.IsRunnerContainer(ctx, name)
		if err != nil || !ok {
			return fmt.Errorf("refusing to destroy %q: not a managed runner (label check failed: %v)", name, err)
		}
	}
	if err := m.cfg.CLI.Destroy(ctx, name); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for hash, n := range m.runners {
		if n == name {
			delete(m.runners, hash)
		}
	}
	return nil
}

// IsRunnerContainer 委托 CLI 校验容器 ID/名带 com.genericagent.runner=true
// label(销毁前归属校验, 方案 §7: 控制面不能任意删除宿主任意容器)。
func (m *Manager) IsRunnerContainer(ctx context.Context, idOrName string) (bool, error) {
	return m.cfg.CLI.IsRunnerContainer(ctx, idOrName)
}

// IsRunnerName 校验 name 是否符合本 Manager 的 Runner 命名模式
// <prefix>-<12hex>-g<generation>, 防止控制面凭据泄露后任意 rm 其他容器。
func (m *Manager) IsRunnerName(name string) bool {
	prefix := m.cfg.ContainerNamePrefix + "-"
	if !strings.HasPrefix(name, prefix) {
		return false
	}
	rest := strings.TrimPrefix(name, prefix)
	sep := strings.LastIndex(rest, "-g")
	if sep != 12 {
		return false
	}
	for _, c := range rest[:sep] {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
			return false
		}
	}
	gen, err := strconv.ParseUint(rest[sep+2:], 10, 64)
	return err == nil && gen > 0
}

// runnerGenerationOf 解析容器名后缀 -g<generation>。
func (m *Manager) runnerGenerationOf(name string) (uint64, error) {
	prefix := m.cfg.ContainerNamePrefix + "-"
	if !strings.HasPrefix(name, prefix) {
		return 0, fmt.Errorf("not a runner name")
	}
	rest := strings.TrimPrefix(name, prefix)
	sep := strings.LastIndex(rest, "-g")
	if sep < 0 {
		return 0, fmt.Errorf("missing generation suffix")
	}
	return strconv.ParseUint(rest[sep+2:], 10, 64)
}
