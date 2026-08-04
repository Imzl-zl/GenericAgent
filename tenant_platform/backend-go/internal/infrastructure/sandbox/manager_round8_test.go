package sandbox

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Round8 审查: EnsureRunner 持 workspace 锁时, post-create inspect 失败路径
// 调用公开 DestroyRunner, 后者再次获取同一把非重入锁 -> 确定性死锁。
func TestManagerEnsureRunnerPostCreateInspectFailureDoesNotDeadlock(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	hash := mustWorkspaceHash("personal:1")
	cli := &fakeCLI{
		inspectErr:      errors.New("post-create config drift"),
		inspectFailOn:   1, // 第 1 次 Inspect(= 创建后校验)失败
		runnerHash:      hash,
		runnerGen:       1,
		managerManaged:  true,
		containerExists: true,
	}
	m := NewManager(ManagerConfig{CLI: cli, WorkspaceRoot: root, ContainerNamePrefix: "ga-runner"})

	done := make(chan struct{})
	var err error
	go func() {
		defer close(done)
		_, _, err = m.EnsureRunner(ctx, EnsureRunnerRequest{WorkspaceKey: "personal:1", Generation: 1})
	}()
	select {
	case <-done:
		if err == nil {
			t.Fatalf("expected post-create inspect failure error, got nil")
		}
		if len(cli.destroyed) != 1 {
			t.Fatalf("expected runner destroyed after inspect failure, destroyed=%v", cli.destroyed)
		}
		// config 目录必须已清理(短期 mTLS 材料不残留)。
		configDir := filepath.Join(root, hash, "config", "g1")
		if _, statErr := os.Stat(configDir); !os.IsNotExist(statErr) {
			t.Fatalf("config dir must be removed after failed create+inspect: %v", statErr)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("EnsureRunner deadlocked on post-create inspect failure (DestroyRunner re-entered workspace lock)")
	}
}

// Round8 审查: EnsureRunner 旧 generation 替换路径直接 CLI.Destroy,
// 绕过 DestroyRunner 的 config/g<gen> 清理, 短期 mTLS 材料残留。
func TestManagerEnsureRunnerReplacesOldGenerationAndCleansConfig(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	hash := mustWorkspaceHash("personal:1")
	// 预写旧 generation 的 config(模拟残留材料)。
	oldConfig := filepath.Join(root, hash, "config", "g1")
	if err := os.MkdirAll(oldConfig, 0o770); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(oldConfig, "server.key"), []byte("old-key"), 0o640); err != nil {
		t.Fatal(err)
	}
	// 模拟 Manager 重启后 scan 到旧 generation 容器(label 提供 hash/gen)。
	cli := &fakeCLI{
		containers: []RunnerInfo{{Name: "ga-runner-" + hash[:12] + "-g1"}},
	}
	m := NewManager(ManagerConfig{CLI: cli, WorkspaceRoot: root, ContainerNamePrefix: "ga-runner"})

	runner, created, err := m.EnsureRunner(ctx, EnsureRunnerRequest{WorkspaceKey: "personal:1", Generation: 2})
	if err != nil {
		t.Fatalf("EnsureRunner: %v", err)
	}
	if !created {
		t.Fatal("expected new runner creation")
	}
	if runner.Name != "ga-runner-"+hash[:12]+"-g2" {
		t.Fatalf("unexpected runner name %q", runner.Name)
	}
	if len(cli.destroyed) != 1 || cli.destroyed[0] != "ga-runner-"+hash[:12]+"-g1" {
		t.Fatalf("expected stale g1 destroyed, got %v", cli.destroyed)
	}
	if _, err := os.Stat(oldConfig); !os.IsNotExist(err) {
		t.Fatalf("old generation config must be cleaned after replacement: %v", err)
	}
}
