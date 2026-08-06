package sandbox

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/domain"
)

// ---------------------------------------------------------------------------
// round11 审查 I6: 无容器对应的 workspace config/g<gen> 目录(短期 mTLS
// 私钥/证书/token)必须被对账回收; 活跃容器的配置与创建中窗口内的目录
// 不得触碰。
// ---------------------------------------------------------------------------

func writeConfigDir(t *testing.T, workspaceRoot, hash string, gen uint64, age time.Duration) string {
	t.Helper()
	dir := filepath.Join(workspaceRoot, hash, "config", "g"+uint64String(gen))
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "mykey.py"), []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-age)
	if err := os.Chtimes(dir, old, old); err != nil {
		t.Fatal(err)
	}
	return dir
}

func uint64String(v uint64) string {
	if v == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	return string(buf[i:])
}

func TestReconcileOrphanWorkspaceConfigsRemovesOrphansKeepsActive(t *testing.T) {
	root := t.TempDir()
	hash1, err := domain.WorkspaceDirHash("personal:1")
	if err != nil {
		t.Fatal(err)
	}
	hash2, err := domain.WorkspaceDirHash("personal:2")
	if err != nil {
		t.Fatal(err)
	}
	// hash1: g1 有活跃容器(保留), g2 无容器(孤儿, 删除)。
	writeConfigDir(t, root, hash1, 1, 2*time.Hour)
	orphan := writeConfigDir(t, root, hash1, 2, 2*time.Hour)
	// hash2: g1 无容器(孤儿, 删除)。
	orphan2 := writeConfigDir(t, root, hash2, 1, 2*time.Hour)

	cli := &fakeCLI{
		managerManaged: true, containerExists: true,
		containers: []RunnerInfo{{
			Name: "ga-runner-" + hash1[:12] + "-g1", Running: true,
			WorkspaceHash: hash1, Generation: 1,
		}},
	}
	m := NewManager(ManagerConfig{CLI: cli, WorkspaceRoot: root, ContainerNamePrefix: "ga-runner"})
	n, err := m.ReconcileOrphanWorkspaceConfigs(context.Background(), "ga-runner")
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("removed = %d, want 2", n)
	}
	if _, err := os.Stat(filepath.Join(root, hash1, "config", "g1")); err != nil {
		t.Fatalf("active runner config must be kept: %v", err)
	}
	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Fatalf("orphan config g2 must be removed, stat err=%v", err)
	}
	if _, err := os.Stat(orphan2); !os.IsNotExist(err) {
		t.Fatalf("orphan config hash2/g1 must be removed, stat err=%v", err)
	}
}

func TestReconcileOrphanWorkspaceConfigsSkipsFreshDirs(t *testing.T) {
	root := t.TempDir()
	hash1, err := domain.WorkspaceDirHash("personal:1")
	if err != nil {
		t.Fatal(err)
	}
	// 孤儿但 mtime 新(创建中窗口): 保留, 防止与进行中的创建竞态。
	fresh := writeConfigDir(t, root, hash1, 3, time.Minute)

	m := NewManager(ManagerConfig{CLI: &fakeCLI{managerManaged: true, containerExists: true}, WorkspaceRoot: root, ContainerNamePrefix: "ga-runner"})
	n, err := m.ReconcileOrphanWorkspaceConfigs(context.Background(), "ga-runner")
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("fresh orphan config must be kept, removed=%d", n)
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Fatalf("fresh config dir must survive: %v", err)
	}
}
