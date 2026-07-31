package postgres

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/domain"
)

func TestNewStoreRejectsInvalidDocumentPoolDeploymentLimit(t *testing.T) {
	if _, err := NewStore(&pgxpool.Pool{}, WithDocumentPoolDeploymentMaxActive(0)); err == nil {
		t.Fatal("expected invalid document pool deployment limit to fail")
	}
}

func TestDocumentPoolSettingsPersistAtomicallyWithCAS(t *testing.T) {
	pool := requireDB(t)
	store, err := NewStore(pool, WithDocumentPoolDeploymentMaxActive(4))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	initial, err := store.GetDocumentPoolSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if initial.Enabled || initial.MaxActive != 1 || initial.MinReady != 0 || initial.Version != 1 {
		t.Fatalf("initial=%+v", initial)
	}

	update := initial
	update.Enabled = true
	update.MaxActive = 2
	update.MinReady = 1
	update.PerTenantActiveLimit = 1
	stored, err := store.UpdateDocumentPoolSettings(ctx, update, initial.Version, 42, "enable document processing")
	if err != nil {
		t.Fatal(err)
	}
	if stored.Version != 2 || stored.UpdatedBy != 42 || stored.Reason != "enable document processing" || !stored.Enabled || stored.MaxActive != 2 {
		t.Fatalf("stored=%+v", stored)
	}

	stale := update
	stale.MaxActive = 3
	if _, err := store.UpdateDocumentPoolSettings(ctx, stale, initial.Version, 43, "stale write"); !errors.Is(err, domain.ErrDocumentPoolSettingsConflict) {
		t.Fatalf("stale update err=%v", err)
	}
	current, err := store.GetDocumentPoolSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if current.Version != 2 || current.MaxActive != 2 || current.UpdatedBy != 42 {
		t.Fatalf("partial/stale mutation detected: %+v", current)
	}
}

func TestDocumentPoolSettingsStoreEnforcesDeploymentLimit(t *testing.T) {
	pool := requireDB(t)
	store, err := NewStore(pool, WithDocumentPoolDeploymentMaxActive(2))
	if err != nil {
		t.Fatal(err)
	}
	initial, err := store.GetDocumentPoolSettings(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	initial.Enabled = true
	initial.MaxActive = 3
	initial.PerTenantActiveLimit = 1
	if _, err := store.UpdateDocumentPoolSettings(context.Background(), initial, initial.Version, 42, "exceeds deployment limit"); err == nil {
		t.Fatal("expected deployment limit validation error")
	}
	if _, err := pool.Exec(context.Background(), `UPDATE document_pool_settings SET max_active = 3 WHERE singleton = TRUE`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.GetDocumentPoolSettings(context.Background()); err == nil {
		t.Fatal("expected persisted settings above deployment limit to be rejected")
	}
}
