package postgres

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// migrationsDir returns the checked-in migrations directory relative to this module.
func migrationsDir() string {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		return ""
	}
	// backend-go/internal/postgres -> tenant_platform/infra/postgres/migrations
	root := filepath.Clean(filepath.Join(filepath.Dir(file), "..", "..", ".."))
	return filepath.Join(root, "infra", "postgres", "migrations")
}

// DefaultMigrationPath returns the checked-in 0001_foundation.sql path relative to this module.
func DefaultMigrationPath() string {
	return filepath.Join(migrationsDir(), "0001_foundation.sql")
}

// migrationFiles lists all migration SQL files in apply order.
func migrationFiles() []string {
	return []string{
		"0001_foundation.sql",
		"0002_team_tables.sql",
		"0003_user_lifecycle.sql",
		"0004_configurable_policies.sql",
		"0005_per_user_tool_policy.sql",
		"0006_delivery_outbox_indexes.sql",
		"0007_llm_providers.sql",
		"0008_user_id_serial.sql",
		"0009_invite_persona_self_binding.sql",
		"0010_user_password_hash.sql",
		"0011_wechat_qr_session.sql",
		"0012_bot_transport_cursor_key_version.sql",
	}
}

// pendingMigrations maps each post-foundation migration file to a marker table
// that indicates it has already been applied. applyPendingMigrations applies
// each file only when its marker table is absent.
var pendingMigrations = []struct {
	file       string
	markerTable string
}{
	{"0002_team_tables.sql", "teams"},
	{"0003_user_lifecycle.sql", "binding_attempts"},
	{"0004_configurable_policies.sql", "platform_commands"},
	{"0005_per_user_tool_policy.sql", "users"},
	{"0006_delivery_outbox_indexes.sql", "task_deliveries"},
	{"0007_llm_providers.sql", "llm_providers"},
	{"0008_user_id_serial.sql", "migration_0008_user_id_serial_marker"},
	{"0009_invite_persona_self_binding.sql", "migration_0009_marker"},
	{"0010_user_password_hash.sql", "migration_0010_user_password_hash_marker"},
	{"0011_wechat_qr_session.sql", "migration_0011_wechat_qr_session_marker"},
	{"0012_bot_transport_cursor_key_version.sql", "migration_0012_bot_transport_cursor_key_version_marker"},
}

// foundationTableNames are dropped before re-applying migrations (dependents first).
var foundationTableNames = []string{
	"llm_providers",
	"audit_events",
	"context_tokens",
	"bot_transport_state",
	"bots",
	"binding_attempts",
	"task_deliveries",
	"task_events",
	"workspace_snapshots",
	"tasks",
	"workspaces",
	"users",
	"team_members",
	"teams",
	"platform_commands",
	"tool_policies",
	"migration_0008_user_id_serial_marker",
	"migration_0012_bot_transport_cursor_key_version_marker",
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

// ApplyMigrations executes all migration SQL files in order.
// It does NOT take advisory locks; callers that need serialization use EnsureSchema
// or OpenTestPool. It also does NOT drop existing tables (idempotent empty-DB apply).
// For re-apply after DROP, call DropFoundationSchema first.
// The migrationPath argument is accepted for backward compatibility but ignored
// when empty; the canonical migration set lives under migrationsDir().
func ApplyMigrations(ctx context.Context, pool *pgxpool.Pool, migrationPath string) error {
	if pool == nil {
		return fmt.Errorf("pool is nil")
	}
	files := migrationFiles()
	// Backward-compatible single-file override (tests/dev tooling).
	if strings.TrimSpace(migrationPath) != "" {
		if info, err := os.Stat(migrationPath); err == nil && !info.IsDir() {
			files = []string{migrationPath}
		}
	}
	dir := migrationsDir()
	for _, name := range files {
		path := name
		if !filepath.IsAbs(path) {
			path = filepath.Join(dir, name)
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("read migration %s: %w", path, err)
		}
		if _, err := pool.Exec(ctx, string(raw)); err != nil {
			return fmt.Errorf("apply migration %s: %w", name, err)
		}
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
		// Foundation slice: migration files are the single source of truth for
		// schema (plan Task 5 Step 3). Runtime ALTER patches are rejected as
		// patch-stacking; if the schema needs to evolve, ship a new migration
		// file instead. When the tasks table already exists we trust the base
		// 0001 migration has run; apply any newer migrations that haven't.
		return applyPendingMigrations(ctx, tx)
	}

	dir := migrationsDir()
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
	for _, name := range migrationFiles() {
		path := filepath.Join(dir, name)
		raw, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("read migration %s: %w", path, err)
		}
		if _, err := tx.Exec(ctx, string(raw)); err != nil {
			return fmt.Errorf("apply migration %s: %w", name, err)
		}
	}
	return tx.Commit(ctx)
}

// applyPendingMigrations applies migration files whose marker table is missing.
// This runs inside the EnsureSchema advisory-locked transaction so concurrent
// platform processes do not race. Each migration file is responsible for being
// idempotent-safe (guarded by the caller's marker table check).
func applyPendingMigrations(ctx context.Context, tx pgx.Tx) error {
	dir := migrationsDir()
	for _, pm := range pendingMigrations {
		var count int
		if err := tx.QueryRow(ctx, `
SELECT COUNT(*) FROM information_schema.tables
WHERE table_schema='public' AND table_name=$1
`, pm.markerTable).Scan(&count); err != nil {
			return err
		}
		if count > 0 {
			continue
		}
		path := filepath.Join(dir, pm.file)
		raw, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("read migration %s: %w", pm.file, err)
		}
		if _, err := tx.Exec(ctx, string(raw)); err != nil {
			return fmt.Errorf("apply migration %s: %w", pm.file, err)
		}
	}
	return tx.Commit(ctx)
}
