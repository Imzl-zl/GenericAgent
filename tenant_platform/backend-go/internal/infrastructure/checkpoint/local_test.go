package checkpoint

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/domain"
	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/infrastructure/postgres"
)

func seedTask(t *testing.T, store *postgres.Store) domain.Task {
	t.Helper()
	ctx := context.Background()
	dev, err := store.EnsureDevelopmentContext(ctx, 42, "dev")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.SubmitTask(ctx, domain.SubmitTaskCommand{
		SessionKey:        dev.SessionKey,
		RequesterUserID:   42,
		Source:            "web",
		SourceInstanceID:  "i1",
		MessageID:         "m-ckpt-1",
		Prompt:            "hello checkpoint",
		PersonaSnapshot:   []string{"p"},
		ToolPolicyVersion: "foundation.no-host-tools.v1",
	}); err != nil {
		t.Fatal(err)
	}
	owner := "platform-a"
	claimed, ok, err := store.ClaimNextTask(ctx, dev.SessionKey, owner, time.Minute)
	if err != nil || !ok {
		t.Fatalf("claim: ok=%v err=%v", ok, err)
	}
	if _, err := store.MarkDispatchStarted(ctx, claimed.ID, owner, "worker-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.MarkRunning(ctx, claimed.ID, owner); err != nil {
		t.Fatal(err)
	}
	return claimed
}

func TestLocalCoordinator_PrepareCommitRead_TokenMismatch(t *testing.T) {
	pool := postgres.OpenTestPool(t)
	store, err := postgres.NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	task := seedTask(t, store)
	root := t.TempDir()
	coord, err := NewLocalCoordinator(LocalConfig{
		RuntimeRoot:        root,
		PlatformInstanceID: "platform-a",
		Store:              store,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	lease, err := coord.Prepare(ctx, CheckpointPrepareRequest{
		TaskID:         task.ID,
		WorkspaceID:    task.WorkspaceID,
		SessionKey:     task.SessionKey,
		MaxBundleBytes: 1024 * 1024,
	})
	if err != nil {
		t.Fatal(err)
	}
	if lease.Token == "" || lease.SnapshotID == "" {
		t.Fatal("empty lease")
	}
	if !strings.Contains(lease.StagingRef, lease.Token) {
		t.Fatalf("staging ref should be token-scoped: %s", lease.StagingRef)
	}

	body := "result-body"
	bundle := map[string]any{
		"schema_version":  "genericagent.snapshot.v1",
		"task_id":         task.ID,
		"session_key":     task.SessionKey,
		"backend_history": []any{},
		"agent_history":   []any{},
		"working":         map[string]any{},
		"display_history": []any{},
		"result":          map[string]any{"content_type": "text/plain; charset=utf-8", "body": body},
		"result_digest":   "sha256:" + hex.EncodeToString(hashBytes([]byte(body))),
	}
	raw, _ := json.Marshal(bundle)
	if err := os.WriteFile(lease.StagingRef, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	sum := "sha256:" + hex.EncodeToString(hashBytes(raw))

	if _, err := coord.Commit(ctx, ReadyCheckpoint{
		TaskID: task.ID, SnapshotID: lease.SnapshotID, CheckpointToken: "wrong",
		StagingRef: lease.StagingRef, Checksum: sum, ResultDigest: bundle["result_digest"].(string),
	}); err == nil {
		t.Fatal("expected token mismatch")
	}

	committed, err := coord.Commit(ctx, ReadyCheckpoint{
		TaskID: task.ID, SnapshotID: lease.SnapshotID, CheckpointToken: lease.Token,
		StagingRef: lease.StagingRef, Checksum: sum, ResultDigest: bundle["result_digest"].(string),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(committed.ResultRef, "result:") {
		t.Fatalf("opaque result ref: %s", committed.ResultRef)
	}
	if strings.Contains(committed.ResultRef, `\`) || strings.Contains(committed.ResultRef, `/`) {
		t.Fatalf("result ref looks like path: %s", committed.ResultRef)
	}

	if _, err := store.CompleteSucceeded(ctx, task.ID, "platform-a", committed.SnapshotID, committed.FileRef, committed.Checksum, committed.ResultRef, committed.ResultDigest, len(body)); err != nil {
		t.Fatal(err)
	}
	restore, ok, err := coord.CurrentRestorePoint(ctx, task.WorkspaceID)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("committed workspace snapshot was not available for restore")
	}
	if restore.SnapshotID != committed.SnapshotID || restore.Checksum != committed.Checksum {
		t.Fatalf("restore=%+v committed=%+v", restore, committed)
	}
	restoredRaw, err := os.ReadFile(restore.SnapshotRef)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(restoredRaw, raw) {
		t.Fatal("restore point did not resolve the committed bundle")
	}

	payload, err := coord.ReadResult(ctx, committed.ResultRef, committed.ResultDigest)
	if err != nil {
		t.Fatal(err)
	}
	if string(payload.Body) != body {
		t.Fatalf("body=%q", payload.Body)
	}

	if _, err := coord.ReadResult(ctx, root+"/results/x", committed.ResultDigest); err == nil {
		t.Fatal("expected path rejection")
	}
	if _, err := coord.ReadResult(ctx, `C:\secrets`, ""); err == nil {
		t.Fatal("expected path rejection")
	}
}

func TestLocalCoordinator_CommitRejectsBundleAbovePreparedLimit(t *testing.T) {
	pool := postgres.OpenTestPool(t)
	store, err := postgres.NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	task := seedTask(t, store)
	coord, err := NewLocalCoordinator(LocalConfig{
		RuntimeRoot: t.TempDir(), PlatformInstanceID: "platform-a", Store: store,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	const maxBundleBytes = 128
	lease, err := coord.Prepare(ctx, CheckpointPrepareRequest{
		TaskID: task.ID, WorkspaceID: task.WorkspaceID, SessionKey: task.SessionKey,
		MaxBundleBytes: maxBundleBytes,
	})
	if err != nil {
		t.Fatal(err)
	}
	body := strings.Repeat("x", 512)
	bundle := map[string]any{
		"schema_version": snapshotSchemaVersion,
		"task_id":        task.ID,
		"session_key":    task.SessionKey,
		"result":         map[string]any{"body": body},
		"result_digest":  "sha256:" + hex.EncodeToString(hashBytes([]byte(body))),
	}
	raw, err := json.Marshal(bundle)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) <= maxBundleBytes {
		t.Fatalf("test bundle len=%d must exceed limit", len(raw))
	}
	if err := os.WriteFile(lease.StagingRef, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	checksum := "sha256:" + hex.EncodeToString(hashBytes(raw))
	if _, err := coord.Commit(ctx, ReadyCheckpoint{
		TaskID: task.ID, SnapshotID: lease.SnapshotID, CheckpointToken: lease.Token,
		StagingRef: lease.StagingRef, Checksum: checksum, ResultDigest: bundle["result_digest"].(string),
	}); err == nil || !strings.Contains(err.Error(), "max bundle") {
		t.Fatalf("expected max bundle rejection, got %v", err)
	}
}

func TestSyncDirectorySupportsCheckpointRuntime(t *testing.T) {
	root := t.TempDir()
	path := root + string(os.PathSeparator) + "bundle.json"
	if err := atomicWrite(path, []byte("bundle")); err != nil {
		t.Fatal(err)
	}
	if err := syncDirectory(root); err != nil {
		t.Fatalf("sync checkpoint directory: %v", err)
	}
}

func TestChecksumHelper(t *testing.T) {
	b := []byte("x")
	sum := sha256.Sum256(b)
	if hex.EncodeToString(hashBytes(b)) != hex.EncodeToString(sum[:]) {
		t.Fatal("hash mismatch")
	}
}
