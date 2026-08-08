package postgres

import (
	"context"
	"fmt"
	"testing"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/domain"
)

// seedUserTaskFixtures 造一个 approved 用户 + personal workspace, 返回
// (store, userID, sessionKey)。测试共享 PG 实例, 用独立 userID 避免与
// 其他测试互踩。
func seedUserTaskFixtures(t *testing.T, store *Store, userID int64) string {
	t.Helper()
	ctx := context.Background()
	sessionKey := fmt.Sprintf("personal:%d", userID)
	uidText := fmt.Sprintf("%d", userID)
	if _, err := store.pool.Exec(ctx, `
INSERT INTO users (id, username, status) VALUES ($1, 'user-task-'||$2, 'approved')
ON CONFLICT (id) DO NOTHING;
`, userID, uidText); err != nil {
		t.Fatal(err)
	}
	if _, err := store.pool.Exec(ctx, `
INSERT INTO workspaces (id, session_key, owner_user_id, kind, team_id, volume_id, bootstrap_marker)
VALUES (gen_random_uuid(), $1, $2, 'personal', NULL, 'vol-user-task-'||$3, NULL)
ON CONFLICT (session_key) DO NOTHING;
`, sessionKey, userID, uidText); err != nil {
		t.Fatal(err)
	}
	return sessionKey
}

func submitUserTask(t *testing.T, store *Store, sessionKey string, userID int64, messageID string) domain.Task {
	t.Helper()
	task, err := store.SubmitTask(context.Background(), domain.SubmitTaskCommand{
		SessionKey: sessionKey, RequesterUserID: userID, Source: "web", SourceInstanceID: "i",
		MessageID: messageID, Prompt: "p", PersonaSnapshot: []string{},
		ToolPolicyVersion: "foundation.no-host-tools.v1",
	})
	if err != nil {
		t.Fatal(err)
	}
	return task
}

func TestListMyTasksScopedToRequesterAndOrderedDesc(t *testing.T) {
	ctx := context.Background()
	store := newChannelBindingTestStore(t)

	const uid = 2001
	sessionKey := seedUserTaskFixtures(t, store, uid)

	first := submitUserTask(t, store, sessionKey, uid, "m-1")
	second := submitUserTask(t, store, sessionKey, uid, "m-2")
	// 其他用户的任务不得出现在本用户列表(租户隔离)。
	otherKey := seedUserTaskFixtures(t, store, 2002)
	_ = submitUserTask(t, store, otherKey, 2002, "m-other")

	tasks, err := store.ListMyTasks(ctx, uid, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 2 {
		t.Fatalf("want 2 tasks, got %d", len(tasks))
	}
	// 按创建时间倒序: 后提交的在前面。
	if tasks[0].ID != second.ID || tasks[1].ID != first.ID {
		t.Fatalf("order wrong: got %s, %s; want %s, %s", tasks[0].ID, tasks[1].ID, second.ID, first.ID)
	}
	if tasks[0].RequesterID != uid || tasks[0].Source != domain.SourceWeb {
		t.Fatalf("task fields wrong: requester=%d source=%s", tasks[0].RequesterID, tasks[0].Source)
	}
}

func TestListMyTasksLimitAndValidation(t *testing.T) {
	ctx := context.Background()
	store := newChannelBindingTestStore(t)

	const uid = 2003
	sessionKey := seedUserTaskFixtures(t, store, uid)
	for i := 0; i < 5; i++ {
		submitUserTask(t, store, sessionKey, uid, "m-limit-"+string(rune('a'+i)))
	}

	tasks, err := store.ListMyTasks(ctx, uid, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 2 {
		t.Fatalf("limit=2: want 2 tasks, got %d", len(tasks))
	}
	if _, err := store.ListMyTasks(ctx, 0, 10); err == nil {
		t.Fatal("expected error for non-positive user id")
	}
}

func TestCountMyTaskStatsGroupsByStatus(t *testing.T) {
	ctx := context.Background()
	store := newChannelBindingTestStore(t)

	const uid = 2004
	sessionKey := seedUserTaskFixtures(t, store, uid)

	queued := submitUserTask(t, store, sessionKey, uid, "m-q1")
	submitUserTask(t, store, sessionKey, uid, "m-q2")
	starting := submitUserTask(t, store, sessionKey, uid, "m-s1")
	succeeded := submitUserTask(t, store, sessionKey, uid, "m-d1")

	// 调整状态: queued(2 个保留) / starting / succeeded。
	if _, err := store.pool.Exec(ctx, `
UPDATE tasks SET status='starting', claim_owner='test-owner', claimed_at=timezone('utc', now()),
 claim_lease_until=timezone('utc', now()) + interval '10 minute' WHERE id=$1
`, starting.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.pool.Exec(ctx, `
UPDATE tasks SET status='succeeded', terminal_at=timezone('utc', now()),
 succeeded_at=timezone('utc', now()) WHERE id=$1
`, succeeded.ID); err != nil {
		t.Fatal(err)
	}
	_ = queued // 其余两个保持 queued

	stats, err := store.CountMyTaskStats(ctx, uid)
	if err != nil {
		t.Fatal(err)
	}
	if stats[domain.TaskQueued] != 2 {
		t.Fatalf("queued = %d, want 2", stats[domain.TaskQueued])
	}
	if stats[domain.TaskStarting] != 1 {
		t.Fatalf("starting = %d, want 1", stats[domain.TaskStarting])
	}
	if stats[domain.TaskSucceeded] != 1 {
		t.Fatalf("succeeded = %d, want 1", stats[domain.TaskSucceeded])
	}
	if _, ok := stats[domain.TaskRunning]; ok {
		t.Fatal("running should be absent (zero count)")
	}
	if _, err := store.CountMyTaskStats(ctx, 0); err == nil {
		t.Fatal("expected error for non-positive user id")
	}
}
