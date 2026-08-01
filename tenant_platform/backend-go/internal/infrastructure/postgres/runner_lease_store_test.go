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

	lease, created, err := store.AcquireRunnerLease(ctx, "personal:1", "platform-a", 30*time.Minute)
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
	again, created, err := store.AcquireRunnerLease(ctx, "personal:1", "platform-a", 30*time.Minute)
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

func TestAcquireRunnerLeaseFailsForForeignOwner(t *testing.T) {
	ctx := context.Background()
	store := newChannelBindingTestStore(t)

	if _, _, err := store.AcquireRunnerLease(ctx, "personal:2", "platform-a", 30*time.Minute); err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	_, created, err := store.AcquireRunnerLease(ctx, "personal:2", "platform-b", 30*time.Minute)
	if err == nil {
		t.Fatal("foreign acquire should fail with ErrRunnerLeaseOwned")
	}
	if !errors.Is(err, ErrRunnerLeaseOwned) {
		t.Fatalf("want ErrRunnerLeaseOwned, got %v", err)
	}
	if created {
		t.Fatal("foreign owner must not create/reuse a live lease")
	}
}

func TestRunnerLeaseGenerationIncrementsAfterRelease(t *testing.T) {
	ctx := context.Background()
	store := newChannelBindingTestStore(t)

	first, _, err := store.AcquireRunnerLease(ctx, "personal:3", "platform-a", 30*time.Minute)
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	if first.Generation != 1 {
		t.Fatalf("generation = %d, want 1", first.Generation)
	}

	// 绑定 container 后释放(Runner 销毁),再获取必须递增 generation。
	if err := store.AttachRunnerContainer(ctx, first.RunnerKey, "container-abc"); err != nil {
		t.Fatalf("attach: %v", err)
	}
	if err := store.ReleaseRunnerLease(ctx, first.RunnerKey, "platform-a"); err != nil {
		t.Fatalf("release: %v", err)
	}

	second, created, err := store.AcquireRunnerLease(ctx, "personal:3", "platform-a", 30*time.Minute)
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

	lease, _, err := store.AcquireRunnerLease(ctx, "personal:4", "platform-a", 1*time.Millisecond)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	time.Sleep(5 * time.Millisecond)

	next, created, err := store.AcquireRunnerLease(ctx, "personal:4", "platform-b", 30*time.Minute)
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

	if _, _, err := store.AcquireRunnerLease(ctx, "personal:5", "platform-a", 1*time.Millisecond); err != nil {
		t.Fatalf("expiring lease: %v", err)
	}
	if _, _, err := store.AcquireRunnerLease(ctx, "personal:6", "platform-a", 30*time.Minute); err != nil {
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

	lease, _, err := store.AcquireRunnerLease(ctx, "personal:7", "platform-a", 30*time.Minute)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if err := store.AttachRunnerContainer(ctx, lease.RunnerKey, "container-xyz"); err != nil {
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
