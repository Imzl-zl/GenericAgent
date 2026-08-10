package postgres

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/domain"
)

// TestMigration0054ReplayIdempotent 验证 0054 重放幂等: 应用后删除其
// marker, EnsureSchema 重放不报错且列/约束不变(0053 先例同款)。
func TestMigration0054ReplayIdempotent(t *testing.T) {
	pool := OpenTestPool(t)
	ctx := context.Background()

	// OpenTestPool 已全量重建(含 0054)。删除 0054 marker 模拟"已应用但
	// marker 丢失"的存量库, 触发重放。
	if _, err := pool.Exec(ctx, `DROP TABLE migration_0054_im_streaming_marker`); err != nil {
		t.Fatalf("drop 0054 marker: %v", err)
	}
	if err := EnsureSchema(ctx, pool, ""); err != nil {
		t.Fatalf("replay EnsureSchema: %v", err)
	}
	// 列存在 + 默认值语义。
	var conversationType string
	var streamFinalAt *time.Time
	if err := pool.QueryRow(ctx, `
SELECT conversation_type, stream_final_at FROM tasks LIMIT 1
`).Scan(&conversationType, &streamFinalAt); err != nil && err != pgx.ErrNoRows {
		t.Fatalf("read tasks columns: %v", err)
	}
	var exists bool
	if err := pool.QueryRow(ctx, `
SELECT EXISTS (
  SELECT 1 FROM pg_constraint
  WHERE conname = 'tasks_conversation_type_check' AND conrelid = 'tasks'::regclass
)`).Scan(&exists); err != nil {
		t.Fatal(err)
	}
	if !exists {
		t.Fatal("tasks_conversation_type_check constraint missing after replay")
	}
	// 重放后 marker 已重建(后续 EnsureSchema 不再重放)。
	var marker bool
	if err := pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM migration_0054_im_streaming_marker)`).Scan(&marker); err != nil {
		t.Fatal(err)
	}
	if !marker {
		t.Fatal("0054 marker not rebuilt after replay")
	}
}

// TestTaskConversationTypeRoundTrip 验证 conversation_type 全链路:
// 提交时归一(group 保留 / 空与非法回退 private), 读回一致。
func TestTaskConversationTypeRoundTrip(t *testing.T) {
	pool := OpenTestPool(t)
	store, err := NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	dev := seedDev(t, store)

	submit := func(msgID, convType string) domain.Task {
		t.Helper()
		task, err := store.SubmitTask(ctx, domain.SubmitTaskCommand{
			SessionKey:        dev.SessionKey,
			RequesterUserID:   1,
			Source:            "qq",
			SourceInstanceID:  "i",
			MessageID:         msgID,
			Prompt:            "hello",
			PersonaSnapshot:   []string{},
			ToolPolicyVersion: "foundation.no-host-tools.v1",
			ConversationKey:   "conv-" + msgID,
			ConversationType:  convType,
		})
		if err != nil {
			t.Fatalf("submit %s: %v", msgID, err)
		}
		return task
	}

	group := submit("group-1", domain.ConversationTypeGroup)
	if group.ConversationType != domain.ConversationTypeGroup {
		t.Fatalf("group task conversation_type=%q, want group", group.ConversationType)
	}
	empty := submit("empty-1", "")
	if empty.ConversationType != domain.ConversationTypePrivate {
		t.Fatalf("empty conversation_type normalized to %q, want private", empty.ConversationType)
	}
	bogus := submit("bogus-1", "channel")
	if bogus.ConversationType != domain.ConversationTypePrivate {
		t.Fatalf("bogus conversation_type normalized to %q, want private", bogus.ConversationType)
	}

	reloaded, err := store.GetTask(ctx, group.ID)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.ConversationType != domain.ConversationTypeGroup {
		t.Fatalf("reloaded conversation_type=%q, want group", reloaded.ConversationType)
	}
	if reloaded.StreamFinalAt != nil {
		t.Fatalf("fresh task stream_final_at=%v, want nil", reloaded.StreamFinalAt)
	}
}

// TestMarkTaskStreamFinal 验证流式最终交付标记: 置位一次、幂等、终态后
// 仍可置位(标记与终态提交无先后依赖——commit 先于 CompleteSucceeded)。
func TestMarkTaskStreamFinal(t *testing.T) {
	pool := OpenTestPool(t)
	store, err := NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	dev := seedDev(t, store)

	task, err := store.SubmitTask(ctx, domain.SubmitTaskCommand{
		SessionKey:        dev.SessionKey,
		RequesterUserID:   1,
		Source:            "feishu",
		SourceInstanceID:  "i",
		MessageID:         "stream-final-1",
		Prompt:            "hi",
		PersonaSnapshot:   []string{},
		ToolPolicyVersion: "foundation.no-host-tools.v1",
		ConversationKey:   "conv-sf",
		ConversationType:  domain.ConversationTypePrivate,
	})
	if err != nil {
		t.Fatal(err)
	}

	first, err := store.MarkTaskStreamFinal(ctx, task.ID)
	if err != nil {
		t.Fatalf("mark stream final: %v", err)
	}
	if first == nil || first.IsZero() {
		t.Fatalf("first mark returned nil/zero time: %v", first)
	}
	// 幂等: 二次置位无操作。
	second, err := store.MarkTaskStreamFinal(ctx, task.ID)
	if err != nil {
		t.Fatalf("second mark: %v", err)
	}
	if second != nil {
		t.Fatalf("second mark returned %v, want nil (no-op)", second)
	}
	reloaded, err := store.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.StreamFinalAt == nil {
		t.Fatal("stream_final_at not persisted")
	}
}

// TestIMStreamingModeSettingPersistsAndValidates 验证 im_streaming_mode
// 开关: 默认 streaming / 更新持久化 / 非法值拒绝。
func TestIMStreamingModeSettingPersistsAndValidates(t *testing.T) {
	pool := OpenTestPool(t)
	store, err := NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	if got, err := store.GetIMStreamingMode(ctx); err != nil || got != domain.DefaultIMStreamingMode {
		t.Fatalf("default mode=%s err=%v, want %s", got, err, domain.DefaultIMStreamingMode)
	}
	if got, err := store.UpdateIMStreamingMode(ctx, domain.IMStreamingFinalOnly, 1); err != nil || got != domain.IMStreamingFinalOnly {
		t.Fatalf("updated mode=%s err=%v", got, err)
	}
	if _, err := store.UpdateIMStreamingMode(ctx, domain.IMStreamingMode("banana"), 1); err == nil {
		t.Fatal("invalid mode must be rejected")
	}
	// 非法值归一(读取层面防御)。
	if got, err := store.GetIMStreamingMode(ctx); err != nil || got != domain.IMStreamingFinalOnly {
		t.Fatalf("mode after update=%s err=%v", got, err)
	}
}
