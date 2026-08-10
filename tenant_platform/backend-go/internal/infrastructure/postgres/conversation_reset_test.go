package postgres

import (
	"context"
	"testing"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/domain"
)

// submitBucketTask 提交指定对话单元桶的任务并返回任务行。
func submitBucketTask(t *testing.T, store *Store, sessionKey string, messageID, conversationKey string) domain.Task {
	t.Helper()
	ctx := context.Background()
	task, err := store.SubmitTask(ctx, domain.SubmitTaskCommand{
		SessionKey:        sessionKey,
		RequesterUserID:   42,
		Source:            "web",
		SourceInstanceID:  "i1",
		MessageID:         messageID,
		ConversationKey:   conversationKey,
		Prompt:            "reset bucket probe",
		PersonaSnapshot:   []string{},
		ToolPolicyVersion: "foundation.no-host-tools.v1",
	})
	if err != nil {
		t.Fatal(err)
	}
	return task
}

// TestConversationReset_BucketIsolation 验证 /new 桶级语义
// (IM_CHANNEL_ARCHITECTURE §3): /new 只影响指定对话单元桶——
// 该桶下一个任务 fresh, 其他桶任务不受影响, 取消也只作用于该桶。
func TestConversationReset_BucketIsolation(t *testing.T) {
	pool := OpenTestPool(t)
	store, err := NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	dev, err := store.EnsureAdminContext(ctx, 42, "dev")
	if err != nil {
		t.Fatal(err)
	}

	// 两桶各排一个 queued 任务(将被 /new 按桶取消判定)。
	qA := submitBucketTask(t, store, dev.SessionKey, "reset-q-a", "")
	qB := submitBucketTask(t, store, dev.SessionKey, "reset-q-b", "group_x")

	// /new 只作用于默认桶(微信单桶语义)。
	cancelled, err := store.ResetWorkspaceForNewSession(ctx, dev.SessionKey, "")
	if err != nil {
		t.Fatal(err)
	}
	if cancelled != 1 {
		t.Fatalf("cancelled = %d, want 1 (only default bucket queued task)", cancelled)
	}

	// 默认桶 queued 任务被取消, 其他桶任务保留。
	if task, err := store.GetTask(ctx, qA.ID); err != nil || task.Status != domain.TaskCancelled {
		t.Fatalf("default bucket task status = %v err=%v, want cancelled", task.Status, err)
	}
	if task, err := store.GetTask(ctx, qB.ID); err != nil || task.Status != domain.TaskQueued {
		t.Fatalf("group_x bucket task status = %v err=%v, want queued (unaffected)", task.Status, err)
	}

	// /new 后: 默认桶新任务 fresh, 其他桶新任务不 fresh。
	freshA := submitBucketTask(t, store, dev.SessionKey, "reset-f-a", "")
	if !freshA.FreshSession {
		t.Fatal("default bucket task after /new must be fresh")
	}
	freshB := submitBucketTask(t, store, dev.SessionKey, "reset-f-b", "group_x")
	if freshB.FreshSession {
		t.Fatal("group_x bucket task must NOT be fresh (reset is bucket-scoped)")
	}

	// WorkspaceIsFresh 按桶判定。
	if ok, err := store.WorkspaceIsFresh(ctx, dev.SessionKey, ""); err != nil || !ok {
		t.Fatalf("default bucket fresh = %v err=%v, want true", ok, err)
	}
	if ok, err := store.WorkspaceIsFresh(ctx, dev.SessionKey, "group_x"); err != nil || ok {
		t.Fatalf("group_x bucket fresh = %v err=%v, want false", ok, err)
	}

	// 消费语义(与 CompleteSucceeded 相同的行删除): 默认桶 fresh 消费后
	// 该桶不再 fresh, 其他桶仍不受影响。
	if _, err := pool.Exec(ctx, `
DELETE FROM conversation_resets WHERE workspace_id = $1 AND conversation_key = ''
`, dev.WorkspaceID); err != nil {
		t.Fatal(err)
	}
	if ok, err := store.WorkspaceIsFresh(ctx, dev.SessionKey, ""); err != nil || ok {
		t.Fatalf("default bucket fresh after consume = %v err=%v, want false", ok, err)
	}
}

// TestConversationResetMigrationReapplyIdempotent 验证 0052 在 marker 缺失
// 重放场景下幂等(EnsureSchema 重放不得因 workspaces.reset_at 列已删而失败)。
func TestConversationResetMigrationReapplyIdempotent(t *testing.T) {
	pool := requireDB(t)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `DROP TABLE IF EXISTS migration_0052_conversation_resets_marker`); err != nil {
		t.Fatal(err)
	}
	if err := EnsureSchema(ctx, pool, ""); err != nil {
		t.Fatalf("EnsureSchema reapply after marker drop: %v", err)
	}
	var hasResetAt bool
	if err := pool.QueryRow(ctx, `
SELECT EXISTS (
  SELECT 1 FROM information_schema.columns
  WHERE table_schema = current_schema() AND table_name = 'workspaces' AND column_name = 'reset_at'
)`).Scan(&hasResetAt); err != nil {
		t.Fatal(err)
	}
	if hasResetAt {
		t.Fatal("workspaces.reset_at must stay dropped after reapply")
	}
	var hasResets bool
	if err := pool.QueryRow(ctx, `
SELECT EXISTS (SELECT 1 FROM information_schema.tables WHERE table_name = 'conversation_resets')
`).Scan(&hasResets); err != nil {
		t.Fatal(err)
	}
	if !hasResets {
		t.Fatal("conversation_resets table missing after reapply")
	}
}
