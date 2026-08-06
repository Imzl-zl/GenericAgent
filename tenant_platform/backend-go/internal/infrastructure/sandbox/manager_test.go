package sandbox

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/domain"
)

// mustWorkspaceHash 测试辅助: 忽略校验错误的 hash(测试 key 均为合法格式)。
func mustWorkspaceHash(key string) string {
	h, _ := domain.WorkspaceDirHash(key)
	return h
}

// fakeCLI 记录调用。
type fakeCLI struct {
	createCalls int
	create      func(spec RunnerSpec) (Runner, error)
	destroy     func(name string) error
	destroyed   []string
	ensureCalls []string
	containers  []RunnerInfo // ListRunnerContainers 返回(模拟 Manager 重启后的存量容器)
	// managerManaged/containerExists 控制 DestroyRunner 的归属校验(审查 F7)。
	managerManaged  bool
	containerExists bool
	// runnerHash 是 RunnerWorkspaceHash 的返回值(模拟 Manager 重启后
	// 按容器 ID 销毁时从 label 恢复 workspace hash, 审查 R5-C6)。
	runnerHash string
	// runnerGen 是 RunnerGenerationLabel 的返回值(审查 C1/I6:
	// 销毁时按 generation 清理 config 子目录)。
	runnerGen uint64
	// inspectErr 控制 Inspect 返回值(模拟容器停止/配置漂移)。
	inspectErr error
	// inspectFailOn 指定第 N 次 Inspect 调用返回 inspectErr(0 表示不启用),
	// 用于区分"创建后校验成功、复用前校验失败"的时序。
	inspectFailOn int
	inspectCalls  int
}

func (f *fakeCLI) CreateAndStart(ctx context.Context, spec RunnerSpec) (Runner, error) {
	f.createCalls++
	if f.create != nil {
		return f.create(spec)
	}
	return Runner{ContainerID: "cid", Name: "ga-runner-" + spec.WorkspaceHash[:12] + "-g" + strconv.FormatUint(spec.Generation, 10)}, nil
}

// EnsureRunner 模拟 Manager 的同 generation 复用/跨 generation 替换。
func (f *fakeCLI) EnsureRunner(ctx context.Context, req EnsureRunnerRequest) (Runner, bool, error) {
	r, err := f.CreateAndStart(ctx, RunnerSpec{
		WorkspaceHash: mustWorkspaceHash(req.WorkspaceKey), Generation: req.Generation,
		Env: req.Env, ConfigFiles: req.ConfigFiles,
	})
	return r, true, err
}

func (f *fakeCLI) ListRunnerContainers(ctx context.Context, namePrefix string) ([]RunnerInfo, error) {
	return append([]RunnerInfo(nil), f.containers...), nil
}

func (f *fakeCLI) Destroy(ctx context.Context, name string) error {
	f.destroyed = append(f.destroyed, name)
	if f.destroy != nil {
		return f.destroy(name)
	}
	// 模拟真实 Docker: 容器被 rm 后 label 不可读(审查 R5-C6 顺序契约)。
	f.runnerHash = ""
	return nil
}

func (f *fakeCLI) Inspect(ctx context.Context, name string) error {
	f.inspectCalls++
	if f.inspectFailOn > 0 && f.inspectCalls == f.inspectFailOn {
		return f.inspectErr
	}
	return nil
}

func (f *fakeCLI) IsManagerRunner(ctx context.Context, idOrName string) (bool, error) {
	return f.managerManaged, nil
}

func (f *fakeCLI) ContainerExists(ctx context.Context, idOrName string) (bool, error) {
	return f.containerExists, nil
}

func (f *fakeCLI) IsRunnerContainer(ctx context.Context, idOrName string) (bool, error) {
	return true, nil
}

func (f *fakeCLI) RunnerWorkspaceHash(ctx context.Context, idOrName string) (string, bool, error) {
	if f.runnerHash == "" {
		return "", false, nil
	}
	return f.runnerHash, true, nil
}

func (f *fakeCLI) RunnerGenerationLabel(ctx context.Context, idOrName string) (uint64, bool, error) {
	if f.runnerGen == 0 {
		return 0, false, nil
	}
	return f.runnerGen, true, nil
}

func (f *fakeCLI) EnsureWorkspace(ctx context.Context, workspaceHash string) error {
	f.ensureCalls = append(f.ensureCalls, workspaceHash)
	return nil
}

func TestManagerEnsureRunnerReusesSameGeneration(t *testing.T) {
	ctx := context.Background()
	cli := &fakeCLI{}
	m := NewManager(ManagerConfig{CLI: cli, WorkspaceRoot: t.TempDir(), ContainerNamePrefix: "ga-runner"})

	first, created, err := m.EnsureRunner(ctx, EnsureRunnerRequest{WorkspaceKey: "personal:1", Generation: 1})
	if err != nil {
		t.Fatalf("EnsureRunner: %v", err)
	}
	if !created {
		t.Fatal("first ensure must create")
	}
	again, created, err := m.EnsureRunner(ctx, EnsureRunnerRequest{WorkspaceKey: "personal:1", Generation: 1})
	if err != nil {
		t.Fatalf("EnsureRunner reuse: %v", err)
	}
	if created {
		t.Fatal("same generation must reuse, not recreate")
	}
	if again.Name != first.Name {
		t.Fatalf("reuse changed runner: %s -> %s", first.Name, again.Name)
	}
	if cli.createCalls != 1 {
		t.Fatalf("expected 1 create, got %d", cli.createCalls)
	}
}

func TestManagerEnsureRunnerReplacesOnGenerationBump(t *testing.T) {
	ctx := context.Background()
	cli := &fakeCLI{}
	m := NewManager(ManagerConfig{CLI: cli, WorkspaceRoot: t.TempDir(), ContainerNamePrefix: "ga-runner"})

	first, _, err := m.EnsureRunner(ctx, EnsureRunnerRequest{WorkspaceKey: "personal:1", Generation: 1})
	if err != nil {
		t.Fatal(err)
	}
	next, created, err := m.EnsureRunner(ctx, EnsureRunnerRequest{WorkspaceKey: "personal:1", Generation: 2})
	if err != nil {
		t.Fatalf("EnsureRunner regen: %v", err)
	}
	if !created {
		t.Fatal("generation bump must create new runner")
	}
	if next.Name == first.Name {
		t.Fatal("generation bump must replace runner name")
	}
	if len(cli.destroyed) != 1 || cli.destroyed[0] != first.Name {
		t.Fatalf("old runner not destroyed: %v", cli.destroyed)
	}
	if cli.createCalls != 2 {
		t.Fatalf("expected 2 creates, got %d", cli.createCalls)
	}
}

// TestManagerDestroyRunnerCleansConfigAfterRestart 验证 Manager 重启后
// (进程内 map 为空)按容器 ID 销毁时, 从容器 label 恢复 workspace hash 并
// 清理 config/ 目录(审查 R5-C6: 短期私钥不得因 map 丢失而残留)。
func TestManagerDestroyRunnerCleansConfigAfterRestart(t *testing.T) {
	ctx := context.Background()
	hash, _ := domain.WorkspaceDirHash("personal:88")
	root := t.TempDir()
	configDir := filepath.Join(root, hash, "config", "g1")
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		t.Fatal(err)
	}
	secret := filepath.Join(configDir, "server.key")
	if err := os.WriteFile(secret, []byte("private-key-material"), 0o600); err != nil {
		t.Fatal(err)
	}
	cli := &fakeCLI{runnerHash: hash, runnerGen: 1, managerManaged: true, containerExists: true}
	m := NewManager(ManagerConfig{CLI: cli, WorkspaceRoot: root, ContainerNamePrefix: "ga-runner"})

	// 按容器 ID 销毁(map 无记录, 模拟重启)。
	if err := m.DestroyRunner(ctx, "some-container-id"); err != nil {
		t.Fatalf("DestroyRunner: %v", err)
	}
	if _, err := os.Stat(secret); !os.IsNotExist(err) {
		t.Fatalf("config secret must be cleaned after restart-scoped destroy (stat err=%v)", err)
	}
}

func TestManagerDestroyRunner(t *testing.T) {
	ctx := context.Background()
	// 本 Manager 创建的容器(manager label 匹配)。
	cli := &fakeCLI{managerManaged: true}
	m := NewManager(ManagerConfig{CLI: cli, WorkspaceRoot: t.TempDir(), ContainerNamePrefix: "ga-runner"})

	if err := m.DestroyRunner(ctx, "ga-runner-bbbbbbbbbbbb-g1"); err != nil {
		t.Fatalf("DestroyRunner: %v", err)
	}
	if len(cli.destroyed) != 1 || cli.destroyed[0] != "ga-runner-bbbbbbbbbbbb-g1" {
		t.Fatalf("destroyed = %v", cli.destroyed)
	}
}

// TestManagerEnsureRunnerRejectsGenerationRegression 验证旧 generation 的
// 迟到请求不能销毁更新的 Runner 并回退 generation(方案 §7 generation 墙)。
func TestManagerEnsureRunnerRejectsGenerationRegression(t *testing.T) {
	ctx := context.Background()
	cli := &fakeCLI{}
	m := NewManager(ManagerConfig{CLI: cli, WorkspaceRoot: t.TempDir(), ContainerNamePrefix: "ga-runner"})

	if _, _, err := m.EnsureRunner(ctx, EnsureRunnerRequest{WorkspaceKey: "personal:1", Generation: 2}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := m.EnsureRunner(ctx, EnsureRunnerRequest{WorkspaceKey: "personal:1", Generation: 1}); err == nil {
		t.Fatal("generation regression must be rejected")
	}
	if cli.createCalls != 1 {
		t.Fatalf("expected 1 create, got %d (stale request destroyed the newer runner)", cli.createCalls)
	}
	if len(cli.destroyed) != 0 {
		t.Fatalf("stale request destroyed runner(s): %v", cli.destroyed)
	}
}

// TestManagerIsRunnerName 验证命名模式校验。
func TestManagerIsRunnerName(t *testing.T) {
	m := NewManager(ManagerConfig{ContainerNamePrefix: "ga-runner"})
	hash, _ := domain.WorkspaceDirHash("personal:1")
	hash12 := hash[:12]
	for _, name := range []string{
		"ga-runner-" + hash12 + "-g1",
		"ga-runner-" + hash12 + "-g42",
	} {
		if !m.IsRunnerName(name) {
			t.Fatalf("%q must be accepted", name)
		}
	}
	for _, name := range []string{
		"postgres-1",
		"ga-runner-" + hash12 + "-g0",
		"ga-runner-" + strings.Repeat("x", 12) + "-g1",
		"ga-runner-" + hash + "-g1-extra",
		"ga-runner-123456-g1",
	} {
		if m.IsRunnerName(name) {
			t.Fatalf("%q must be rejected", name)
		}
	}
}

func TestManagerEnsureWorkspaceDerivesHashAndDelegates(t *testing.T) {
	ctx := context.Background()
	cli := &fakeCLI{}
	m := NewManager(ManagerConfig{CLI: cli, WorkspaceRoot: t.TempDir(), ContainerNamePrefix: "ga-runner"})

	if err := m.EnsureWorkspace(ctx, "personal:42"); err != nil {
		t.Fatalf("EnsureWorkspace: %v", err)
	}
	want, _ := domain.WorkspaceDirHash("personal:42")
	if len(cli.ensureCalls) != 1 || cli.ensureCalls[0] != want {
		t.Fatalf("EnsureWorkspace hash = %v, want [%s]", cli.ensureCalls, want)
	}
	if err := m.EnsureWorkspace(ctx, ""); err == nil {
		t.Fatal("EnsureWorkspace with empty key must fail")
	}
}

// TestManagerEnsureRunnerScansExistingContainersAfterRestart 验证 Manager
// 重启(内存 map 为空)后, EnsureRunner 按确定性名字前缀扫描同 workspace 的
// 存量容器: 同 generation 复用、旧 generation 销毁、更新 generation 拒绝。
// (审查: 否则旧容器继续存活挂载同一工作区, 与新容器并发读写破坏串行。)
func TestManagerEnsureRunnerScansExistingContainersAfterRestart(t *testing.T) {
	ctx := context.Background()
	hash, _ := domain.WorkspaceDirHash("personal:1")
	prefix := "ga-runner-" + hash[:12] + "-"
	cli := &fakeCLI{
		containers: []RunnerInfo{
			{Name: prefix + "g1", Running: true},
			{Name: prefix + "g2", Running: true},
		},
	}
	m := NewManager(ManagerConfig{CLI: cli, WorkspaceRoot: t.TempDir(), ContainerNamePrefix: "ga-runner"})

	// 请求 g3: 旧的 g1/g2(running)必须全部销毁。
	runner, created, err := m.EnsureRunner(ctx, EnsureRunnerRequest{WorkspaceKey: "personal:1", Generation: 3})
	if err != nil {
		t.Fatalf("EnsureRunner g3: %v", err)
	}
	if !created {
		t.Fatal("g3 must be created")
	}
	if runner.Name != prefix+"g3" {
		t.Fatalf("runner name = %s, want %s", runner.Name, prefix+"g3")
	}
	if len(cli.destroyed) != 2 {
		t.Fatalf("stale runners destroyed = %v, want [g1 g2]", cli.destroyed)
	}
}

// TestManagerEnsureRunnerFailsClosedWhenStaleDestroyFails 验证 Manager 重启
// 扫描路径(内存 map 为空)销毁旧 generation 失败必须 fail-closed(审查
// R5-C3): 旧容器仍挂载同一 workspace, 继续创建新容器会让两代并发写。
func TestManagerEnsureRunnerFailsClosedWhenStaleDestroyFails(t *testing.T) {
	ctx := context.Background()
	hash, _ := domain.WorkspaceDirHash("personal:77")
	prefix := "ga-runner-" + hash[:12] + "-"
	cli := &fakeCLI{
		containers: []RunnerInfo{{Name: prefix + "g1", Running: true}},
		destroy:    func(name string) error { return os.ErrPermission },
	}
	m := NewManager(ManagerConfig{CLI: cli, WorkspaceRoot: t.TempDir(), ContainerNamePrefix: "ga-runner"})

	if _, _, err := m.EnsureRunner(ctx, EnsureRunnerRequest{WorkspaceKey: "personal:77", Generation: 2}); err == nil {
		t.Fatal("EnsureRunner must fail when stale generation destroy fails")
	}
	if cli.createCalls != 0 {
		t.Fatalf("createCalls = %d, want 0 (must not create after stale destroy failure)", cli.createCalls)
	}
}

func TestManagerEnsureRunnerReusesExistingSameGenerationAfterRestart(t *testing.T) {
	ctx := context.Background()
	hash, _ := domain.WorkspaceDirHash("personal:1")
	prefix := "ga-runner-" + hash[:12] + "-"
	cli := &fakeCLI{
		containers: []RunnerInfo{{Name: prefix + "g2", Running: true}},
	}
	m := NewManager(ManagerConfig{CLI: cli, WorkspaceRoot: t.TempDir(), ContainerNamePrefix: "ga-runner"})
	runner, created, err := m.EnsureRunner(ctx, EnsureRunnerRequest{WorkspaceKey: "personal:1", Generation: 2})
	if err != nil {
		t.Fatalf("EnsureRunner g2: %v", err)
	}
	if created {
		t.Fatal("existing same-generation runner must be reused, not created")
	}
	if runner.Name != prefix+"g2" {
		t.Fatalf("runner name = %s, want %s", runner.Name, prefix+"g2")
	}
	if len(cli.destroyed) != 0 {
		t.Fatalf("no runner should be destroyed, got %v", cli.destroyed)
	}
}

func TestManagerEnsureRunnerRejectsNewerExistingGeneration(t *testing.T) {
	ctx := context.Background()
	hash, _ := domain.WorkspaceDirHash("personal:1")
	prefix := "ga-runner-" + hash[:12] + "-"
	cli := &fakeCLI{containers: []RunnerInfo{{Name: prefix + "g5", Running: true}}}
	m := NewManager(ManagerConfig{CLI: cli, WorkspaceRoot: t.TempDir(), ContainerNamePrefix: "ga-runner"})
	if _, _, err := m.EnsureRunner(ctx, EnsureRunnerRequest{WorkspaceKey: "personal:1", Generation: 3}); err == nil {
		t.Fatal("requesting a generation older than an existing runner must be rejected")
	}
}

func TestManagerDestroyRunnerRejectsForeignManager(t *testing.T) {
	ctx := context.Background()
	cli := &fakeCLI{managerManaged: false, containerExists: true}
	m := NewManager(ManagerConfig{CLI: cli, WorkspaceRoot: t.TempDir(), ContainerNamePrefix: "ga-runner"})
	// 命名模式匹配但 label 归属不符: 拒绝销毁(审查 F7)。
	err := m.DestroyRunner(ctx, "ga-runner-aaaaaaaaaaaa-g1")
	if err == nil {
		t.Fatal("expected refusal for foreign-manager runner")
	}
	if len(cli.destroyed) != 0 {
		t.Fatalf("destroy must not be issued: %v", cli.destroyed)
	}
}

func TestManagerDestroyRunnerIdempotentWhenMissing(t *testing.T) {
	ctx := context.Background()
	cli := &fakeCLI{managerManaged: false, containerExists: false}
	m := NewManager(ManagerConfig{CLI: cli, WorkspaceRoot: t.TempDir(), ContainerNamePrefix: "ga-runner"})
	// 容器已不存在: 幂等成功(审查 F6)。
	if err := m.DestroyRunner(ctx, "ga-runner-aaaaaaaaaaaa-g1"); err != nil {
		t.Fatalf("destroy missing runner must be idempotent: %v", err)
	}
}

func TestManagerDestroyRunnerClearsCacheAndConfig(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	hash, _ := domain.WorkspaceDirHash("personal:1")
	configDir := root + "/" + hash + "/config/g1"
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configDir+"/server.key", []byte("key"), 0o600); err != nil {
		t.Fatal(err)
	}
	cli := &fakeCLI{managerManaged: true, containerExists: true, runnerHash: hash, runnerGen: 1}
	m := NewManager(ManagerConfig{CLI: cli, WorkspaceRoot: root, ContainerNamePrefix: "ga-runner"})
	first, created, err := m.EnsureRunner(ctx, EnsureRunnerRequest{WorkspaceKey: "personal:1", Generation: 1})
	if err != nil || !created {
		t.Fatalf("ensure: created=%v err=%v", created, err)
	}
	if err := m.DestroyRunner(ctx, first.Name); err != nil {
		t.Fatalf("destroy: %v", err)
	}
	// 缓存已清理: 再次同 generation ensure 必须重新创建(而非复用已删容器)。
	_, created, err = m.EnsureRunner(ctx, EnsureRunnerRequest{WorkspaceKey: "personal:1", Generation: 1})
	if err != nil {
		t.Fatalf("re-ensure: %v", err)
	}
	if !created {
		t.Fatal("cache must be cleared after destroy")
	}
	// config 目录残留清理(审查 F11): 短期 mTLS 材料不跨容器生命周期。
	if _, err := os.Stat(configDir); !os.IsNotExist(err) {
		t.Fatalf("config dir must be removed after destroy: %v", err)
	}
}

// 审查 R5-I7: 同 generation 复用前 Inspect 发现容器已停止(ErrRunnerNotRunning)
// 时, 必须销毁重建而不是把死容器当作可用 Runner 返回。
func TestManagerEnsureRunnerReplacesStoppedContainer(t *testing.T) {
	ctx := context.Background()
	cli := &fakeCLI{inspectErr: ErrRunnerNotRunning, inspectFailOn: 2}
	m := NewManager(ManagerConfig{CLI: cli, WorkspaceRoot: t.TempDir(), ContainerNamePrefix: "ga-runner"})

	first, created, err := m.EnsureRunner(ctx, EnsureRunnerRequest{WorkspaceKey: "personal:1", Generation: 1})
	if err != nil {
		t.Fatalf("EnsureRunner: %v", err)
	}
	if !created {
		t.Fatal("first ensure must create")
	}
	// 容器停止后再次 Ensure: 不得复用, 销毁并重建。
	again, created, err := m.EnsureRunner(ctx, EnsureRunnerRequest{WorkspaceKey: "personal:1", Generation: 1})
	if err != nil {
		t.Fatalf("EnsureRunner after stop: %v", err)
	}
	if !created {
		t.Fatal("stopped container must be replaced, not reused")
	}
	if len(cli.destroyed) != 1 || cli.destroyed[0] != first.Name {
		t.Fatalf("destroyed = %v, want [%s]", cli.destroyed, first.Name)
	}
	if again.Name != first.Name {
		t.Fatalf("replacement name = %q, want deterministic %q (hash+generation 推导)", again.Name, first.Name)
	}
}

// 审查 R5-I6: 按容器 ID 销毁(非 RunnerName 模式)时, 必须通过 manager label
// 校验归属——只有 runner=true 标签的容器不得被本 Manager 销毁。
func TestManagerDestroyRunnerByIDRejectsForeignManager(t *testing.T) {
	ctx := context.Background()
	cli := &fakeCLI{managerManaged: false, containerExists: true}
	m := NewManager(ManagerConfig{CLI: cli, WorkspaceRoot: t.TempDir(), ContainerNamePrefix: "ga-runner"})

	if err := m.DestroyRunner(ctx, "some-container-id"); err == nil {
		t.Fatal("DestroyRunner by ID without manager label must fail")
	}
	if len(cli.destroyed) != 0 {
		t.Fatalf("foreign container must not be destroyed: %v", cli.destroyed)
	}
}

// TestManagerDestroyRunnerCleansOnlyOwnGenerationConfig 验证审查 C1/I6:
// config 按 generation 隔离后, DestroyRunner 的清理只删自己 generation 的
// config/g<gen> 子目录——旧 generation 容器销毁不得删除新 generation 已
// 写入的 mTLS 材料(否则新 Runner 丢失凭据/挂载已 unlink 目录)。
func TestManagerDestroyRunnerCleansOnlyOwnGenerationConfig(t *testing.T) {
	ctx := context.Background()
	cli := &fakeCLI{}
	root := t.TempDir()
	m := NewManager(ManagerConfig{CLI: cli, WorkspaceRoot: root, ContainerNamePrefix: "ga-runner"})
	hash := mustWorkspaceHash("personal:1")
	// 预写两个 generation 的 config(模拟 g1 遗留 + g2 已创建)。
	if err := os.MkdirAll(filepath.Join(root, hash, "config", "g1"), 0o770); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, hash, "config", "g2"), 0o770); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, hash, "config", "g1", "server.crt"), []byte("old"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, hash, "config", "g2", "server.crt"), []byte("new"), 0o640); err != nil {
		t.Fatal(err)
	}
	// 模拟 Manager 重启后按容器名销毁: label 提供 hash 与 generation。
	cli.runnerHash = hash
	cli.runnerGen = 1
	cli.managerManaged = true
	cli.containerExists = true
	if err := m.DestroyRunner(ctx, "ga-runner-"+hash[:12]+"-g1"); err != nil {
		t.Fatalf("DestroyRunner: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, hash, "config", "g1")); !os.IsNotExist(err) {
		t.Fatal("g1 config dir must be removed after destroy")
	}
	if _, err := os.Stat(filepath.Join(root, hash, "config", "g2", "server.crt")); err != nil {
		t.Fatal("g2 config must survive old-generation destroy")
	}
}

// TestManagerDestroyRunnerSerializesWithEnsureRunner 验证审查 C1/I6:
// DestroyRunner 与 EnsureRunner 共享 per-workspace 锁——并发时旧 generation
// 销毁清理不得与新建(写 config/g<new>)交错, 避免"旧删新"竞态。
func TestManagerDestroyRunnerSerializesWithEnsureRunner(t *testing.T) {
	ctx := context.Background()
	cli := &fakeCLI{}
	root := t.TempDir()
	m := NewManager(ManagerConfig{CLI: cli, WorkspaceRoot: root, ContainerNamePrefix: "ga-runner"})
	hash := mustWorkspaceHash("personal:1")
	if err := os.MkdirAll(filepath.Join(root, hash, "config", "g2"), 0o770); err != nil {
		t.Fatal(err)
	}
	cli.runnerHash = hash
	cli.runnerGen = 1
	cli.managerManaged = true
	cli.containerExists = true
	// 同一 workspace: 先创建 g2(写 config/g2), 再销毁 g1(只应删 config/g1)。
	if err := os.WriteFile(filepath.Join(root, hash, "config", "g2", "server.crt"), []byte("new-cert"), 0o640); err != nil {
		t.Fatal(err)
	}
	if _, _, err := m.EnsureRunner(ctx, EnsureRunnerRequest{
		WorkspaceKey: "personal:1", Generation: 2,
		ConfigFiles: map[string][]byte{"server.crt": []byte("new-cert")},
	}); err != nil {
		t.Fatalf("EnsureRunner g2: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(root, hash, "config", "g1"), 0o770); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, hash, "config", "g1", "server.crt"), []byte("old"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := m.DestroyRunner(ctx, "ga-runner-"+hash[:12]+"-g1"); err != nil {
		t.Fatalf("DestroyRunner: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, hash, "config", "g1")); !os.IsNotExist(err) {
		t.Fatal("g1 config dir must be removed")
	}
	if _, err := os.Stat(filepath.Join(root, hash, "config", "g2", "server.crt")); err != nil {
		t.Fatal("g2 config written by EnsureRunner must survive destroy of g1")
	}
}
