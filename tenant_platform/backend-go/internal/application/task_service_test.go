package application

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/domain"
	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/infrastructure/policy"
	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/infrastructure/postgres"
)

func foundationPolicy(t *testing.T) string {
	t.Helper()
	_, file, _, _ := runtime.Caller(0)
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", "..", "..", "contracts", "policy", "foundation.v1.json"))
}

func serviceFixture(t *testing.T) (TaskService, *postgres.Store, policy.Registry, postgres.AdminContext) {
	t.Helper()
	pool := postgres.OpenTestPool(t)
	store, err := postgres.NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	reg, err := policy.LoadRegistry(foundationPolicy(t))
	if err != nil {
		t.Fatal(err)
	}
	dev, err := store.EnsureAdminContext(context.Background(), 1, "dev")
	if err != nil {
		t.Fatal(err)
	}
	svc, err := NewTaskService(TaskServiceConfig{
		Store: store, Registry: reg, PlatformInstanceID: "svc-instance", ClaimLease: time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	return svc, store, reg, dev
}

func TestTaskService_RejectsUnknownPolicy(t *testing.T) {
	svc, _, _, dev := serviceFixture(t)
	_, err := svc.SubmitTask(context.Background(), domain.SubmitTaskCommand{
		SessionKey: dev.SessionKey, RequesterUserID: 1,
		Source: "web", SourceInstanceID: "i", MessageID: "m",
		Prompt: "x", PersonaSnapshot: []string{}, ToolPolicyVersion: "host-tools.v1",
	})
	if err == nil {
		t.Fatal("expected policy rejection")
	}
}

func TestTaskService_ClaimRoundTripsDurableEnvelope(t *testing.T) {
	svc, store, _, dev := serviceFixture(t)
	ctx := context.Background()
	prompt := "durable-prompt-value"
	persona := []string{"persona-A", "persona-B"}
	task, err := svc.SubmitTask(ctx, domain.SubmitTaskCommand{
		SessionKey: dev.SessionKey, RequesterUserID: 1,
		Source: "web", SourceInstanceID: "i", MessageID: "m-rt",
		Prompt: prompt, PersonaSnapshot: persona, ToolPolicyVersion: "foundation.no-host-tools.v1",
	})
	if err != nil {
		t.Fatal(err)
	}
	claimed, ok, err := store.ClaimNextTask(ctx, dev.SessionKey, "new-proc", time.Minute)
	if err != nil || !ok {
		t.Fatalf("claim: ok=%v err=%v", ok, err)
	}
	if claimed.ID != task.ID {
		t.Fatal("id")
	}
	if claimed.Prompt != prompt {
		t.Fatalf("prompt=%q", claimed.Prompt)
	}
	if len(claimed.PersonaSnapshot) != 2 || claimed.PersonaSnapshot[0] != "persona-A" {
		t.Fatalf("persona=%v", claimed.PersonaSnapshot)
	}
	if claimed.ToolPolicyVersion != "foundation.no-host-tools.v1" {
		t.Fatalf("policy=%s", claimed.ToolPolicyVersion)
	}
}

func TestNewScheduler_RejectsEmptyIDAndLease(t *testing.T) {
	_, store, reg, _ := serviceFixture(t)
	if _, err := NewScheduler(SchedulerConfig{PlatformInstanceID: "", ClaimLease: time.Second, Store: store, Registry: reg}); err == nil {
		t.Fatal("empty id")
	}
	if _, err := NewScheduler(SchedulerConfig{PlatformInstanceID: "x", ClaimLease: 0, Store: store, Registry: reg}); err == nil {
		t.Fatal("zero lease")
	}
}

// TestTaskService_SubmitSessionGate 验证审查 I-4 提交门禁:
// pending 用户被拒(与 capability 在线校验一致), 跨用户 personal 会话被拒,
// approved 用户可向自己会话提交。
func TestTaskService_SubmitSessionGate(t *testing.T) {
	svc, store, reg, dev := serviceFixture(t)
	ctx := context.Background()
	// 注意: 不能二次调用 OpenTestPool(全局互斥锁, 同测试内死锁);
	// 用 pgx 单连接直插测试数据。
	conn, err := pgx.Connect(ctx, os.Getenv("TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	const pendingID int64 = 4242
	if _, err := conn.Exec(ctx, `
INSERT INTO users (id, username, status) VALUES ($1, 'pending-user', 'pending')
`, pendingID); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, `
INSERT INTO workspaces (id, session_key, owner_user_id, kind, volume_id)
VALUES (gen_random_uuid(), 'personal:4242', 4242, 'personal', 'test-vol')
`); err != nil {
		t.Fatal(err)
	}
	base := domain.SubmitTaskCommand{
		Source: "web", SourceInstanceID: "i", MessageID: "m",
		Prompt: "x", PersonaSnapshot: []string{}, ToolPolicyVersion: DefaultToolPolicyVersion,
	}
	// pending 用户提交到自己的会话 → 拒绝(非 approved)。
	cmd := base
	cmd.SessionKey = "personal:4242"
	cmd.RequesterUserID = pendingID
	if _, err := svc.SubmitTask(ctx, cmd); err == nil {
		t.Fatal("pending user submit must be rejected")
	}
	// approved 用户(dev)提交到他人 personal 会话 → 拒绝(会话归属)。
	cmd = base
	cmd.SessionKey = "personal:4242"
	cmd.RequesterUserID = dev.UserID
	if _, err := svc.SubmitTask(ctx, cmd); err == nil {
		t.Fatal("cross-user session submit must be rejected")
	}
	// 非法 session 格式 → 拒绝。
	cmd = base
	cmd.SessionKey = "workspace:1"
	cmd.RequesterUserID = dev.UserID
	if _, err := svc.SubmitTask(ctx, cmd); err == nil {
		t.Fatal("unsupported session key must be rejected")
	}
	// approved 用户向自己会话提交 → 接受。
	cmd = base
	cmd.SessionKey = dev.SessionKey
	cmd.RequesterUserID = dev.UserID
	if _, err := svc.SubmitTask(ctx, cmd); err != nil {
		t.Fatalf("own session submit: %v", err)
	}
	// 引用 store/reg 防未使用(编译期保证接口实现)。
	_ = store
	_ = reg
}
