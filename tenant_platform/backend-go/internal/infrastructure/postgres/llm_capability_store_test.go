package postgres

import (
	"context"
	"testing"
	"time"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/infrastructure/llmproxy"
)

func TestCapabilityRevocationPersistsHashedJTI(t *testing.T) {
	pool := requireDB(t)
	store, err := NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	expiresAt := time.Now().UTC().Add(time.Hour)
	if err := store.RevokeCapability(ctx, "jti-sensitive", expiresAt); err != nil {
		t.Fatal(err)
	}

	revoked, err := store.IsCapabilityRevoked(ctx, llmproxy.HashJTI("jti-sensitive"))
	if err != nil {
		t.Fatal(err)
	}
	if !revoked {
		t.Fatal("expected capability to be revoked")
	}

	var rawJTIStored bool
	if err := pool.QueryRow(ctx, `
		SELECT EXISTS(
			SELECT 1 FROM llm_capability_revocations
			WHERE encode(jti_hash, 'escape') = 'jti-sensitive'
		)
	`).Scan(&rawJTIStored); err != nil {
		t.Fatal(err)
	}
	if rawJTIStored {
		t.Fatal("raw JTI was stored instead of its hash")
	}
}

func TestCapabilityRevocationCleanupDeletesOnlyExpiredRows(t *testing.T) {
	pool := requireDB(t)
	store, err := NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	now := time.Now().UTC()
	if err := store.RevokeCapability(ctx, "expired", now.Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := store.RevokeCapability(ctx, "active", now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}

	deleted, err := store.DeleteExpiredCapabilityRevocations(ctx, now)
	if err != nil {
		t.Fatal(err)
	}
	if deleted != 1 {
		t.Fatalf("deleted = %d, want 1", deleted)
	}
	active, err := store.IsCapabilityRevoked(ctx, llmproxy.HashJTI("active"))
	if err != nil {
		t.Fatal(err)
	}
	if !active {
		t.Fatal("active revocation was deleted")
	}
}
