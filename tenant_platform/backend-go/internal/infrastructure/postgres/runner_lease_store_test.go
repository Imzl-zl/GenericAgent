package postgres

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/domain"
)

func TestAcquireRunnerLeaseCreatesAndReuses(t *testing.T) {
	ctx := context.Background()
	store := newChannelBindingTestStore(t)

	lease, created, err := store.AcquireRunnerLease(ctx, "personal:1", "platform-a", 30*time.Minute, 0)
	if err != nil {
		t.Fatalf("AcquireRunnerLease: %v", err)
	}
	if !created {
		t.Fatal("first acquire should create")
	}
	if lease.RunnerKey != "personal:1" || lease.Owner != "platform-a" || lease.Generation != 1 {
		t.Fatalf("unexpected lease: %+v", lease)
	}
	if lease.ContainerID != "" {
		t.Fatalf("new lease should have no container yet: %+v", lease)
	}

	// 同 owner 复用:不新增 generation,返回同一 lease。
	again, created, err := store.AcquireRunnerLease(ctx, "personal:1", "platform-a", 30*time.Minute, 0)
	if err != nil {
		t.Fatalf("re-acquire: %v", err)
	}
	if created {
		t.Fatal("re-acquire by same owner should reuse")
	}
	if again.Generation != 1 {
		t.Fatalf("reuse must keep generation, got %d", again.Generation)
	}
}

func TestAcquireRunnerLeaseFailsForForeignOwnerWithActiveTask(t *testing.T) {
	ctx := context.Background()
	store := newChannelBindingTestStore(t)

	if _, _, err := store.AcquireRunnerLease(ctx, "personal:2", "platform-a", 30*time.Minute, 0); err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	// 异主 + owner 仍有活跃 task claim: 必须拒绝(多实例同时运行同一 Runner)。
	seedActiveClaim(t, store, "personal:2", "platform-a")
	_, created, err := store.AcquireRunnerLease(ctx, "personal:2", "platform-b", 30*time.Minute, 0)
	if err == nil {
		t.Fatal("foreign acquire with live owner task must fail with ErrRunnerLeaseOwned")
	}
	if !errors.Is(err, ErrRunnerLeaseOwned) {
		t.Fatalf("want ErrRunnerLeaseOwned, got %v", err)
	}
	if created {
		t.Fatal("foreign owner must not create/reuse a live lease")
	}
}

func TestAcquireRunnerLeaseForeignOwnerTakeoverWithoutActiveTask(t *testing.T) {
	ctx := context.Background()
	store := newChannelBindingTestStore(t)

	first, _, err := store.AcquireRunnerLease(ctx, "personal:7", "platform-a", 30*time.Minute, 0)
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	if first.Generation != 1 {
		t.Fatalf("first generation = %d, want 1", first.Generation)
	}
	// 绑定容器后异主接管: 旧容器必须移入 stale_container_id 供定向销毁,
	// 新 generation 的 container_id 保持为空(round10 审查 B2: 重启后新
	// processID 接管旧进程 lease 时, 旧 Runner 必须被销毁重建并注入新 CA)。
	if err := store.AttachRunnerContainer(ctx, first.RunnerKey, "old-container-1", first.Generation, "platform-a"); err != nil {
		t.Fatalf("attach: %v", err)
	}
	// 异主 + owner 无活跃 task claim(崩溃/长期停机): 允许接管, generation +1。
	// 审查: 接管必须返回完整 lease 行(generation>0), 否则下游签发零值。
	taken, created, err := store.AcquireRunnerLease(ctx, "personal:7", "platform-b", 30*time.Minute, 0)
	if err != nil {
		t.Fatalf("takeover acquire: %v", err)
	}
	if !created {
		t.Fatal("takeover must report created")
	}
	if taken.Generation != 2 {
		t.Fatalf("takeover generation = %d, want 2 (monotonic +1)", taken.Generation)
	}
	if taken.Owner != "platform-b" {
		t.Fatalf("takeover owner = %q, want platform-b", taken.Owner)
	}
	if taken.StaleContainerID != "old-container-1" {
		t.Fatalf("takeover stale_container_id = %q, want old-container-1", taken.StaleContainerID)
	}
	if taken.ContainerID != "" {
		t.Fatalf("takeover container_id = %q, want empty", taken.ContainerID)
	}
}

// seedActiveClaim 为 session_key 插入一条 claim_owner 持有且未过期的
// starting 任务, 使异主 lease 获取被 ErrRunnerLeaseOwned 拒绝。
func seedActiveClaim(t *testing.T, store *Store, sessionKey, claimOwner string) {
	t.Helper()
	ctx := context.Background()
	if _, err := store.pool.Exec(ctx, `
INSERT INTO users (id, username, status) VALUES (100, 'lease-owner', 'approved')
ON CONFLICT (id) DO NOTHING;
`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.pool.Exec(ctx, `
INSERT INTO workspaces (id, session_key, owner_user_id, kind, team_id, volume_id, bootstrap_marker)
VALUES ('00000000-0000-4000-8000-000000000064', $1, 100, 'personal', NULL, 'vol-lease-owner', NULL)
ON CONFLICT (session_key) DO NOTHING;
`, sessionKey); err != nil {
		t.Fatal(err)
	}
	task, err := store.SubmitTask(ctx, domain.SubmitTaskCommand{
		SessionKey: sessionKey, RequesterUserID: 100, Source: "web", SourceInstanceID: "i",
		MessageID: "lease-owner", Prompt: "p", PersonaSnapshot: []string{},
		ToolPolicyVersion: "foundation.no-host-tools.v1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.pool.Exec(ctx, `
UPDATE tasks SET status='starting', claim_owner=$2, claimed_at=timezone('utc', now()),
 claim_lease_until=timezone('utc', now()) + interval '10 minute'
WHERE id=$1
`, task.ID, claimOwner); err != nil {
		t.Fatal(err)
	}
}

func TestRunnerLeaseGenerationIncrementsAfterRelease(t *testing.T) {
	ctx := context.Background()
	store := newChannelBindingTestStore(t)

	first, _, err := store.AcquireRunnerLease(ctx, "personal:3", "platform-a", 30*time.Minute, 0)
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	if first.Generation != 1 {
		t.Fatalf("generation = %d, want 1", first.Generation)
	}

	// 绑定 container 后释放(Runner 销毁),再获取必须递增 generation。
	if err := store.AttachRunnerContainer(ctx, first.RunnerKey, "container-abc", first.Generation, first.Owner); err != nil {
		t.Fatalf("attach: %v", err)
	}
	if err := store.ReleaseRunnerLease(ctx, first.RunnerKey, "platform-a", first.Generation); err != nil {
		t.Fatalf("release: %v", err)
	}

	// round10 审查(B1): release 必须同时清空容器字段——残留的 container_id
	// 会在下次接管时进入 stale_container_id, 对已删除容器定向销毁失败导致
	// 该工作区永久无法重建 Runner。
	released, err := store.GetRunnerLease(ctx, first.RunnerKey)
	if err != nil {
		t.Fatalf("get released lease: %v", err)
	}
	if released.ContainerID != "" || released.StaleContainerID != "" || released.ControlEndpoint != "" {
		t.Fatalf("release must clear container fields: container=%q stale=%q endpoint=%q",
			released.ContainerID, released.StaleContainerID, released.ControlEndpoint)
	}

	second, created, err := store.AcquireRunnerLease(ctx, "personal:3", "platform-a", 30*time.Minute, 0)
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if !created {
		t.Fatal("after release, acquire should create a new generation")
	}
	if second.Generation != 2 {
		t.Fatalf("generation = %d, want 2 (monotonic)", second.Generation)
	}
	if second.ContainerID != "" {
		t.Fatalf("new generation must not inherit container id")
	}
}

func TestExpiredRunnerLeaseCanBeReacquiredWithNextGeneration(t *testing.T) {
	ctx := context.Background()
	store := newChannelBindingTestStore(t)

	lease, _, err := store.AcquireRunnerLease(ctx, "personal:4", "platform-a", 1*time.Millisecond, 0)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	time.Sleep(5 * time.Millisecond)

	next, created, err := store.AcquireRunnerLease(ctx, "personal:4", "platform-b", 30*time.Minute, 0)
	if err != nil {
		t.Fatalf("acquire after expiry: %v", err)
	}
	if !created {
		t.Fatal("expired lease should be replaceable")
	}
	if next.Generation != lease.Generation+1 {
		t.Fatalf("generation = %d, want %d", next.Generation, lease.Generation+1)
	}
	if next.Owner != "platform-b" {
		t.Fatalf("owner = %q, want platform-b", next.Owner)
	}
}

func TestListExpiredRunnerLeasesOnlyReturnsExpired(t *testing.T) {
	ctx := context.Background()
	store := newChannelBindingTestStore(t)

	if _, _, err := store.AcquireRunnerLease(ctx, "personal:5", "platform-a", 1*time.Millisecond, 0); err != nil {
		t.Fatalf("expiring lease: %v", err)
	}
	if _, _, err := store.AcquireRunnerLease(ctx, "personal:6", "platform-a", 30*time.Minute, 0); err != nil {
		t.Fatalf("live lease: %v", err)
	}
	time.Sleep(5 * time.Millisecond)

	expired, err := store.ListExpiredRunnerLeases(ctx, time.Now().UTC())
	if err != nil {
		t.Fatalf("ListExpiredRunnerLeases: %v", err)
	}
	seen := map[string]bool{}
	for _, l := range expired {
		seen[l.RunnerKey] = true
	}
	if !seen["personal:5"] {
		t.Fatalf("personal:5 lease should be expired, got %v", seen)
	}
	if seen["personal:6"] {
		t.Fatal("live lease must not appear in expired list")
	}
}

func TestRunnerLeaseFieldsRoundTrip(t *testing.T) {
	ctx := context.Background()
	store := newChannelBindingTestStore(t)

	lease, _, err := store.AcquireRunnerLease(ctx, "personal:7", "platform-a", 30*time.Minute, 0)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if err := store.AttachRunnerContainer(ctx, lease.RunnerKey, "container-xyz", lease.Generation, lease.Owner); err != nil {
		t.Fatalf("attach: %v", err)
	}
	if err := store.SetRunnerControlEndpoint(ctx, lease.RunnerKey, "https://runner-ctrl:9443"); err != nil {
		t.Fatalf("set endpoint: %v", err)
	}

	got, err := store.GetRunnerLease(ctx, "personal:7")
	if err != nil {
		t.Fatalf("GetRunnerLease: %v", err)
	}
	if got.ContainerID != "container-xyz" {
		t.Fatalf("container = %q", got.ContainerID)
	}
	if got.ControlEndpoint != "https://runner-ctrl:9443" {
		t.Fatalf("endpoint = %q", got.ControlEndpoint)
	}
	if got.Generation != 1 || got.Owner != "platform-a" {
		t.Fatalf("unexpected lease: %+v", got)
	}
	if !got.ExpiresAt.After(time.Now().UTC()) {
		t.Fatal("lease should be unexpired")
	}
	if got.Status != domain.RunnerLeaseActive {
		t.Fatalf("status = %q, want active", got.Status)
	}
}

// 审查 R5-I6: AttachRunnerContainer 必须由 lease owner 在有效期内执行,
// 且不可覆盖已有 container_id(不可变容器身份)——旧 generation/异主/过期
// 的 attach 不得改写当前 lease。
func TestAttachRunnerContainerFencesOwnerLeaseAndImmutableID(t *testing.T) {
	ctx := context.Background()
	store := newChannelBindingTestStore(t)

	lease, _, err := store.AcquireRunnerLease(ctx, "personal:8", "platform-a", 30*time.Minute, 0)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if err := store.AttachRunnerContainer(ctx, lease.RunnerKey, "container-1", lease.Generation, "platform-a"); err != nil {
		t.Fatalf("owner attach: %v", err)
	}
	// 异主 attach 必须拒绝。
	if err := store.AttachRunnerContainer(ctx, lease.RunnerKey, "container-2", lease.Generation, "platform-b"); err == nil {
		t.Fatal("foreign owner attach must fail")
	}
	// 同 owner 覆盖已有 container_id 必须拒绝(不可变身份)。
	if err := store.AttachRunnerContainer(ctx, lease.RunnerKey, "container-3", lease.Generation, "platform-a"); err == nil {
		t.Fatal("overwriting container id must fail")
	}
	// 同值幂等 attach 允许(重试安全)。
	if err := store.AttachRunnerContainer(ctx, lease.RunnerKey, "container-1", lease.Generation, "platform-a"); err != nil {
		t.Fatalf("idempotent re-attach: %v", err)
	}
	// lease 过期后 attach 必须拒绝。
	if _, err := store.pool.Exec(ctx, `UPDATE runner_leases SET expires_at = timezone('utc', now()) - interval '1 second' WHERE runner_key = $1`, lease.RunnerKey); err != nil {
		t.Fatal(err)
	}
	if err := store.AttachRunnerContainer(ctx, lease.RunnerKey, "container-1", lease.Generation, "platform-a"); err == nil {
		t.Fatal("attach after lease expiry must fail")
	}
}
