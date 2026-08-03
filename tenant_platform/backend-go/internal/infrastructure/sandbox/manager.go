package sandbox

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
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
	// RunnerWorkspaceHash 返回容器 label 中的 workspace hash(审查 R5-C6:
	// Manager 重启后按容器 ID 销毁时, 用它恢复 hash 以定位 config/ 清理
	// 目标——短期私钥不得因进程内 map 丢失而残留)。容器不存在返回 ok=false。
	RunnerWorkspaceHash(ctx context.Context, idOrName string) (hash string, ok bool, err error)
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
	hash, err := WorkspaceDirHash(req.WorkspaceKey)
	if err != nil {
		return Runner{}, false, fmt.Errorf("invalid workspace key: %w", err)
	}

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
			// 审查 R5-I7: 容器已停止/退出(ErrRunnerNotRunning)时不得复用——
			// 销毁重建; 其他 inspect 错误(配置漂移等)fail-closed, 由调度层
			// 退回重试。
			if errors.Is(err, ErrRunnerNotRunning) {
				if err := m.cfg.CLI.Destroy(ctx, existing); err != nil {
					return Runner{}, false, fmt.Errorf("destroy stopped runner %s: %w", existing, err)
				}
				m.mu.Lock()
				delete(m.runners, hash)
				m.mu.Unlock()
				hasExisting = false // 已销毁, 下方"旧 generation 或首次"分支不得二次销毁
			} else {
				return Runner{}, false, fmt.Errorf("inspect existing runner %s: %w", existing, err)
			}
		} else {
			return Runner{Name: existing}, false, nil
		}
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
						// 审查 R5-I7: 停止/退出的容器不得复用, 销毁重建。
						if errors.Is(err, ErrRunnerNotRunning) {
							if err := m.cfg.CLI.Destroy(ctx, info.Name); err != nil {
								return Runner{}, false, fmt.Errorf("destroy stopped runner %s: %w", info.Name, err)
							}
							continue
						}
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
						// 审查 R5-C3: 扫描路径销毁旧 generation 失败必须 fail-closed。
						// Destroy 对不存在容器幂等成功, 此处的错误即真实故障——旧
						// 容器仍挂载同一 workspace, 继续创建新容器会让两代并发写
						// memory/temp/state, 破坏串行执行与 generation fencing。
						return Runner{}, false, fmt.Errorf("destroy stale runner %s: %w", info.Name, err)
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
		// 销毁成功后立即清缓存(审查 F6): 后续创建失败时 map 不得指向已
		// 不存在的容器名——否则下次重试会反复销毁同一名字并因 not-found
		// 之外的错误卡住, 该工作区永久无法创建新 Runner。
		m.mu.Lock()
		delete(m.runners, hash)
		m.mu.Unlock()
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
		// 审查 R5-C6: create/start 中途失败(如 docker create 后 start 失败)
		// 必须清理已写入 workspace config/ 的短期私钥/证书/token——清理不能
		// 依赖进程内 map(下次创建会重建), 否则残留私钥随卷快照长期保存。
		m.cleanupWorkspaceConfig(hash)
		return Runner{}, false, err
	}
	if err := m.cfg.CLI.Inspect(ctx, runner.Name); err != nil {
		// 审查 R5-C6: 走 DestroyRunner(而非 CLI.Destroy)以执行归属校验与
		// workspace config/ 清理; 直接调 CLI.Destroy 会绕过清理, 让私钥残留。
		_ = m.DestroyRunner(ctx, runner.Name)
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
	if workspaceKey == "" {
		return fmt.Errorf("workspace key is required")
	}
	hash, err := WorkspaceDirHash(workspaceKey)
	if err != nil {
		return fmt.Errorf("invalid workspace key: %w", err)
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
// 归属校验(审查 F7): 命名模式匹配后仍须通过 manager label 校验——容器名
// 模式可被其他部署复用, 只有 label 才能证明是本 Manager 实例创建; 容器已
// 不存在时幂等成功(审查 F6)。
func (m *Manager) DestroyRunner(ctx context.Context, name string) error {
	if m.IsRunnerName(name) {
		if checker, ok := m.cfg.CLI.(interface {
			IsManagerRunner(ctx context.Context, idOrName string) (bool, error)
			ContainerExists(ctx context.Context, idOrName string) (bool, error)
		}); ok {
			managed, err := checker.IsManagerRunner(ctx, name)
			if err != nil {
				return fmt.Errorf("refusing to destroy %q: manager label check failed: %w", name, err)
			}
			if !managed {
				exists, existsErr := checker.ContainerExists(ctx, name)
				if existsErr != nil {
					return fmt.Errorf("refusing to destroy %q: existence check failed: %w", name, existsErr)
				}
				if !exists {
					return nil // 已不存在: 幂等成功
				}
				return fmt.Errorf("refusing to destroy %q: not created by this manager instance", name)
			}
		}
	} else {
		// 审查 R5-I6: 容器 ID 路径(非 RunnerName 模式)同样必须通过 manager
		// label 归属校验——只有 runner=true 标签不能证明是本 Manager 创建,
		// 其他部署/宿主任意容器的 runner=true 标签不得被销毁。
		if checker, ok := m.cfg.CLI.(interface {
			IsManagerRunner(ctx context.Context, idOrName string) (bool, error)
		}); ok {
			managed, err := checker.IsManagerRunner(ctx, name)
			if err != nil {
				return fmt.Errorf("refusing to destroy %q: manager label check failed: %w", name, err)
			}
			if !managed {
				return fmt.Errorf("refusing to destroy %q: not created by this manager instance", name)
			}
		} else {
			ok, err := m.cfg.CLI.IsRunnerContainer(ctx, name)
			if err != nil || !ok {
				return fmt.Errorf("refusing to destroy %q: not a managed runner (label check failed: %v)", name, err)
			}
		}
	}
	// 审查 F11/R5-C6: 短期 mTLS 私钥/证书/token 不跨容器生命周期残留——销毁
	// 容器后清理 workspace config/ 目录(下次创建由 CreateAndStart 重建)。
	// 进程内 map 无记录(Manager 重启后按名/ID 销毁)时, 从容器 label 恢复
	// workspace hash 定位清理目标。注意: 必须在 Destroy **之前**读取
	// label——容器被 rm 后 inspect 无法再返回任何信息。
	var hashFromLabel string
	if getter, ok := m.cfg.CLI.(interface {
		RunnerWorkspaceHash(ctx context.Context, idOrName string) (string, bool, error)
	}); ok {
		if h, found, err := getter.RunnerWorkspaceHash(context.WithoutCancel(ctx), name); err == nil && found {
			hashFromLabel = h
		}
	}
	if err := m.cfg.CLI.Destroy(ctx, name); err != nil {
		return err
	}
	m.mu.Lock()
	var hash string
	for h, n := range m.runners {
		if n == name {
			hash = h
			delete(m.runners, h)
		}
	}
	m.mu.Unlock()
	if hash == "" {
		hash = hashFromLabel
	}
	if hash != "" {
		m.cleanupWorkspaceConfig(hash)
	}
	return nil
}

// cleanupWorkspaceConfig 删除工作区 config/ 目录内容(best-effort)。
// 目录结构由下次 CreateAndStart 的 prepareWorkspaceDirs/writeConfigFiles
// 重建; 失败仅记日志, 不阻断销毁路径。
func (m *Manager) cleanupWorkspaceConfig(hash string) {
	if m.cfg.WorkspaceRoot == "" {
		return
	}
	dir := filepath.Join(m.cfg.WorkspaceRoot, hash, "config")
	if err := os.RemoveAll(dir); err != nil {
		slog.Warn("sandbox manager: cleanup workspace config dir failed",
			"workspace_hash", hash, "error", err)
	}
}

// RunnerWorkspaceHash 从容器 label 读取 workspace hash(审查 R5-C6:
// Platform 销毁路径按容器 ID 定位 config/ 清理目标)。
func (m *Manager) RunnerWorkspaceHash(ctx context.Context, idOrName string) (string, bool, error) {
	return m.cfg.CLI.RunnerWorkspaceHash(ctx, idOrName)
}

// ListRunnerContainers 列出本 Manager 创建的 Runner 容器(label 过滤,
// 含 stopped), 供孤儿回收使用(审查 R5-I7)。
func (m *Manager) ListRunnerContainers(ctx context.Context, namePrefix string) ([]RunnerInfo, error) {
	lister, ok := m.cfg.CLI.(interface {
		ListRunnerContainers(ctx context.Context, namePrefix string) ([]RunnerInfo, error)
	})
	if !ok {
		return nil, fmt.Errorf("CLI does not support listing runner containers")
	}
	return lister.ListRunnerContainers(ctx, namePrefix)
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
