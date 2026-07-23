package postgres

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Shared test DB mutex: foundation tests share one PostgreSQL database.
var testDBMu sync.Mutex

// OpenTestPool returns a pool with exclusive schema reset+migrate for this process.
func OpenTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := strings.TrimSpace(os.Getenv("TEST_DATABASE_URL"))
	if url == "" {
		t.Fatal("TEST_DATABASE_URL is required (no SQLite fallback)")
	}
	testDBMu.Lock()
	t.Cleanup(func() { testDBMu.Unlock() })

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)

	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock(87236401)`); err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = conn.Exec(ctx, `SELECT pg_advisory_unlock(87236401)`) }()

	if err := resetAndMigrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	return pool
}

func resetAndMigrate(ctx context.Context, pool *pgxpool.Pool) error {
	if err := DropFoundationSchema(ctx, pool); err != nil {
		return fmt.Errorf("drop: %w", err)
	}
	// ApplyMigrations also drops; call the raw SQL path only.
	path := DefaultMigrationPath()
	raw, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read migration: %w", err)
	}
	if _, err := pool.Exec(ctx, string(raw)); err != nil {
		// One retry after force-clean orphan types.
		_ = DropFoundationSchema(ctx, pool)
		if _, err2 := pool.Exec(ctx, string(raw)); err2 != nil {
			return fmt.Errorf("apply migration: %w (retry: %v)", err, err2)
		}
	}
	return nil
}
