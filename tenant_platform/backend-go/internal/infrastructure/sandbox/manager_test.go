package sandbox

import (
	"context"
	"strconv"
	"strings"
	"testing"
)

// fakeCLI 记录调用。
type fakeCLI struct {
	createCalls int
	create      func(spec RunnerSpec) (Runner, error)
	destroy     func(name string) error
	destroyed   []string
	ensureCalls []string
	containers  []RunnerInfo // ListRunnerContainers 返回(模拟 Manager 重启后的存量容器)
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
		WorkspaceHash: WorkspaceDirHash(req.WorkspaceKey), Generation: req.Generation,
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
	return nil
}

func (f *fakeCLI) Inspect(ctx context.Context, name string) error { return nil }

func (f *fakeCLI) IsRunnerContainer(ctx context.Context, idOrName string) (bool, error) {
	return true, nil
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

func TestManagerDestroyRunner(t *testing.T) {
	ctx := context.Background()
	cli := &fakeCLI{}
	m := NewManager(ManagerConfig{CLI: cli, WorkspaceRoot: t.TempDir(), ContainerNamePrefix: "ga-runner"})

	if err := m.DestroyRunner(ctx, "ga-runner-dead-g1"); err != nil {
		t.Fatalf("DestroyRunner: %v", err)
	}
	if len(cli.destroyed) != 1 || cli.destroyed[0] != "ga-runner-dead-g1" {
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
	hash := WorkspaceDirHash("personal:1")[:12]
	for _, name := range []string{
		"ga-runner-" + hash + "-g1",
		"ga-runner-" + hash + "-g42",
	} {
		if !m.IsRunnerName(name) {
			t.Fatalf("%q must be accepted", name)
		}
	}
	for _, name := range []string{
		"postgres-1",
		"ga-runner-" + hash + "-g0",
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
	want := WorkspaceDirHash("personal:42")
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
	hash := WorkspaceDirHash("personal:1")
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

func TestManagerEnsureRunnerReusesExistingSameGenerationAfterRestart(t *testing.T) {
	ctx := context.Background()
	hash := WorkspaceDirHash("personal:1")
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
	hash := WorkspaceDirHash("personal:1")
	prefix := "ga-runner-" + hash[:12] + "-"
	cli := &fakeCLI{containers: []RunnerInfo{{Name: prefix + "g5", Running: true}}}
	m := NewManager(ManagerConfig{CLI: cli, WorkspaceRoot: t.TempDir(), ContainerNamePrefix: "ga-runner"})
	if _, _, err := m.EnsureRunner(ctx, EnsureRunnerRequest{WorkspaceKey: "personal:1", Generation: 3}); err == nil {
		t.Fatal("requesting a generation older than an existing runner must be rejected")
	}
}
