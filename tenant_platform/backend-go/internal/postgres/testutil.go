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

	// Serialize schema rebuild against other connections using an advisory lock.
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
	// Drop in a single batch with CASCADE; ignore missing.
	if _, err := pool.Exec(ctx, `
DROP TABLE IF EXISTS task_deliveries CASCADE;
DROP TABLE IF EXISTS task_events CASCADE;
DROP TABLE IF EXISTS workspace_snapshots CASCADE;
DROP TABLE IF EXISTS tasks CASCADE;
DROP TABLE IF EXISTS workspaces CASCADE;
DROP TABLE IF EXISTS users CASCADE;
`); err != nil {
		return fmt.Errorf("drop: %w", err)
	}
	// Ensure no orphaned types remain.
	if _, err := pool.Exec(ctx, `
DO $$ DECLARE r RECORD;
BEGIN
  FOR r IN (
    SELECT c.relname AS name
    FROM pg_class c
    JOIN pg_namespace n ON n.oid = c.relnamespace
    WHERE n.nspname = 'public' AND c.relkind = 'r'
      AND c.relname IN ('users','workspaces','tasks','task_events','task_deliveries','workspace_snapshots')
  ) LOOP
    EXECUTE 'DROP TABLE IF EXISTS public.' || quote_ident(r.name) || ' CASCADE';
  END LOOP;
END $$;
`); err != nil {
		return fmt.Errorf("force drop: %w", err)
	}
	return ApplyMigrations(ctx, pool, "")
}
