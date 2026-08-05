package checkpoint

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/domain"
	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/infrastructure/postgres"
)

// commitTestCheckpoint runs Prepare + Commit for a seeded task and returns the
// committed checkpoint plus the workspace hash directory.
func commitTestCheckpoint(t *testing.T, coord *WorkspaceCoordinator, taskID, sessionKey string) (CommittedCheckpoint, string) {
	t.Helper()
	ctx := context.Background()
	lease, err := coord.Prepare(ctx, CheckpointPrepareRequest{
		TaskID: taskID, SessionKey: sessionKey,
		MaxBundleBytes: 1 << 20, RunnerGeneration: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	body := "reconcile test body"
	bundle := map[string]any{
		"schema_version":    snapshotSchemaVersion,
		"task_id":           taskID,
		"session_key":       sessionKey,
		"runner_generation": 1,
		"result":            map[string]any{"body": body},
		"result_digest":     "sha256:" + hex.EncodeToString(hashBytes([]byte(body))),
	}
	raw, err := json.Marshal(bundle)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(lease.StagingRef, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	checksum := "sha256:" + hex.EncodeToString(hashBytes(raw))
	committed, err := coord.Commit(ctx, ReadyCheckpoint{
		TaskID: taskID, SnapshotID: lease.SnapshotID, CheckpointToken: lease.Token,
		StagingRef: lease.StagingRef, Checksum: checksum,
		ResultDigest: bundle["result_digest"].(string), RunnerGeneration: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	hash, id, err := parseOpaqueRef(committed.FileRef, opaqueFilePrefix)
	if err != nil {
		t.Fatal(err)
	}
	if id != committed.SnapshotID {
		t.Fatalf("opaque ref snapshot %s != committed %s", id, committed.SnapshotID)
	}
	return committed, hash
}

// commitAndSucceed runs the DB commit path (CompleteSucceeded) so the snapshot
// row moves to state='committed' and is referenced as a restore point.
func commitAndSucceed(t *testing.T, store *postgres.Store, task domain.Task, committed CommittedCheckpoint) {
	t.Helper()
	if _, err := store.CompleteSucceeded(context.Background(), task.ID, "platform-a",
		committed.SnapshotID, committed.FileRef, committed.Checksum,
		committed.ResultRef, committed.ResultDigest, 19, nil); err != nil {
		t.Fatal(err)
	}
}

func bundlePath(coord *WorkspaceCoordinator, hash, snapshotID string) string {
	return filepath.Join(coord.workspacesRoot, hash, "state", "committed", snapshotID+".bundle.json")
}

func resultPath(coord *WorkspaceCoordinator, hash, snapshotID string) string {
	return filepath.Join(coord.workspacesRoot, hash, "state", "results", snapshotID+".result")
}

func deleteSnapshotRow(t *testing.T, pool *pgxpool.Pool, snapshotID string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `DELETE FROM workspace_snapshots WHERE id = $1::uuid`, snapshotID); err != nil {
		t.Fatal(err)
	}
}

func setFileAge(t *testing.T, path string, age time.Duration) {
	t.Helper()
	old := time.Now().Add(-age)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
}

// TestReconcileOrphanCommittedFiles_KeepsReferenced verifies that files still
// referenced by a committed snapshot row survive reconciliation.
func TestReconcileOrphanCommittedFiles_KeepsReferenced(t *testing.T) {
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
	committed, hash := commitTestCheckpoint(t, coord, task.ID, task.SessionKey)
	commitAndSucceed(t, store, task, committed)
	setFileAge(t, bundlePath(coord, hash, committed.SnapshotID), 2*time.Hour)
	setFileAge(t, resultPath(coord, hash, committed.SnapshotID), 2*time.Hour)

	n, err := coord.ReconcileOrphanCommittedFiles(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("referenced committed files must not be removed, removed=%d", n)
	}
	if _, err := os.Stat(bundlePath(coord, hash, committed.SnapshotID)); err != nil {
		t.Fatalf("committed bundle must still exist: %v", err)
	}
}

// TestReconcileOrphanCommittedFiles_RemovesConfirmedOrphans verifies that old
// files whose DB row is gone (or no longer committed) are deleted.
func TestReconcileOrphanCommittedFiles_RemovesConfirmedOrphans(t *testing.T) {
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
	committed, hash := commitTestCheckpoint(t, coord, task.ID, task.SessionKey)
	deleteSnapshotRow(t, pool, committed.SnapshotID)
	setFileAge(t, bundlePath(coord, hash, committed.SnapshotID), 2*time.Hour)
	setFileAge(t, resultPath(coord, hash, committed.SnapshotID), 2*time.Hour)

	n, err := coord.ReconcileOrphanCommittedFiles(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("expected 1 orphan removed, got %d", n)
	}
	if _, err := os.Stat(bundlePath(coord, hash, committed.SnapshotID)); !os.IsNotExist(err) {
		t.Fatalf("orphan bundle must be removed, stat err=%v", err)
	}
	if _, err := os.Stat(resultPath(coord, hash, committed.SnapshotID)); !os.IsNotExist(err) {
		t.Fatalf("orphan result must be removed, stat err=%v", err)
	}
}

// TestReconcileOrphanCommittedFiles_SkipsFreshFiles verifies that young orphan
// files (within the in-flight Commit window) are kept, so reconciliation can
// never race a Commit that materialized the file but has not committed yet.
func TestReconcileOrphanCommittedFiles_SkipsFreshFiles(t *testing.T) {
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
	committed, hash := commitTestCheckpoint(t, coord, task.ID, task.SessionKey)
	deleteSnapshotRow(t, pool, committed.SnapshotID)
	// File mtime stays fresh: must be kept.

	n, err := coord.ReconcileOrphanCommittedFiles(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("fresh orphan must be kept for the in-flight window, removed=%d", n)
	}
	if _, err := os.Stat(bundlePath(coord, hash, committed.SnapshotID)); err != nil {
		t.Fatalf("fresh orphan bundle must still exist: %v", err)
	}
}

// TestReconcileOrphanCommittedFiles_NonCommittedStateIsOrphan verifies that a
// file whose row exists but is not in committed state is treated as orphan
// (e.g. snapshot quarantined after a failed terminal path).
func TestReconcileOrphanCommittedFiles_NonCommittedStateIsOrphan(t *testing.T) {
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
	committed, hash := commitTestCheckpoint(t, coord, task.ID, task.SessionKey)
	if _, err := pool.Exec(context.Background(),
		`UPDATE workspace_snapshots SET state = 'quarantined' WHERE id = $1::uuid`, committed.SnapshotID); err != nil {
		t.Fatal(err)
	}
	setFileAge(t, bundlePath(coord, hash, committed.SnapshotID), 2*time.Hour)

	n, err := coord.ReconcileOrphanCommittedFiles(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("expected non-committed state file to be removed, got %d", n)
	}
}
