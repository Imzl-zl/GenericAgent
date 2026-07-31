package postgres

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Shared test DB mutex: foundation tests share one PostgreSQL database.
var (
	testDBMu              sync.Mutex
	testSchemaInitialized bool
	testPool              *pgxpool.Pool
	testPoolURL           string
	testResetTables       []string
	testSequenceResetSQL  string
)

var testSeedTables = []string{
	"platform_commands",
	"tool_policies",
	"platform_runtime_settings",
	"document_pool_settings",
}

// OpenTestPool returns a pool with exclusive, transactionally reset state.
func OpenTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := strings.TrimSpace(os.Getenv("TEST_DATABASE_URL"))
	if url == "" {
		t.Fatal("TEST_DATABASE_URL is required (no SQLite fallback)")
	}
	testDBMu.Lock()
	t.Cleanup(func() { testDBMu.Unlock() })

	ctx := context.Background()
	pool := testPool
	var err error
	if pool == nil {
		config, parseErr := pgxpool.ParseConfig(url)
		if parseErr != nil {
			t.Fatalf("parse test database config: %v", parseErr)
		}
		config.ConnConfig.RuntimeParams["synchronous_commit"] = "off"
		config.ConnConfig.DefaultQueryExecMode = pgx.QueryExecModeCacheDescribe
		config.MinConns = 8
		config.MaxConns = 32
		pool, err = pgxpool.NewWithConfig(ctx, config)
		if err != nil {
			t.Fatalf("connect: %v", err)
		}
		if err := pool.Ping(ctx); err != nil {
			pool.Close()
			t.Fatalf("ping test database: %v", err)
		}
		testPool = pool
		testPoolURL = url
	} else if testPoolURL != url {
		t.Fatalf("TEST_DATABASE_URL changed within one test process")
	}

	healthy := false
	if testSchemaInitialized {
		healthy, err = testSchemaHealthy(ctx, pool)
		if err != nil {
			t.Fatal(err)
		}
	}
	if !healthy {
		if err := resetAndMigrate(ctx, pool); err != nil {
			t.Fatal(err)
		}
		if err := captureTestSeed(ctx, pool); err != nil {
			t.Fatal(err)
		}
		testSchemaInitialized = true
	} else if err := resetTestData(ctx, pool); err != nil {
		t.Fatal(err)
	}
	return pool
}

func testSchemaHealthy(ctx context.Context, pool *pgxpool.Pool) (bool, error) {
	var healthy bool
	err := pool.QueryRow(ctx, `
SELECT to_regclass('public.tasks') IS NOT NULL
   AND to_regclass('public.migration_0033_document_job_finish_marker') IS NOT NULL
   AND EXISTS (
       SELECT 1 FROM information_schema.columns
       WHERE table_schema='public' AND table_name='document_jobs' AND column_name='commands_closed_at'
   )
   AND EXISTS (
       SELECT 1 FROM pg_trigger
       WHERE tgrelid='public.task_sop_snapshots'::regclass
         AND tgname='task_sop_snapshots_sealed' AND NOT tgisinternal
   )
`).Scan(&healthy)
	return healthy, err
}

func captureTestSeed(ctx context.Context, pool *pgxpool.Pool) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `DROP SCHEMA IF EXISTS ga_test_baseline CASCADE; CREATE SCHEMA ga_test_baseline`); err != nil {
		return fmt.Errorf("recreate test baseline schema: %w", err)
	}
	for _, table := range testSeedTables {
		statement := fmt.Sprintf(`CREATE TABLE ga_test_baseline.%s AS TABLE public.%s`, table, table)
		if _, err := tx.Exec(ctx, statement); err != nil {
			return fmt.Errorf("capture test seed %s: %w", table, err)
		}
	}
	tables, sequenceSQL, err := buildTestResetPlan(ctx, tx)
	if err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	testResetTables = tables
	testSequenceResetSQL = sequenceSQL
	return nil
}

func buildTestResetPlan(ctx context.Context, tx pgx.Tx) ([]string, string, error) {
	rows, err := tx.Query(ctx, `
SELECT quote_ident(tablename)
FROM pg_tables
WHERE schemaname='public' AND tablename NOT LIKE 'migration\_%' ESCAPE '\'
ORDER BY tablename
`)
	if err != nil {
		return nil, "", err
	}
	var tables []string
	for rows.Next() {
		var table string
		if err := rows.Scan(&table); err != nil {
			rows.Close()
			return nil, "", err
		}
		tables = append(tables, "public."+table)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, "", err
	}
	rows.Close()
	if len(tables) == 0 {
		return nil, "", fmt.Errorf("test schema contains no resettable tables")
	}

	sequenceRows, err := tx.Query(ctx, `
SELECT quote_literal(format('%I.%I', schemaname, sequencename)), min_value
FROM pg_sequences
WHERE schemaname='public'
`)
	if err != nil {
		return nil, "", fmt.Errorf("list test sequences: %w", err)
	}
	var sequenceStatements strings.Builder
	for sequenceRows.Next() {
		var sequence string
		var minimum int64
		if err := sequenceRows.Scan(&sequence, &minimum); err != nil {
			sequenceRows.Close()
			return nil, "", err
		}
		fmt.Fprintf(&sequenceStatements, "SELECT setval(%s::regclass,%d,false);", sequence, minimum)
	}
	if err := sequenceRows.Err(); err != nil {
		sequenceRows.Close()
		return nil, "", err
	}
	sequenceRows.Close()
	return tables, sequenceStatements.String(), nil
}

func resetTestData(ctx context.Context, pool *pgxpool.Pool) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(87236401)`); err != nil {
		return err
	}
	tables := testResetTables
	if len(tables) == 0 {
		return fmt.Errorf("test reset plan is empty")
	}
	checks := make([]string, 0, len(tables))
	for _, table := range tables {
		literal := strings.ReplaceAll(table, `'`, `''`)
		checks = append(checks, fmt.Sprintf(`SELECT '%s' AS table_name WHERE EXISTS (SELECT 1 FROM %s LIMIT 1)`, literal, table))
	}
	rows, err := tx.Query(ctx, strings.Join(checks, " UNION ALL "))
	if err != nil {
		return fmt.Errorf("find populated test tables: %w", err)
	}
	var populated []string
	for rows.Next() {
		var table string
		if err := rows.Scan(&table); err != nil {
			rows.Close()
			return err
		}
		populated = append(populated, table)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	if len(populated) > 0 {
		if _, err := tx.Exec(ctx, `TRUNCATE TABLE `+strings.Join(populated, ",")+` RESTART IDENTITY CASCADE`); err != nil {
			return fmt.Errorf("truncate test data: %w", err)
		}
	}
	if testSequenceResetSQL != "" {
		if _, err := tx.Exec(ctx, testSequenceResetSQL); err != nil {
			return fmt.Errorf("reset test sequences: %w", err)
		}
	}
	for _, table := range testSeedTables {
		statement := fmt.Sprintf(`INSERT INTO public.%s SELECT * FROM ga_test_baseline.%s`, table, table)
		if _, err := tx.Exec(ctx, statement); err != nil {
			return fmt.Errorf("restore test seed %s: %w", table, err)
		}
	}
	if _, err := tx.Exec(ctx, `
SELECT setval(
    pg_get_serial_sequence('public.platform_commands','id'),
    COALESCE((SELECT MAX(id) FROM public.platform_commands), 1),
    EXISTS(SELECT 1 FROM public.platform_commands)
)
`); err != nil {
		return fmt.Errorf("reset platform command sequence: %w", err)
	}
	return tx.Commit(ctx)
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
	if _, err := tx.Exec(ctx, `DROP SCHEMA IF EXISTS public CASCADE; CREATE SCHEMA public`); err != nil {
		return fmt.Errorf("recreate test schema: %w", err)
	}

	batch, err := readMigrationBatch(migrationFiles())
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, batch); err != nil {
		return fmt.Errorf("apply test migrations: %w", err)
	}
	return tx.Commit(ctx)
}
