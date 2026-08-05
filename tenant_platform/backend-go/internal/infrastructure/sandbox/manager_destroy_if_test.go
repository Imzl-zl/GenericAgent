package sandbox

import (
	"context"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// round11 审查 I3: 孤儿回收的判定与销毁必须在同一 workspace 锁内、基于
// 销毁时刻的最新容器状态——扫描快照与销毁之间的 created→running 竞态
// 不得误杀刚启动的 Runner; 活跃 Runner 的 absTTL 强杀同样以锁内状态为准。
// ---------------------------------------------------------------------------

func destroyIfManager(t *testing.T, cli *fakeCLI, name string) (bool, error) {
	t.Helper()
	m := NewManager(ManagerConfig{CLI: cli, WorkspaceRoot: t.TempDir(), ContainerNamePrefix: "ga-runner"})
	return m.DestroyRunnerIf(context.Background(), name, func(info RunnerInfo) bool {
		if !info.Running {
			return true
		}
		return time.Since(info.CreatedAt) >= 25*time.Hour
	})
}

func TestDestroyRunnerIfReapsStoppedRunner(t *testing.T) {
	cli := &fakeCLI{
		managerManaged: true, containerExists: true,
		runnerHash: "abc", runnerGen: 2,
		containers: []RunnerInfo{{Name: "ga-runner-abc-g2", Running: false, CreatedAt: time.Now().Add(-2 * time.Hour)}},
	}
	reaped, err := destroyIfManager(t, cli, "ga-runner-abc-g2")
	if err != nil {
		t.Fatal(err)
	}
	if !reaped {
		t.Fatal("stopped runner must be reaped")
	}
	if len(cli.destroyed) != 1 || cli.destroyed[0] != "ga-runner-abc-g2" {
		t.Fatalf("destroyed = %v, want [ga-runner-abc-g2]", cli.destroyed)
	}
}

func TestDestroyRunnerIfSkipsRunningFreshRunner(t *testing.T) {
	cli := &fakeCLI{
		managerManaged: true, containerExists: true,
		runnerHash: "abc", runnerGen: 2,
		containers: []RunnerInfo{{Name: "ga-runner-abc-g2", Running: true, CreatedAt: time.Now()}},
	}
	reaped, err := destroyIfManager(t, cli, "ga-runner-abc-g2")
	if err != nil {
		t.Fatal(err)
	}
	if reaped {
		t.Fatal("fresh running runner must not be reaped")
	}
	if len(cli.destroyed) != 0 {
		t.Fatalf("destroyed = %v, want none", cli.destroyed)
	}
}

func TestDestroyRunnerIfReapsRunningPastTTL(t *testing.T) {
	cli := &fakeCLI{
		managerManaged: true, containerExists: true,
		runnerHash: "abc", runnerGen: 2,
		containers: []RunnerInfo{{Name: "ga-runner-abc-g2", Running: true, CreatedAt: time.Now().Add(-48 * time.Hour)}},
	}
	reaped, err := destroyIfManager(t, cli, "ga-runner-abc-g2")
	if err != nil {
		t.Fatal(err)
	}
	if !reaped {
		t.Fatal("running runner beyond absolute TTL must be reaped")
	}
	if len(cli.destroyed) != 1 {
		t.Fatalf("destroyed = %v, want 1", cli.destroyed)
	}
}

// TestDestroyRunnerIfCatchesCreatedToRunningRace is the core regression for
// round11 I3: the sweep snapshot says "created" (not running), but at destroy
// time the container is already running and fresh — it must NOT be killed.
func TestDestroyRunnerIfCatchesCreatedToRunningRace(t *testing.T) {
	cli := &fakeCLI{
		managerManaged: true, containerExists: true,
		runnerHash: "abc", runnerGen: 2,
		// runnerInfo 锁内 re-inspect 返回的最新状态: running 且新建。
		containers: []RunnerInfo{{Name: "ga-runner-abc-g2", Running: true, CreatedAt: time.Now()}},
	}
	// 模拟 sweep 的 stillOrphan 基于旧快照(created → 无条件回收)的意图:
	// 判定回调仍以最新状态为准, 拒绝回收。
	m := NewManager(ManagerConfig{CLI: cli, WorkspaceRoot: t.TempDir(), ContainerNamePrefix: "ga-runner"})
	reaped, err := m.DestroyRunnerIf(context.Background(), "ga-runner-abc-g2", func(info RunnerInfo) bool {
		return !info.Running // 旧逻辑: 非 running 才回收; 最新状态是 running → false
	})
	if err != nil {
		t.Fatal(err)
	}
	if reaped {
		t.Fatal("runner that became running before destroy must survive (created→running race)")
	}
	if len(cli.destroyed) != 0 {
		t.Fatalf("destroyed = %v, want none", cli.destroyed)
	}
}

// TestDestroyRunnerIfIdempotentWhenContainerGone verifies a container that
// disappears between sweep and destroy is treated as already reaped.
func TestDestroyRunnerIfIdempotentWhenContainerGone(t *testing.T) {
	cli := &fakeCLI{
		managerManaged: true, containerExists: true,
		runnerHash: "abc", runnerGen: 2,
		containers: nil, // 锁内 re-inspect 找不到
	}
	reaped, err := destroyIfManager(t, cli, "ga-runner-abc-g2")
	if err != nil {
		t.Fatal(err)
	}
	if !reaped {
		t.Fatal("missing container must be treated as already reaped")
	}
	if len(cli.destroyed) != 0 {
		t.Fatalf("destroyed = %v, want none", cli.destroyed)
	}
}

// TestDestroyRunnerIfRejectsForeignRunner verifies ownership is enforced
// before the lock/re-inspect path (control-plane credential leak must not
// allow destroying arbitrary host containers).
func TestDestroyRunnerIfRejectsForeignRunner(t *testing.T) {
	cli := &fakeCLI{managerManaged: false, containerExists: true}
	_, err := destroyIfManager(t, cli, "not-a-managed-runner")
	if err == nil {
		t.Fatal("foreign runner must be rejected")
	}
	if len(cli.destroyed) != 0 {
		t.Fatalf("destroyed = %v, want none", cli.destroyed)
	}
}
