package postgres

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
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

	if err := resetAndMigrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	return pool
}

func resetAndMigrate(ctx context.Context, pool *pgxpool.Pool) error {
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()

	tx, err := conn.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(87236401)`); err != nil {
		return err
	}
	for _, name := range foundationTableNames {
		if _, err := tx.Exec(ctx, fmt.Sprintf(`DROP TABLE IF EXISTS %s CASCADE`, name)); err != nil {
			return fmt.Errorf("drop table %s: %w", name, err)
		}
	}
	for _, name := range foundationTableNames {
		if _, err := tx.Exec(ctx, fmt.Sprintf(`DROP TYPE IF EXISTS %s CASCADE`, name)); err != nil {
			return fmt.Errorf("drop type %s: %w", name, err)
		}
	}
	if _, err := tx.Exec(ctx, `DROP SEQUENCE IF EXISTS task_events_id_seq CASCADE`); err != nil {
		return err
	}

	dir := migrationsDir()
	for _, name := range migrationFiles() {
		path := filepath.Join(dir, name)
		raw, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("read migration %s: %w", name, err)
		}
		if _, err := tx.Exec(ctx, string(raw)); err != nil {
			return fmt.Errorf("apply migration %s: %w", name, err)
		}
	}
	return tx.Commit(ctx)
}
