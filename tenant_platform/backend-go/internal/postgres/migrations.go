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

// ApplyMigrations executes the foundation SQL migration (idempotent for empty DB only).
// Tests recreate schema with DROP CASCADE then apply.
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
	sql := string(raw)
	if _, err := pool.Exec(ctx, sql); err != nil {
		return fmt.Errorf("apply migration: %w", err)
	}
	return nil
}

// ResetSchema drops foundation tables so tests start clean.
func ResetSchema(ctx context.Context, pool *pgxpool.Pool) error {
	_, err := pool.Exec(ctx, `
DROP TABLE IF EXISTS task_deliveries CASCADE;
DROP TABLE IF EXISTS task_events CASCADE;
DROP TABLE IF EXISTS workspace_snapshots CASCADE;
DROP TABLE IF EXISTS tasks CASCADE;
DROP TABLE IF EXISTS workspaces CASCADE;
DROP TABLE IF EXISTS users CASCADE;
`)
	if err != nil {
		return fmt.Errorf("reset schema: %w", err)
	}
	// Clear any leftover composite types from failed partial applies.
	_, _ = pool.Exec(ctx, `
DO $$ DECLARE r RECORD;
BEGIN
  FOR r IN (SELECT typname FROM pg_type t JOIN pg_namespace n ON n.oid=t.typnamespace
            WHERE n.nspname='public' AND t.typtype='c'
              AND typname IN ('users','workspaces','tasks','task_events','task_deliveries','workspace_snapshots'))
  LOOP
    EXECUTE 'DROP TYPE IF EXISTS public.' || quote_ident(r.typname) || ' CASCADE';
  END LOOP;
END $$;
`)
	return nil
}
