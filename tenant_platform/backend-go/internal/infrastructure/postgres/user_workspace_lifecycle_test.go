package postgres

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/domain"
)

// TestCreateUserCreatesPersonalWorkspace 锁定生命周期不变量:
// users 行 ⇔ workspaces 存在 session_key='personal:<uid>' 行。
// 用户创建路径(CreateUser)必须同事务建立 workspace 行, 否则审批通过后
// 提交任务必因 workspace not found 失败(ROUTER_ERROR 500)。
func TestCreateUserCreatesPersonalWorkspace(t *testing.T) {
	pool := OpenTestPool(t)
	store, err := NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	u, err := store.CreateUser(ctx, "ws-invariant-user", "hash")
	if err != nil {
		t.Fatal(err)
	}
	assertPersonalWorkspace(t, pool, u.ID)
}

// TestInviteRegistrationCreatesPersonalWorkspace 验证邀请注册路径同样建行。
func TestInviteRegistrationCreatesPersonalWorkspace(t *testing.T) {
	pool := OpenTestPool(t)
	store, err := NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	dev, err := store.EnsureAdminContext(ctx, 9, "invite-ws-dev")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	code := fmt.Sprintf("WS-%d", now.UnixNano())
	if _, err := store.CreateInviteCode(ctx, code, dev.UserID, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	u, err := store.CreateUserWithInvite(ctx, "invite-ws-user", "hash", code, "invite-ws-token", now, now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	assertPersonalWorkspace(t, pool, u.ID)
}

// TestApproveUserKeepsWorkspaceSingle 验证审批是纯状态迁移: 不重复建行、
// 不删行, 行数与 volume_id 在审批前后一致。
func TestApproveUserKeepsWorkspaceSingle(t *testing.T) {
	pool := OpenTestPool(t)
	store, err := NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	u, err := store.CreateUser(ctx, "ws-approve-user", "hash")
	if err != nil {
		t.Fatal(err)
	}
	wsKey := fmt.Sprintf("personal:%d", u.ID)
	hash, err := domain.WorkspaceDirHash(wsKey)
	if err != nil {
		t.Fatal(err)
	}
	if got := countWorkspaceRows(t, pool, wsKey); got != 1 {
		t.Fatalf("workspace rows before approve = %d, want 1", got)
	}

	au, err := store.ApproveUser(ctx, u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if au.Status != domain.UserApproved {
		t.Fatalf("status = %s, want approved", au.Status)
	}
	if got := countWorkspaceRows(t, pool, wsKey); got != 1 {
		t.Fatalf("workspace rows after approve = %d, want 1 (approve must not duplicate rows)", got)
	}
	var vol *string
	if err := pool.QueryRow(ctx, `SELECT volume_id FROM workspaces WHERE session_key = $1`, wsKey).Scan(&vol); err != nil {
		t.Fatal(err)
	}
	if vol == nil || *vol != hash {
		t.Fatalf("volume_id after approve = %v, want %s", vol, hash)
	}
}

// TestEnsureAdminContextBootstrapWorkspace 验证 bootstrap 路径复用同一
// helper: 行存在, bootstrap_marker 非空, volume_id 为 NULL(共享卷由
// WorkspaceCoordinator 首调写入)。
func TestEnsureAdminContextBootstrapWorkspace(t *testing.T) {
	pool := OpenTestPool(t)
	store, err := NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	dev, err := store.EnsureAdminContext(ctx, 77, "ws-bootstrap-dev")
	if err != nil {
		t.Fatal(err)
	}
	if dev.SessionKey != "personal:77" {
		t.Fatalf("session key = %s, want personal:77", dev.SessionKey)
	}
	var marker, vol *string
	if err := pool.QueryRow(ctx, `SELECT bootstrap_marker, volume_id FROM workspaces WHERE session_key = $1`, dev.SessionKey).Scan(&marker, &vol); err != nil {
		t.Fatal(err)
	}
	if marker == nil || *marker != "dev-loopback" {
		t.Fatalf("bootstrap_marker = %v, want dev-loopback", marker)
	}
	if vol != nil {
		t.Fatalf("volume_id = %v, want NULL for bootstrap workspace", vol)
	}
}

// assertPersonalWorkspace 断言 personal:<uid> 行存在且 volume_id 等于
// domain.WorkspaceDirHash(与运行时共享卷路径同源), bootstrap_marker 为 NULL。
func assertPersonalWorkspace(t *testing.T, pool *pgxpool.Pool, userID int64) {
	t.Helper()
	wsKey := fmt.Sprintf("personal:%d", userID)
	hash, err := domain.WorkspaceDirHash(wsKey)
	if err != nil {
		t.Fatal(err)
	}
	var vol *string
	var marker *string
	var kind string
	if err := pool.QueryRow(context.Background(), `
SELECT kind, volume_id, bootstrap_marker FROM workspaces WHERE session_key = $1
`, wsKey).Scan(&kind, &vol, &marker); err != nil {
		t.Fatalf("personal workspace %s not created: %v", wsKey, err)
	}
	if kind != "personal" {
		t.Fatalf("kind = %s, want personal", kind)
	}
	if vol == nil || *vol != hash {
		t.Fatalf("volume_id = %v, want %s", vol, hash)
	}
	if marker != nil {
		t.Fatalf("bootstrap_marker = %v, want NULL for regular user", marker)
	}
}

func countWorkspaceRows(t *testing.T, pool *pgxpool.Pool, sessionKey string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), `SELECT COUNT(*) FROM workspaces WHERE session_key = $1`, sessionKey).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}
