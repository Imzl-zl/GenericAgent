package sandbox

import (
	"context"
	"strconv"
	"testing"
)

// fakeCLI 记录调用。
type fakeCLI struct {
	createCalls int
	create      func(spec RunnerSpec) (Runner, error)
	destroy     func(name string) error
	destroyed   []string
}

func (f *fakeCLI) CreateAndStart(ctx context.Context, spec RunnerSpec) (Runner, error) {
	f.createCalls++
	if f.create != nil {
		return f.create(spec)
	}
	return Runner{ContainerID: "cid", Name: "ga-runner-" + spec.WorkspaceHash[:12] + "-g" + strconv.FormatUint(spec.Generation, 10)}, nil
}

// EnsureRunner 模拟 Manager 的同 generation 复用/跨 generation 替换。
func (f *fakeCLI) EnsureRunner(ctx context.Context, workspaceKey string, generation uint64) (Runner, bool, error) {
	r, err := f.CreateAndStart(ctx, RunnerSpec{WorkspaceHash: WorkspaceDirHash(workspaceKey), Generation: generation})
	return r, true, err
}

func (f *fakeCLI) Destroy(ctx context.Context, name string) error {
	f.destroyed = append(f.destroyed, name)
	if f.destroy != nil {
		return f.destroy(name)
	}
	return nil
}

func (f *fakeCLI) Inspect(ctx context.Context, name string) error { return nil }

func TestManagerEnsureRunnerReusesSameGeneration(t *testing.T) {
	ctx := context.Background()
	cli := &fakeCLI{}
	m := NewManager(ManagerConfig{CLI: cli, WorkspaceRoot: t.TempDir(), ContainerNamePrefix: "ga-runner"})

	first, created, err := m.EnsureRunner(ctx, "personal:1", 1)
	if err != nil {
		t.Fatalf("EnsureRunner: %v", err)
	}
	if !created {
		t.Fatal("first ensure must create")
	}
	again, created, err := m.EnsureRunner(ctx, "personal:1", 1)
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

	first, _, err := m.EnsureRunner(ctx, "personal:1", 1)
	if err != nil {
		t.Fatal(err)
	}
	next, created, err := m.EnsureRunner(ctx, "personal:1", 2)
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
