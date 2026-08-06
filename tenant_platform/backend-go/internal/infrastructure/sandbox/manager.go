package sandbox

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/domain"
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
	hash, err := domain.WorkspaceDirHash(req.WorkspaceKey)
	if err != nil {
		return Runner{}, false, fmt.Errorf("invalid workspace key: %w", err)
	}

	// per-workspace 锁: 防止并发 EnsureRunner 为同一工作区创建两个容器。
	lock := m.workspaceLock(hash)
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
				// Round8: 持锁调用私有销毁变体(不得重入公开 DestroyRunner 死锁),
				// 并清理同 generation 的 config 短期材料。
				if err := m.destroyRunnerLocked(ctx, existing, hash, req.Generation); err != nil {
					return Runner{}, false, fmt.Errorf("destroy stopped runner %s: %w", existing, err)
				}
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
							if err := m.destroyRunnerLocked(ctx, info.Name, hash, gen); err != nil {
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
					// Round8: 持锁销毁并清理旧 generation config(原 CLI.Destroy 绕过清理)。
					if err := m.destroyRunnerLocked(ctx, info.Name, hash, gen); err != nil {
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
		existingGen, genErr := m.runnerGenerationOf(existing)
		if genErr == nil && existingGen > req.Generation {
			return Runner{}, false, fmt.Errorf(
				"runner %s generation %d is newer than requested %d; refresh runner lease and retry",
				existing, existingGen, req.Generation)
		}
		// Round8: 持锁销毁并清理旧 generation config(原 CLI.Destroy 绕过清理)。
		if err := m.destroyRunnerLocked(ctx, existing, hash, existingGen); err != nil {
			return Runner{}, false, fmt.Errorf("destroy stale runner %s: %w", existing, err)
		}
		// destroyRunnerLocked 已清缓存(审查 F6): 后续创建失败时 map 不得指向已
		// 不存在的容器名——否则下次重试会反复销毁同一名字并因 not-found
		// 之外的错误卡住, 该工作区永久无法创建新 Runner。
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
		// 必须清理已写入 workspace config/g<gen> 的短期私钥/证书/token
		// ——清理不能依赖进程内 map(下次创建会重建),
		// 否则残留私钥随卷快照长期保存(审查 C1/I6:
		// 按 generation 清理, 不影响已创建的新一代 config)。
		// round11 审查(I6): 清理失败返回组合错误, 残留由
		// ReconcileOrphanWorkspaceConfigs 兜底。
		if cleanupErr := m.cleanupWorkspaceConfig(hash, req.Generation); cleanupErr != nil {
			return Runner{}, false, errors.Join(err, fmt.Errorf("cleanup workspace config: %w", cleanupErr))
		}
		return Runner{}, false, err
	}
	if err := m.cfg.CLI.Inspect(ctx, runner.Name); err != nil {
		// 审查 R5-C6: 销毁并清理 workspace config/(短期私钥/证书/token)。
		// Round8: 必须调用持锁私有变体——公开 DestroyRunner 会再次获取
		// 本函数持有的 workspace 锁, 造成确定性死锁。
		_ = m.destroyRunnerLocked(ctx, runner.Name, hash, req.Generation)
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
	hash, err := domain.WorkspaceDirHash(workspaceKey)
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

// assertManagedRunner 校验 name/ID 属于本 Manager 的 Runner(方案 §7:
// 控制面凭据泄露后不得任意删除宿主任意容器; 容器已不存在时幂等成功)。
func (m *Manager) assertManagedRunner(ctx context.Context, name string) error {
	checker, ok := m.cfg.CLI.(interface {
		IsManagerRunner(ctx context.Context, idOrName string) (bool, error)
		ContainerExists(ctx context.Context, idOrName string) (bool, error)
	})
	if !ok {
		if !m.IsRunnerName(name) {
			ok, err := m.cfg.CLI.IsRunnerContainer(ctx, name)
			if err != nil || !ok {
				return fmt.Errorf("refusing to destroy %q: not a managed runner (label check failed: %v)", name, err)
			}
		}
		return nil
	}
	managed, err := checker.IsManagerRunner(ctx, name)
	if err != nil {
		return fmt.Errorf("refusing to destroy %q: manager label check failed: %w", name, err)
	}
	if managed {
		return nil
	}
	exists, existsErr := checker.ContainerExists(ctx, name)
	if existsErr != nil {
		return fmt.Errorf("refusing to destroy %q: existence check failed: %w", name, existsErr)
	}
	if !exists {
		return nil // 已不存在: 幂等成功(round10 B1)
	}
	return fmt.Errorf("refusing to destroy %q: not created by this manager instance", name)
}

// runnerLabels 在容器被删除**之前**读取 label 中的 workspace hash 与
// generation(审查 F11/R5-C6): 进程内 map 无记录(Manager 重启后按名/ID
// 销毁)时, 从 label 恢复清理目标; 容器被 rm 后 label 不可读。
func (m *Manager) runnerLabels(ctx context.Context, name string) (hash string, generation uint64) {
	if getter, ok := m.cfg.CLI.(interface {
		RunnerWorkspaceHash(ctx context.Context, idOrName string) (string, bool, error)
	}); ok {
		if h, found, err := getter.RunnerWorkspaceHash(context.WithoutCancel(ctx), name); err == nil && found {
			hash = h
		}
	}
	if genGetter, ok := m.cfg.CLI.(interface {
		RunnerGenerationLabel(ctx context.Context, idOrName string) (uint64, bool, error)
	}); ok {
		if g, found, err := genGetter.RunnerGenerationLabel(context.WithoutCancel(ctx), name); err == nil && found {
			generation = g
		}
	}
	return hash, generation
}

// runnerInfo 查询单个 Runner 容器的当前状态(锁内使用, round11 审查 I3:
// 孤儿回收的"是否可回收"判定必须基于销毁时刻的最新状态, 而非扫描快照)。
func (m *Manager) runnerInfo(ctx context.Context, name string) (RunnerInfo, bool, error) {
	infos, err := m.ListRunnerContainers(ctx, name)
	if err != nil {
		return RunnerInfo{}, false, err
	}
	for _, i := range infos {
		if i.Name == name {
			return i, true, nil
		}
	}
	return RunnerInfo{}, false, nil
}

// DestroyRunnerIf 在 workspace 锁内重新读取容器当前状态, 仅当
// stillOrphan(基于最新状态)判定为真时销毁(round11 审查 I3):
//   - 消除扫描快照与销毁之间 created→running 的竞态(容器已启动时
//     最新状态为 running, stillOrphan 可拒绝回收);
//   - 活跃 Runner 的 absTTL 强杀同样以锁内状态与判定为准。
// 容器已不存在视为已满足条件(幂等成功)。返回是否销毁。
func (m *Manager) DestroyRunnerIf(ctx context.Context, name string, stillOrphan func(info RunnerInfo) bool) (bool, error) {
	if err := m.assertManagedRunner(ctx, name); err != nil {
		return false, err
	}
	hash, generation := m.runnerLabels(ctx, name)
	m.mu.Lock()
	for h, n := range m.runners {
		if n == name {
			hash = h
			break
		}
	}
	m.mu.Unlock()
	var lock *sync.Mutex
	if hash != "" {
		lock = m.workspaceLock(hash)
	}
	if lock != nil {
		lock.Lock()
		defer lock.Unlock()
	}
	if generation == 0 {
		if gen, err := m.runnerGenerationOf(name); err == nil {
			generation = gen
		}
	}
	info, found, err := m.runnerInfo(ctx, name)
	if err != nil {
		return false, fmt.Errorf("re-inspect runner %q before destroy: %w", name, err)
	}
	if !found {
		return true, nil // 已不存在: 幂等成功
	}
	if !stillOrphan(info) {
		return false, nil
	}
	return true, m.destroyRunnerLocked(ctx, name, hash, generation)
}

// DestroyRunner removes a Runner container by name or container ID.
// 归属校验(审查 F7): 命名模式匹配后仍须通过 manager label 校验——容器名
// 模式可被其他部署复用, 只有 label 才能证明是本 Manager 实例创建; 容器已
// 不存在时幂等成功(审查 F6)。
func (m *Manager) DestroyRunner(ctx context.Context, name string) error {
	if err := m.assertManagedRunner(ctx, name); err != nil {
		return err
	}
	// 审查 F11/R5-C6: 短期 mTLS 私钥/证书/token 不跨容器生命周期残留——销毁
	// 容器后清理 workspace config/g<generation> 目录(下次创建由
	// CreateAndStart 重建)。进程内 map 无记录(Manager 重启后按名/ID
	// 销毁)时, 从容器 label 恢复 workspace hash 定位清理目标。
	// 注意: 必须在 Destroy **之前**读取 label——容器被 rm 后
	// inspect 无法再返回任何信息。
	hashFromLabel, genFromLabel := m.runnerLabels(ctx, name)
	// 审查 C1/I6: 与 EnsureRunner 共享 per-workspace 锁——同一工作区的
	// 创建/销毁串行, 旧 generation 销毁的 config 清理不得与新创建写入
	// 交错(否则可删掉新一代的 mTLS 材料)。
	m.mu.Lock()
	var hash string
	for h, n := range m.runners {
		if n == name {
			hash = h
			break
		}
	}
	m.mu.Unlock()
	if hash == "" {
		hash = hashFromLabel
	}
	// 审查 C1/I6(收紧): get-or-create 取锁——若 DestroyRunner 在
	// EnsureRunner 创建锁之前读取了 nil, 同 generation 的销毁可能与
	// writeConfigFiles 交错。
	var lock *sync.Mutex
	if hash != "" {
		lock = m.workspaceLock(hash)
	}
	if lock != nil {
		lock.Lock()
		defer lock.Unlock()
	}
	generation := genFromLabel
	if generation == 0 {
		if gen, err := m.runnerGenerationOf(name); err == nil {
			generation = gen
		}
	}
	return m.destroyRunnerLocked(ctx, name, hash, generation)
}

// destroyRunnerLocked 是持锁销毁变体:调用方必须已持有 workspaceLock(hash)
// (hash 为空时无锁, 仅当 label 与 map 都无法定位 workspace)。Round8 审查:
// EnsureRunner 持锁期间销毁容器必须走本变体——公开 DestroyRunner 会再次
// 获取同一把非重入锁, 造成确定性死锁; 同时统一保证所有销毁路径清理
// workspace config/g<generation> 的短期 mTLS 材料。
// round11 审查(I6): config 清理失败不再静默——销毁返回组合错误, 残留由
// ReconcileOrphanWorkspaceConfigs 兜底回收。
func (m *Manager) destroyRunnerLocked(ctx context.Context, name, hash string, generation uint64) error {
	if err := m.cfg.CLI.Destroy(ctx, name); err != nil {
		return err
	}
	m.mu.Lock()
	for h, n := range m.runners {
		if n == name {
			hash = h
			delete(m.runners, h)
		}
	}
	m.mu.Unlock()
	if hash != "" {
		if err := m.cleanupWorkspaceConfig(hash, generation); err != nil {
			return fmt.Errorf("destroy runner %s: container removed but config cleanup failed: %w", name, err)
		}
	}
	return nil
}

// cleanupWorkspaceConfig 删除工作区 config/g<generation> 目录
// (审查 C1/I6: 按 generation 隔离, 不影响已创建的新一代配置)。目录结构由
// 下次 CreateAndStart 的 writeConfigFiles 重建。round11 审查(I6): 失败
// 返回 error 而非仅日志——短期私钥/证书/token 不得因清理失败永久残留,
// 残留由 ReconcileOrphanWorkspaceConfigs 对账回收。
func (m *Manager) cleanupWorkspaceConfig(hash string, generation uint64) error {
	if m.cfg.WorkspaceRoot == "" {
		return nil
	}
	if generation == 0 {
		// 无法定位 generation(名称/label 都不可用): 不删除,
		// 防止误删新一代配置。残留由下次创建覆盖 + 对账兜底。
		return nil
	}
	dir := filepath.Join(m.cfg.WorkspaceRoot, hash, "config", fmt.Sprintf("g%d", generation))
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("remove config dir %s: %w", dir, err)
	}
	return nil
}

// orphanConfigReconcileAge 是孤儿 config 目录回收的最小年龄: 覆盖
// writeConfigFiles→CreateAndStart 的窗口, 防止对账器与进行中的创建竞态
// 删掉即将被容器挂载的短期凭据。
const orphanConfigReconcileAge = time.Hour

// ReconcileOrphanWorkspaceConfigs 对账回收没有对应 Runner 容器的
// workspace config/g<generation> 目录(round11 审查 I6): 销毁/创建失败路径
// 的清理失败(或进程崩溃)会留下含短期私钥/证书/token 的目录, 必须兜底
// 删除。判定基于容器 label(workspace hash + generation)与目录 mtime 年龄。
func (m *Manager) ReconcileOrphanWorkspaceConfigs(ctx context.Context, namePrefix string) (int, error) {
	if m.cfg.WorkspaceRoot == "" {
		return 0, nil
	}
	containers, err := m.ListRunnerContainers(ctx, namePrefix)
	if err != nil {
		return 0, fmt.Errorf("reconcile configs: list runners: %w", err)
	}
	active := make(map[string]struct{}, len(containers))
	for _, c := range containers {
		if c.WorkspaceHash != "" && c.Generation > 0 {
			active[fmt.Sprintf("%s/g%d", c.WorkspaceHash, c.Generation)] = struct{}{}
		}
	}
	now := time.Now()
	removed := 0
	workspaceDirs, err := os.ReadDir(m.cfg.WorkspaceRoot)
	if err != nil {
		return 0, fmt.Errorf("reconcile configs: list workspaces root: %w", err)
	}
	for _, ws := range workspaceDirs {
		if !ws.IsDir() || !workspaceHashPattern.MatchString(ws.Name()) {
			continue
		}
		configRoot := filepath.Join(m.cfg.WorkspaceRoot, ws.Name(), "config")
		configDirs, err := os.ReadDir(configRoot)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return removed, fmt.Errorf("reconcile configs: list %s: %w", configRoot, err)
		}
		for _, g := range configDirs {
			if !g.IsDir() || !strings.HasPrefix(g.Name(), "g") {
				continue
			}
			gen, err := strconv.ParseUint(strings.TrimPrefix(g.Name(), "g"), 10, 64)
			if err != nil || gen == 0 {
				continue
			}
			key := fmt.Sprintf("%s/g%d", ws.Name(), gen)
			if _, ok := active[key]; ok {
				continue // 容器存活: 配置是活跃凭据
			}
			info, err := g.Info()
			if err != nil {
				return removed, fmt.Errorf("reconcile configs: stat %s: %w", g.Name(), err)
			}
			if now.Sub(info.ModTime()) < orphanConfigReconcileAge {
				continue // 创建中窗口: 保留
			}
			if err := os.RemoveAll(filepath.Join(configRoot, g.Name())); err != nil {
				return removed, fmt.Errorf("reconcile configs: remove %s: %w", g.Name(), err)
			}
			removed++
		}
	}
	return removed, nil
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

// ContainerExists 委托 CLI 判断容器(按 ID 或名称)是否存在
// (round10 审查 B1: 销毁路径区分"不存在=幂等成功"与"存在但非 Runner=拒绝")。
func (m *Manager) ContainerExists(ctx context.Context, idOrName string) (bool, error) {
	if checker, ok := m.cfg.CLI.(interface {
		ContainerExists(ctx context.Context, idOrName string) (bool, error)
	}); ok {
		return checker.ContainerExists(ctx, idOrName)
	}
	return false, fmt.Errorf("CLI does not support container existence checks")
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

// workspaceLock 返回工作区锁, 不存在时创建(审查 C1/I6:
// EnsureRunner 与 DestroyRunner 必须共享同一锁实例, 避免创建/销毁交错)。
func (m *Manager) workspaceLock(hash string) *sync.Mutex {
	m.mu.Lock()
	defer m.mu.Unlock()
	lock, ok := m.locks[hash]
	if !ok {
		lock = &sync.Mutex{}
		m.locks[hash] = lock
	}
	return lock
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
