package checkpoint

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/infrastructure/postgres"
)

// round12 审查(M2): Commit 成功后删除 staging 失败(或提交期间崩溃)时,
// staging 文件既无 DB 引用也不被 SweepExpiredCheckpoints 覆盖(lease 已消费),
// 由 ReconcileOrphanStagingFiles 按"无 writing 引用 + 孤儿年龄"兜底回收。

func stagingPath(coord *WorkspaceCoordinator, hash, token string) string {
	return filepath.Join(coord.workspacesRoot, hash, "state", "staging", token+".bundle.json")
}

// TestReconcileOrphanStagingFiles_RemovesUnreferencedStaleFile: 无 writing
// 引用的陈旧 staging 文件必须删除。
func TestReconcileOrphanStagingFiles_RemovesUnreferencedStaleFile(t *testing.T) {
	pool := postgres.OpenTestPool(t)
	store, err := postgres.NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	task := seedTask(t, store)
	coord, err := NewWorkspaceCoordinator(WorkspaceConfig{
		WorkspacesRoot: t.TempDir(), PlatformInstanceID: "platform-a", Store: store,
	})
	if err != nil {
		t.Fatal(err)
	}
	lease, err := coord.Prepare(context.Background(), CheckpointPrepareRequest{
		TaskID: task.ID, SessionKey: task.SessionKey,
		MaxBundleBytes: 1 << 20, RunnerGeneration: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(lease.StagingRef, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	// 模拟提交已消费/行已删除: staging 无 writing 引用。
	deleteSnapshotRow(t, pool, lease.SnapshotID)
	// 文件改为陈旧(超过孤儿年龄窗口)。
	old := time.Now().Add(-2 * orphanReconcileAge)
	if err := os.Chtimes(lease.StagingRef, old, old); err != nil {
		t.Fatal(err)
	}

	n, err := coord.ReconcileOrphanStagingFiles(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("expected 1 orphan staging removed, got %d", n)
	}
	if _, err := os.Stat(lease.StagingRef); !os.IsNotExist(err) {
		t.Fatalf("orphan staging file must be removed, stat err=%v", err)
	}
}

// TestReconcileOrphanStagingFiles_KeepsLiveWritingFile: 仍有 writing 引用的
// staging 文件(无论多旧)必须保留。
func TestReconcileOrphanStagingFiles_KeepsLiveWritingFile(t *testing.T) {
	pool := postgres.OpenTestPool(t)
	store, err := postgres.NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	task := seedTask(t, store)
	coord, err := NewWorkspaceCoordinator(WorkspaceConfig{
		WorkspacesRoot: t.TempDir(), PlatformInstanceID: "platform-a", Store: store,
	})
	if err != nil {
		t.Fatal(err)
	}
	lease, err := coord.Prepare(context.Background(), CheckpointPrepareRequest{
		TaskID: task.ID, SessionKey: task.SessionKey,
		MaxBundleBytes: 1 << 20, RunnerGeneration: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(lease.StagingRef, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-2 * orphanReconcileAge)
	if err := os.Chtimes(lease.StagingRef, old, old); err != nil {
		t.Fatal(err)
	}

	n, err := coord.ReconcileOrphanStagingFiles(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("live writing staging must be kept, removed=%d", n)
	}
	if _, err := os.Stat(lease.StagingRef); err != nil {
		t.Fatalf("live writing staging file must still exist: %v", err)
	}
}

// TestReconcileOrphanStagingFiles_SkipsFreshFiles: 新鲜孤儿文件保留
// (进行中 Prepare 窗口: 文件已写、DB 行尚未提交)。
func TestReconcileOrphanStagingFiles_SkipsFreshFiles(t *testing.T) {
	pool := postgres.OpenTestPool(t)
	store, err := postgres.NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	task := seedTask(t, store)
	coord, err := NewWorkspaceCoordinator(WorkspaceConfig{
		WorkspacesRoot: t.TempDir(), PlatformInstanceID: "platform-a", Store: store,
	})
	if err != nil {
		t.Fatal(err)
	}
	lease, err := coord.Prepare(context.Background(), CheckpointPrepareRequest{
		TaskID: task.ID, SessionKey: task.SessionKey,
		MaxBundleBytes: 1 << 20, RunnerGeneration: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(lease.StagingRef, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	deleteSnapshotRow(t, pool, lease.SnapshotID)

	n, err := coord.ReconcileOrphanStagingFiles(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("fresh orphan staging must be kept, removed=%d", n)
	}
	if _, err := os.Stat(lease.StagingRef); err != nil {
		t.Fatalf("fresh orphan staging file must still exist: %v", err)
	}
}
