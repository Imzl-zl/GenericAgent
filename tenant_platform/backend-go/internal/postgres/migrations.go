package postgres

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
)

// DefaultMigrationPath returns the checked-in 0001_foundation.sql path relative to this module.
func DefaultMigrationPath() string {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		return ""
	}
	// backend-go/internal/postgres -> tenant_platform/infra/postgres/migrations
	root := filepath.Clean(filepath.Join(filepath.Dir(file), "..", "..", ".."))
	return filepath.Join(root, "infra", "postgres", "migrations", "0001_foundation.sql")
}

// foundationTableNames are dropped before re-applying the migration.
var foundationTableNames = []string{
	"task_deliveries",
	"task_events",
	"workspace_snapshots",
	"tasks",
	"workspaces",
	"users",
}

// DropFoundationSchema removes foundation tables and leftover composite types.
// Safe to call when objects are missing. Uses CASCADE.
func DropFoundationSchema(ctx context.Context, pool *pgxpool.Pool) error {
	if pool == nil {
		return fmt.Errorf("pool is nil")
	}
	// Order: dependents first.
	for _, name := range foundationTableNames {
		if _, err := pool.Exec(ctx, fmt.Sprintf(`DROP TABLE IF EXISTS %s CASCADE`, name)); err != nil {
			return fmt.Errorf("drop table %s: %w", name, err)
		}
	}
	// PostgreSQL creates a composite type per table; orphan types after partial
	// failed CREATE produce pg_type_typname_nsp_index 23505 on retry.
	for _, name := range foundationTableNames {
		if _, err := pool.Exec(ctx, fmt.Sprintf(`DROP TYPE IF EXISTS %s CASCADE`, name)); err != nil {
			return fmt.Errorf("drop type %s: %w", name, err)
		}
	}
	// Sequences owned by dropped tables should be gone; clean known leftover.
	if _, err := pool.Exec(ctx, `DROP SEQUENCE IF EXISTS task_events_id_seq CASCADE`); err != nil {
		return fmt.Errorf("drop sequence: %w", err)
	}
	return nil
}

// ApplyMigrations executes the foundation SQL migration on an empty schema.
// Call DropFoundationSchema first when re-applying in tests.
func ApplyMigrations(ctx context.Context, pool *pgxpool.Pool, migrationPath string) error {
	if pool == nil {
		return fmt.Errorf("pool is nil")
	}
	if strings.TrimSpace(migrationPath) == "" {
		migrationPath = DefaultMigrationPath()
	}
	raw, err := os.ReadFile(migrationPath)
	if err != nil {
		return fmt.Errorf("read migration %s: %w", migrationPath, err)
	}
	// Pre-clean orphan types that would collide with CREATE TABLE composite types.
	if err := DropFoundationSchema(ctx, pool); err != nil {
		return err
	}
	if _, err := pool.Exec(ctx, string(raw)); err != nil {
		return fmt.Errorf("apply migration: %w", err)
	}
	return nil
}

// ResetSchema drops foundation tables so tests start clean.
func ResetSchema(ctx context.Context, pool *pgxpool.Pool) error {
	if err := DropFoundationSchema(ctx, pool); err != nil {
		return fmt.Errorf("reset schema: %w", err)
	}
	return nil
}

// EnsureSchema applies the migration only when the tasks table is absent.
// Uses a session advisory lock so concurrent platform/test processes do not
// race CREATE TABLE composite types.
func EnsureSchema(ctx context.Context, pool *pgxpool.Pool, migrationPath string) error {
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock(87236401)`); err != nil {
		return err
	}
	defer func() { _, _ = conn.Exec(ctx, `SELECT pg_advisory_unlock(87236401)`) }()

	var n int
	if err := conn.QueryRow(ctx, `
SELECT COUNT(*) FROM information_schema.tables
WHERE table_schema='public' AND table_name='tasks'
`).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return nil
	}
	// Build a temporary one-conn pool view: use the locked connection via pool.Exec is fine
	// after lock is held on this backend; other backends wait on the same lock key.
	return ApplyMigrations(ctx, pool, migrationPath)
}
