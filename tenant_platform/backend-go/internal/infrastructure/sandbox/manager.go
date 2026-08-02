package sandbox

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
)

// ContainerCLI 是 Manager 内部依赖的容器原语(docker create/start/rm/inspect)。
type ContainerCLI interface {
	CreateAndStart(ctx context.Context, spec RunnerSpec) (Runner, error)
	Destroy(ctx context.Context, name string) error
	Inspect(ctx context.Context, name string) error
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
	runners map[string]string // workspaceHash -> containerName (活跃 Runner)
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

	// 旧 generation 或首次:销毁旧的(如有),再创建新 Runner。
	if hasExisting {
		if err := m.cfg.CLI.Destroy(ctx, existing); err != nil {
			return Runner{}, false, fmt.Errorf("destroy stale runner %s: %w", existing, err)
		}
	}

	spec := RunnerSpec{
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

// DestroyRunner removes a Runner container by name (workspace data preserved).
func (m *Manager) DestroyRunner(ctx context.Context, name string) error {
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
