package checkpoint

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/domain"
	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/infrastructure/postgres"
)

// seedTaskConv 与 seedTask 同构, 额外指定对话单元桶键(conversation_key)。
func seedTaskConv(t *testing.T, store *postgres.Store, messageID, conversationKey string) domain.Task {
	t.Helper()
	ctx := context.Background()
	dev, err := store.EnsureAdminContext(ctx, 42, "dev")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.SubmitTask(ctx, domain.SubmitTaskCommand{
		SessionKey:        dev.SessionKey,
		RequesterUserID:   42,
		Source:            "web",
		SourceInstanceID:  "i1",
		MessageID:         messageID,
		ConversationKey:   conversationKey,
		Prompt:            "hello bucket",
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
	if _, err := store.MarkDispatchStarted(ctx, claimed.ID, owner, "worker-1", false); err != nil {
		t.Fatal(err)
	}
	if _, err := store.MarkRunning(ctx, claimed.ID, owner); err != nil {
		t.Fatal(err)
	}
	return claimed
}

// commitBundle 模拟 worker 写 staging bundle 并提交, 返回 committed 快照。
func commitBundle(t *testing.T, store *postgres.Store, coord Coordinator, task domain.Task, body string) (committed CommittedCheckpoint, raw []byte) {
	t.Helper()
	ctx := context.Background()
	lease, err := coord.Prepare(ctx, CheckpointPrepareRequest{
		TaskID:           task.ID,
		WorkspaceID:      task.WorkspaceID,
		SessionKey:       task.SessionKey,
		MaxBundleBytes:   1024 * 1024,
		RunnerGeneration: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	bundle := map[string]any{
		"schema_version":    "genericagent.snapshot.v1",
		"task_id":           task.ID,
		"session_key":       task.SessionKey,
		"runner_generation": 1,
		"backend_history":   []any{map[string]any{"role": "user", "content": body}},
		"agent_history":     []any{},
		"working":           map[string]any{},
		"display_history":   []any{},
		"result":            map[string]any{"content_type": "text/plain; charset=utf-8", "body": body},
		"result_digest":     "sha256:" + hex.EncodeToString(hashBytes([]byte(body))),
	}
	raw, _ = json.Marshal(bundle)
	if err := os.WriteFile(lease.StagingRef, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	sum := "sha256:" + hex.EncodeToString(hashBytes(raw))
	committed, err = coord.Commit(ctx, ReadyCheckpoint{
		TaskID: task.ID, SnapshotID: lease.SnapshotID, CheckpointToken: lease.Token,
		StagingRef: lease.StagingRef, Checksum: sum,
		ResultDigest:    bundle["result_digest"].(string),
		RunnerGeneration: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.CompleteSucceeded(
		ctx, task.ID, "platform-a", committed.SnapshotID, committed.FileRef, committed.Checksum,
		committed.ResultRef, committed.ResultDigest, len(body), nil,
	); err != nil {
		t.Fatal(err)
	}
	return committed, raw
}

// TestLocalCoordinator_ConversationBuckets_Isolated 验证对话单元分桶的端到端
// 不变量(IM_CHANNEL_ARCHITECTURE §3):
//  1. 默认桶('')与非默认桶('group_x')各自独立恢复点;
//  2. 非默认桶提交不覆盖默认桶指针(workspaces.current_snapshot_id);
//  3. 任一桶的进展不影响其他桶的恢复点。
func TestLocalCoordinator_ConversationBuckets_Isolated(t *testing.T) {
	pool := postgres.OpenTestPool(t)
	store, err := postgres.NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	coord, err := NewLocalCoordinator(LocalConfig{
		RuntimeRoot:        t.TempDir(),
		PlatformInstanceID: "platform-a",
		Store:              store,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	// 1. 默认桶任务(微信个人自用单桶语义, conversation_key='')。
	taskA := seedTaskConv(t, store, "m-bucket-a", "")
	committedA, rawA := commitBundle(t, store, coord, taskA, "bucket-A-body")
	wsID := taskA.WorkspaceID

	restoreA, ok, err := coord.CurrentRestorePoint(ctx, wsID, "")
	if err != nil || !ok {
		t.Fatalf("default bucket restore: ok=%v err=%v", ok, err)
	}
	if restoreA.SnapshotID != committedA.SnapshotID {
		t.Fatalf("default bucket restore = %s, want %s", restoreA.SnapshotID, committedA.SnapshotID)
	}
	if got, err := os.ReadFile(restoreA.SnapshotRef); err != nil || !bytes.Equal(got, rawA) {
		t.Fatalf("default bucket bundle mismatch: err=%v", err)
	}

	// 2. 非默认桶任务(群聊语义, conversation_key='group_x')。
	taskB := seedTaskConv(t, store, "m-bucket-b", "group_x")
	committedB, _ := commitBundle(t, store, coord, taskB, "bucket-B-body")

	restoreB, ok, err := coord.CurrentRestorePoint(ctx, wsID, "group_x")
	if err != nil || !ok {
		t.Fatalf("bucket group_x restore: ok=%v err=%v", ok, err)
	}
	if restoreB.SnapshotID != committedB.SnapshotID {
		t.Fatalf("bucket group_x restore = %s, want %s", restoreB.SnapshotID, committedB.SnapshotID)
	}

	// 3. 不变量: 非默认桶提交不得覆盖默认桶指针。
	restoreA2, ok, err := coord.CurrentRestorePoint(ctx, wsID, "")
	if err != nil || !ok {
		t.Fatalf("default bucket restore after group_x commit: ok=%v err=%v", ok, err)
	}
	if restoreA2.SnapshotID != committedA.SnapshotID {
		t.Fatalf("default bucket pointer overwritten by non-default bucket: got %s want %s", restoreA2.SnapshotID, committedA.SnapshotID)
	}

	// 4. 不变量: 未提交过的桶无恢复点; 未知桶同样为空。
	if _, ok, err := coord.CurrentRestorePoint(ctx, wsID, "never_seen"); err != nil || ok {
		t.Fatalf("unknown bucket restore: ok=%v err=%v", ok, err)
	}

	// 5. 反向不变量: 默认桶再推进一轮, 非默认桶恢复点不变。
	taskA2 := seedTaskConv(t, store, "m-bucket-a2", "")
	committedA2, _ := commitBundle(t, store, coord, taskA2, "bucket-A2-body")
	restoreA3, ok, err := coord.CurrentRestorePoint(ctx, wsID, "")
	if err != nil || !ok || restoreA3.SnapshotID != committedA2.SnapshotID {
		t.Fatalf("default bucket second round: ok=%v err=%v got=%s", ok, err, restoreA3.SnapshotID)
	}
	restoreB2, ok, err := coord.CurrentRestorePoint(ctx, wsID, "group_x")
	if err != nil || !ok {
		t.Fatalf("bucket group_x after default bucket advance: ok=%v err=%v", ok, err)
	}
	if restoreB2.SnapshotID != committedB.SnapshotID {
		t.Fatalf("group_x restore point polluted by default bucket: got %s want %s", restoreB2.SnapshotID, committedB.SnapshotID)
	}

	// 6. snapshot 行记录了桶键(审计/对账维度)。
	var gotKey string
	if err := pool.QueryRow(ctx, `
SELECT conversation_key FROM workspace_snapshots WHERE id = $1::uuid
`, committedB.SnapshotID).Scan(&gotKey); err != nil {
		t.Fatal(err)
	}
	if gotKey != "group_x" {
		t.Fatalf("snapshot conversation_key = %q, want group_x", gotKey)
	}
	var taskKey string
	if err := pool.QueryRow(ctx, `
SELECT conversation_key FROM tasks WHERE id = $1
`, taskB.ID).Scan(&taskKey); err != nil {
		t.Fatal(err)
	}
	if taskKey != "group_x" {
		t.Fatalf("task conversation_key = %q, want group_x", taskKey)
	}
}
