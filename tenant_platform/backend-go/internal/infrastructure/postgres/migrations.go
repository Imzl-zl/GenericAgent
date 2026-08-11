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
	if configured := strings.TrimSpace(os.Getenv("GA_MIGRATIONS_DIR")); configured != "" {
		return filepath.Clean(configured)
	}
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		return ""
	}
	// backend-go/internal/infrastructure/postgres -> tenant_platform/infra/postgres/migrations
	root := filepath.Clean(filepath.Join(filepath.Dir(file), "..", "..", "..", ".."))
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
		"0013_messages.sql",
		"0014_media_assets.sql",
		"0015_task_last_activity_at.sql",
		"0016_team_lifecycle.sql",
		"0017_relay_opt_in.sql",
		"0018_session_reset.sql",
		"0019_drop_binding_attempts.sql",
		"0020_task_event_sequence_counter.sql",
		"0021_tasks_requester_status_index.sql",
		"0022_task_started_delivery.sql",
		"0023_llm_provider_ga_config.sql",
		"0024_transparent_llm_proxy.sql",
		"0025_platform_runtime_settings.sql",
		"0026_safe_user_commands.sql",
		"0027_outbound_delivery_progress.sql",
		"0028_agent_max_turns_setting.sql",
		"0029_mcp_servers.sql",
		"0030_remove_mcp_headers.sql",
		"0035_sophub_sop_registry.sql",
		"0036_channel_bindings.sql",
		"0037_runner_leases.sql",
		"0039_drop_sophub_registry.sql",
		"0040_checkpoint_runner_generation.sql",
		"0041_runner_lease_stale_container.sql",
		"0042_task_capability_jtis.sql",
		"0043_capability_usage.sql",
		"0044_task_delivery_files.sql",
		"0045_delivery_cancelled.sql",
		"0046_drop_tool_policy.sql",
		"0047_delivery_attempt_token.sql",
		"0048_platform_admin_bootstrap.sql",
		"0049_mcp_gateway.sql",
		"0050_user_personal_workspace.sql",
		"0051_conversation_key.sql",
		"0052_conversation_resets.sql",
		"0053_channel_configs.sql",
		"0054_im_streaming.sql",
		"0055_mcp_governance.sql",
	}
}

// pendingMigrations maps each post-foundation migration file to a marker table
// that indicates it has already been applied. applyPendingMigrations applies
// each file only when its marker table is absent.
var pendingMigrations = []struct {
	file        string
	markerTable string
}{
	{"0002_team_tables.sql", "teams"},
	{"0003_user_lifecycle.sql", "bots"},
	{"0004_configurable_policies.sql", "platform_commands"},
	{"0005_per_user_tool_policy.sql", "users"},
	{"0006_delivery_outbox_indexes.sql", "task_deliveries"},
	{"0007_llm_providers.sql", "llm_providers"},
	{"0008_user_id_serial.sql", "migration_0008_user_id_serial_marker"},
	{"0009_invite_persona_self_binding.sql", "migration_0009_marker"},
	{"0010_user_password_hash.sql", "migration_0010_user_password_hash_marker"},
	{"0011_wechat_qr_session.sql", "migration_0011_wechat_qr_session_marker"},
	{"0012_bot_transport_cursor_key_version.sql", "migration_0012_bot_transport_cursor_key_version_marker"},
	{"0013_messages.sql", "migration_0013_messages_marker"},
	{"0014_media_assets.sql", "migration_0014_media_assets_marker"},
	{"0015_task_last_activity_at.sql", "migration_0015_task_last_activity_at_marker"},
	{"0016_team_lifecycle.sql", "migration_0016_team_lifecycle_marker"},
	{"0017_relay_opt_in.sql", "relay_preferences"},
	{"0018_session_reset.sql", "migration_0018_session_reset_marker"},
	{"0019_drop_binding_attempts.sql", "migration_0019_drop_binding_attempts_marker"},
	{"0020_task_event_sequence_counter.sql", "migration_0020_task_event_sequence_counter_marker"},
	{"0021_tasks_requester_status_index.sql", "migration_0021_tasks_requester_status_index_marker"},
	{"0022_task_started_delivery.sql", "migration_0022_task_started_delivery_marker"},
	{"0023_llm_provider_ga_config.sql", "migration_0023_llm_provider_ga_config_marker"},
	{"0024_transparent_llm_proxy.sql", "migration_0024_transparent_llm_proxy_marker"},
	{"0025_platform_runtime_settings.sql", "migration_0025_platform_runtime_settings_marker"},
	{"0026_safe_user_commands.sql", "migration_0026_safe_user_commands_marker"},
	{"0027_outbound_delivery_progress.sql", "migration_0027_outbound_delivery_progress_marker"},
	{"0028_agent_max_turns_setting.sql", "migration_0028_agent_max_turns_setting_marker"},
	{"0029_mcp_servers.sql", "migration_0029_mcp_servers_marker"},
	{"0030_remove_mcp_headers.sql", "migration_0030_remove_mcp_headers_marker"},
	{"0035_sophub_sop_registry.sql", "migration_0035_sophub_sop_registry_marker"},
	{"0036_channel_bindings.sql", "migration_0036_channel_bindings_marker"},
	{"0037_runner_leases.sql", "migration_0037_runner_leases_marker"},
	{"0039_drop_sophub_registry.sql", "migration_0039_drop_sophub_registry_marker"},
	{"0040_checkpoint_runner_generation.sql", "migration_0040_checkpoint_runner_generation_marker"},
	{"0041_runner_lease_stale_container.sql", "migration_0041_runner_lease_stale_container_marker"},
	{"0042_task_capability_jtis.sql", "migration_0042_task_capability_jtis_marker"},
	{"0043_capability_usage.sql", "migration_0043_capability_usage_marker"},
	{"0044_task_delivery_files.sql", "migration_0044_task_delivery_files_marker"},
	{"0045_delivery_cancelled.sql", "migration_0045_delivery_cancelled_marker"},
	{"0046_drop_tool_policy.sql", "migration_0046_drop_tool_policy_marker"},
	{"0047_delivery_attempt_token.sql", "migration_0047_delivery_attempt_token_marker"},
	{"0048_platform_admin_bootstrap.sql", "migration_0048_platform_admin_bootstrap_marker"},
	{"0049_mcp_gateway.sql", "migration_0049_mcp_gateway_marker"},
	{"0050_user_personal_workspace.sql", "migration_0050_user_personal_workspace_marker"},
	{"0051_conversation_key.sql", "migration_0051_conversation_key_marker"},
	{"0052_conversation_resets.sql", "migration_0052_conversation_resets_marker"},
	{"0053_channel_configs.sql", "migration_0053_channel_configs_marker"},
	{"0054_im_streaming.sql", "migration_0054_im_streaming_marker"},
	{"0055_mcp_governance.sql", "migration_0055_mcp_governance_marker"},
}

// foundationTableNames are dropped before re-applying migrations (dependents first).
var foundationTableNames = []string{
	"task_sop_snapshots",
	"sop_versions",
	"sop_entries",
	"sop_candidates",
	"sophub_bindings",
	"llm_capability_revocations",
	"channel_bindings",
	"runner_leases",
	"media_assets",
	"messages",
	"mcp_servers",
	"llm_providers",
	"audit_events",
	"context_tokens",
	"bot_transport_state",
	"channel_configs",
	"bots",
	"binding_attempts",
	"task_deliveries",
	"task_events",
	"workspace_snapshots",
	"tasks",
	"workspaces",
	"wechat_qr_sessions",
	"personas",
	"user_sessions",
	"invite_codes",
	"users",
	"team_members",
	"teams",
	"platform_commands",
	"tool_policies",
	"relay_preferences",
	"platform_runtime_settings",
	"migration_0008_user_id_serial_marker",
	"migration_0009_marker",
	"migration_0010_user_password_hash_marker",
	"migration_0011_wechat_qr_session_marker",
	"migration_0023_llm_provider_ga_config_marker",
	"migration_0024_transparent_llm_proxy_marker",
	"migration_0025_platform_runtime_settings_marker",
	"migration_0026_safe_user_commands_marker",
	"migration_0027_outbound_delivery_progress_marker",
	"migration_0028_agent_max_turns_setting_marker",
	"migration_0029_mcp_servers_marker",
	"migration_0030_remove_mcp_headers_marker",
	"migration_0035_sophub_sop_registry_marker",
	"migration_0036_channel_bindings_marker",
	"migration_0037_runner_leases_marker",
	"migration_0039_drop_sophub_registry_marker",
	"migration_0040_checkpoint_runner_generation_marker",
	"migration_0041_runner_lease_stale_container_marker",
	"migration_0042_task_capability_jtis_marker",
	"capability_usage",
	"migration_0043_capability_usage_marker",
	"task_delivery_files",
	"migration_0044_task_delivery_files_marker",
	"migration_0045_delivery_cancelled_marker",
	"migration_0046_drop_tool_policy_marker",
	"migration_0047_delivery_attempt_token_marker",
	"migration_0048_platform_admin_bootstrap_marker",
	"migration_0049_mcp_gateway_marker",
	"migration_0053_channel_configs_marker",
	"migration_0054_im_streaming_marker",
	"migration_0055_mcp_governance_marker",
	"migration_0012_bot_transport_cursor_key_version_marker",
	"migration_0013_messages_marker",
	"migration_0014_media_assets_marker",
	"migration_0015_task_last_activity_at_marker",
	"migration_0016_team_lifecycle_marker",
	"migration_0018_session_reset_marker",
	"migration_0019_drop_binding_attempts_marker",
	"migration_0020_task_event_sequence_counter_marker",
	"team_invite_codes",
	"active_contexts",
}

func readMigrationBatch(files []string) (string, error) {
	dir := migrationsDir()
	var batch strings.Builder
	for _, name := range files {
		path := name
		if !filepath.IsAbs(path) {
			path = filepath.Join(dir, name)
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return "", fmt.Errorf("read migration %s: %w", path, err)
		}
		fmt.Fprintf(&batch, "\n-- migration: %s\n", name)
		batch.Write(raw)
		batch.WriteByte('\n')
	}
	return batch.String(), nil
}

// DropFoundationSchema removes foundation tables and leftover composite types.
// Safe to call when objects are missing. Uses CASCADE.
func DropFoundationSchema(ctx context.Context, pool *pgxpool.Pool) error {
	if pool == nil {
		return fmt.Errorf("pool is nil")
	}
	if _, err := pool.Exec(ctx, `DROP TABLE IF EXISTS `+strings.Join(foundationTableNames, ",")+` CASCADE`); err != nil {
		return fmt.Errorf("drop foundation tables: %w", err)
	}
	// PostgreSQL creates a composite type per table; orphan types after partial
	// failed CREATE produce pg_type_typname_nsp_index 23505 on retry.
	if _, err := pool.Exec(ctx, `DROP TYPE IF EXISTS `+strings.Join(foundationTableNames, ",")+` CASCADE`); err != nil {
		return fmt.Errorf("drop foundation types: %w", err)
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

	if _, err := tx.Exec(ctx, `DROP TABLE IF EXISTS `+strings.Join(foundationTableNames, ",")+` CASCADE`); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DROP TYPE IF EXISTS `+strings.Join(foundationTableNames, ",")+` CASCADE`); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DROP SEQUENCE IF EXISTS task_events_id_seq CASCADE`); err != nil {
		return err
	}
	batch, err := readMigrationBatch(migrationFiles())
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, batch); err != nil {
		return fmt.Errorf("apply fresh schema migrations: %w", err)
	}
	return tx.Commit(ctx)
}

// applyPendingMigrations applies migration files whose marker table is missing.
// This runs inside the EnsureSchema advisory-locked transaction so concurrent
// platform processes do not race. Each migration file is responsible for being
// idempotent-safe (guarded by the caller's marker table check).
func applyPendingMigrations(ctx context.Context, tx pgx.Tx) error {
	markers := make([]string, 0, len(pendingMigrations))
	for _, migration := range pendingMigrations {
		markers = append(markers, migration.markerTable)
	}
	rows, err := tx.Query(ctx, `
SELECT marker
FROM unnest($1::text[]) AS marker
WHERE to_regclass(format('%I.%I', 'public', marker)) IS NULL
`, markers)
	if err != nil {
		return fmt.Errorf("find pending migrations: %w", err)
	}
	missingMarkers := make(map[string]struct{}, len(pendingMigrations))
	for rows.Next() {
		var marker string
		if err := rows.Scan(&marker); err != nil {
			rows.Close()
			return err
		}
		missingMarkers[marker] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()

	files := make([]string, 0, len(missingMarkers))
	for _, migration := range pendingMigrations {
		if _, missing := missingMarkers[migration.markerTable]; missing {
			files = append(files, migration.file)
		}
	}
	if len(files) > 0 {
		batch, err := readMigrationBatch(files)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, batch); err != nil {
			return fmt.Errorf("apply pending migrations %s: %w", strings.Join(files, ", "), err)
		}
	}
	return tx.Commit(ctx)
}
