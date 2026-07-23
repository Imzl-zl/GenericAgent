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
	if _, err := pool.Exec(ctx, `DROP SEQUENCE IF EXISTS task_events_id_seq CASCADE`); err != nil {
		return fmt.Errorf("drop sequence: %w", err)
	}
	return nil
}

// ApplyMigrations executes the foundation SQL migration.
// It does NOT take advisory locks; callers that need serialization use EnsureSchema
// or OpenTestPool. It also does NOT drop existing tables (idempotent empty-DB apply).
// For re-apply after DROP, call DropFoundationSchema first.
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
// Uses a transaction-scoped advisory lock so concurrent platform/test processes
// do not race CREATE TABLE composite types. Nested ApplyMigrations does not lock.
func EnsureSchema(ctx context.Context, pool *pgxpool.Pool, migrationPath string) error {
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

	// Transaction-scoped lock: released on commit/rollback; avoids session-lock
	// deadlocks with other code paths that also take the same key.
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(87236401)`); err != nil {
		return err
	}

	var n int
	if err := tx.QueryRow(ctx, `
SELECT COUNT(*) FROM information_schema.tables
WHERE table_schema='public' AND table_name='tasks'
`).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return tx.Commit(ctx)
	}

	if strings.TrimSpace(migrationPath) == "" {
		migrationPath = DefaultMigrationPath()
	}
	raw, err := os.ReadFile(migrationPath)
	if err != nil {
		return fmt.Errorf("read migration %s: %w", migrationPath, err)
	}
	// Clean orphans inside the same locked transaction, then create.
	for _, name := range foundationTableNames {
		if _, err := tx.Exec(ctx, fmt.Sprintf(`DROP TABLE IF EXISTS %s CASCADE`, name)); err != nil {
			return err
		}
	}
	for _, name := range foundationTableNames {
		if _, err := tx.Exec(ctx, fmt.Sprintf(`DROP TYPE IF EXISTS %s CASCADE`, name)); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(ctx, `DROP SEQUENCE IF EXISTS task_events_id_seq CASCADE`); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, string(raw)); err != nil {
		return fmt.Errorf("apply migration: %w", err)
	}
	return tx.Commit(ctx)
}
