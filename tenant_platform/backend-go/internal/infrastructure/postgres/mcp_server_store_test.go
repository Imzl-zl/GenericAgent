package postgres

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/domain"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

func TestClassifyMCPServerStoreError(t *testing.T) {
	if err := classifyMCPServerStoreError(pgx.ErrNoRows); !errors.Is(err, domain.ErrMCPServerNotFound) {
		t.Fatalf("not found classification: %v", err)
	}
	unique := &pgconn.PgError{Code: "23505"}
	if err := classifyMCPServerStoreError(unique); !errors.Is(err, domain.ErrMCPServerConflict) {
		t.Fatalf("conflict classification: %v", err)
	}
	internal := errors.New("database unavailable")
	if err := classifyMCPServerStoreError(internal); !errors.Is(err, internal) {
		t.Fatalf("internal classification: %v", err)
	}
}

func TestMCPHeaderCleanupMigrationRemovesLegacyColumns(t *testing.T) {
	pool := requireDB(t)
	ctx := context.Background()
	if err := ResetSchema(ctx, pool); err != nil {
		t.Fatal(err)
	}
	for _, name := range migrationFiles() {
		if name == "0029_mcp_servers.sql" {
			break
		}
		raw, err := os.ReadFile(filepath.Join(migrationsDir(), name))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, string(raw)); err != nil {
			t.Fatalf("apply %s: %v", name, err)
		}
	}
	if _, err := pool.Exec(ctx, `
		CREATE TABLE mcp_servers (
			id BIGSERIAL PRIMARY KEY,
			server_key TEXT NOT NULL UNIQUE,
			name TEXT NOT NULL,
			url TEXT NOT NULL,
			headers_ciphertext BYTEA NOT NULL,
			headers_key_version TEXT NOT NULL,
			timeout_seconds INTEGER NOT NULL,
			enabled BOOLEAN NOT NULL DEFAULT FALSE,
			revision BIGINT NOT NULL DEFAULT 1,
			created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
		);
		INSERT INTO mcp_servers (
			server_key, name, url, headers_ciphertext, headers_key_version, timeout_seconds
		) VALUES ('legacy', 'Legacy', 'https://example.com/mcp', 'secret', '1', 30);
		CREATE TABLE migration_0029_mcp_servers_marker (
			id BOOLEAN PRIMARY KEY DEFAULT TRUE CHECK (id)
		);
		INSERT INTO migration_0029_mcp_servers_marker(id) VALUES (TRUE);
	`); err != nil {
		t.Fatal(err)
	}

	if err := EnsureSchema(ctx, pool, ""); err != nil {
		t.Fatal(err)
	}
	var legacyColumns int
	if err := pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM information_schema.columns
		WHERE table_schema = current_schema()
		  AND table_name = 'mcp_servers'
		  AND column_name IN ('headers_ciphertext', 'headers_key_version')
	`).Scan(&legacyColumns); err != nil {
		t.Fatal(err)
	}
	if legacyColumns != 0 {
		t.Fatalf("legacy MCP header columns remain: %d", legacyColumns)
	}
}

func TestMCPServerStoreLifecycleAndRevision(t *testing.T) {
	pool := OpenTestPool(t)
	store, err := NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	created, err := store.CreateMCPServer(ctx, domain.MCPServerCreate{
		ServerKey: "exa", Name: "Exa", URL: "https://mcp.exa.ai/mcp",
		TimeoutSeconds: 30,
	})
	if err != nil {
		t.Fatal(err)
	}
	if created.Enabled || created.Revision != 1 {
		t.Fatalf("created=%+v", created)
	}
	if enabled, err := store.ListEnabledMCPServers(ctx); err != nil || len(enabled) != 0 {
		t.Fatalf("enabled=%+v err=%v", enabled, err)
	}

	enabled, err := store.SetMCPServerEnabled(ctx, created.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	if !enabled.Enabled || enabled.Revision != 2 {
		t.Fatalf("enabled=%+v", enabled)
	}
	idempotent, err := store.SetMCPServerEnabled(ctx, created.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	if idempotent.Revision != 2 {
		t.Fatalf("idempotent revision=%d", idempotent.Revision)
	}

	updated, err := store.UpdateMCPServer(ctx, created.ID, domain.MCPServerUpdate{
		MCPServerCreate: domain.MCPServerCreate{
			ServerKey: "exa", Name: "Exa Search", URL: "https://mcp.exa.ai/mcp",
			TimeoutSeconds: 45,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if updated.Revision != 3 || updated.TimeoutSeconds != 45 || updated.Name != "Exa Search" {
		t.Fatalf("updated=%+v", updated)
	}
	noChange, err := store.UpdateMCPServer(ctx, created.ID, domain.MCPServerUpdate{
		MCPServerCreate: domain.MCPServerCreate{
			ServerKey: "exa", Name: "Exa Search", URL: "https://mcp.exa.ai/mcp",
			TimeoutSeconds: 45,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if noChange.Revision != 3 {
		t.Fatalf("no-op update revision=%d", noChange.Revision)
	}

	active, err := store.ListEnabledMCPServers(ctx)
	if err != nil || len(active) != 1 || active[0].ID != created.ID {
		t.Fatalf("active=%+v err=%v", active, err)
	}

	if err := store.DeleteMCPServer(ctx, created.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.GetMCPServer(ctx, created.ID); err == nil {
		t.Fatal("expected deleted MCP server lookup to fail")
	}
}
