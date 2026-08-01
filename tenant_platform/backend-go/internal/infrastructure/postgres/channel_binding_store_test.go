package postgres

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/domain"
)

func TestBindChannelAccountAndResolve(t *testing.T) {
	ctx := context.Background()
	pool := newChannelBindingTestStore(t)

	userID := createTestChannelUser(t, ctx, pool)
	binding, err := pool.BindChannelAccount(ctx, "wechat", "wx_account_1", userID)
	if err != nil {
		t.Fatalf("BindChannelAccount: %v", err)
	}
	if binding.ChannelType != "wechat" || binding.ChannelAccountID != "wx_account_1" || binding.CanonicalUserID != userID {
		t.Fatalf("unexpected binding: %+v", binding)
	}

	resolved, err := pool.ResolveCanonicalUserID(ctx, "wechat", "wx_account_1")
	if err != nil {
		t.Fatalf("ResolveCanonicalUserID: %v", err)
	}
	if resolved != userID {
		t.Fatalf("resolved user = %d, want %d", resolved, userID)
	}
}

func TestResolveUnknownChannelAccount(t *testing.T) {
	ctx := context.Background()
	pool := newChannelBindingTestStore(t)

	_, err := pool.ResolveCanonicalUserID(ctx, "wechat", "never_bound_account")
	if !errors.Is(err, domain.ErrChannelBindingNotFound) {
		t.Fatalf("want ErrChannelBindingNotFound, got %v", err)
	}
}

func TestRebindChannelAccountMovesToNewUser(t *testing.T) {
	ctx := context.Background()
	pool := newChannelBindingTestStore(t)

	firstUser := createTestChannelUser(t, ctx, pool)
	secondUser := createTestChannelUser(t, ctx, pool)

	if _, err := pool.BindChannelAccount(ctx, "qq", "qq_account_9", firstUser); err != nil {
		t.Fatalf("first bind: %v", err)
	}
	if _, err := pool.BindChannelAccount(ctx, "qq", "qq_account_9", secondUser); err != nil {
		t.Fatalf("rebind: %v", err)
	}

	resolved, err := pool.ResolveCanonicalUserID(ctx, "qq", "qq_account_9")
	if err != nil {
		t.Fatalf("ResolveCanonicalUserID: %v", err)
	}
	if resolved != secondUser {
		t.Fatalf("resolved user = %d, want %d (rebound)", resolved, secondUser)
	}
}

func TestDifferentChannelsSameAccountAreDistinct(t *testing.T) {
	ctx := context.Background()
	pool := newChannelBindingTestStore(t)

	userA := createTestChannelUser(t, ctx, pool)
	userB := createTestChannelUser(t, ctx, pool)

	if _, err := pool.BindChannelAccount(ctx, "wechat", "shared_account", userA); err != nil {
		t.Fatalf("bind wechat: %v", err)
	}
	if _, err := pool.BindChannelAccount(ctx, "qq", "shared_account", userB); err != nil {
		t.Fatalf("bind qq: %v", err)
	}

	wechat, err := pool.ResolveCanonicalUserID(ctx, "wechat", "shared_account")
	if err != nil || wechat != userA {
		t.Fatalf("wechat resolved = %d, err = %v; want %d", wechat, err, userA)
	}
	qq, err := pool.ResolveCanonicalUserID(ctx, "qq", "shared_account")
	if err != nil || qq != userB {
		t.Fatalf("qq resolved = %d, err = %v; want %d", qq, err, userB)
	}
}

func createTestChannelUser(t *testing.T, ctx context.Context, pool *Store) int64 {
	t.Helper()
	user, err := pool.CreateUser(ctx, "chan-"+uuid.NewString(), "hash")
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	return user.ID
}

func newChannelBindingTestStore(t *testing.T) *Store {
	t.Helper()
	store, err := NewStore(OpenTestPool(t))
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	return store
}
