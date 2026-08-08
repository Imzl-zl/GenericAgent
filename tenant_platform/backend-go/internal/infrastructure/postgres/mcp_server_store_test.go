package postgres

import (
	"context"
	"errors"
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
	if _, err := pool.Exec(ctx, `
ALTER TABLE mcp_servers
    ADD COLUMN headers_ciphertext BYTEA,
    ADD COLUMN headers_key_version TEXT;
DROP TABLE migration_0030_remove_mcp_headers_marker;
INSERT INTO mcp_servers (
    server_key, name, url, headers_ciphertext, headers_key_version, timeout_seconds
) VALUES ('legacy', 'Legacy', 'https://example.com/mcp', 'secret', '1', 30)
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

// TestMCPServerMixedTransportRoundTrip 回归: 真实环境 WORKER_START_FAILED
// (scan NULL into *string)——http server 的 command 列为 NULL, 混合
// http+stdio 启用列表必须能正常扫描, 且 CHECK 约束要求 http 的
// command/args 为 NULL、stdio 的 command 非空(0049_mcp_gateway.sql)。
func TestMCPServerMixedTransportRoundTrip(t *testing.T) {
	pool := requireDB(t)
	store, err := NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	httpServer, err := store.CreateMCPServer(ctx, domain.MCPServerCreate{
		ServerKey: "exa", Name: "Exa Search", URL: "https://mcp.exa.ai/mcp",
		TimeoutSeconds: 30,
	})
	if err != nil {
		t.Fatalf("create http server: %v", err)
	}
	if httpServer.Transport != domain.MCPTransportHTTP || httpServer.Command != "" || len(httpServer.Args) != 0 {
		t.Fatalf("http server fields: transport=%q command=%q args=%v", httpServer.Transport, httpServer.Command, httpServer.Args)
	}

	stdioServer, err := store.CreateMCPServer(ctx, domain.MCPServerCreate{
		ServerKey: "pandoc", Name: "Pandoc", Transport: domain.MCPTransportStdio,
		Command: "/opt/mcp-tools/mcp-pandoc", Args: []string{"--stdio"},
		TimeoutSeconds: 60,
	})
	if err != nil {
		t.Fatalf("create stdio server: %v", err)
	}
	if stdioServer.Transport != domain.MCPTransportStdio || stdioServer.Command != "/opt/mcp-tools/mcp-pandoc" {
		t.Fatalf("stdio server fields: %+v", stdioServer)
	}

	if _, err := store.SetMCPServerEnabled(ctx, httpServer.ID, true); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SetMCPServerEnabled(ctx, stdioServer.ID, true); err != nil {
		t.Fatal(err)
	}

	// 回归点: ListEnabledMCPServers 必须能扫描混合 transport(NULL command)。
	servers, err := store.ListEnabledMCPServers(ctx)
	if err != nil {
		t.Fatalf("list enabled servers: %v", err)
	}
	seen := map[string]bool{}
	for _, s := range servers {
		seen[s.ServerKey] = true
		if s.ServerKey == "exa" && s.Command != "" {
			t.Fatalf("http server command must be empty, got %q", s.Command)
		}
		if s.ServerKey == "pandoc" && s.Command != "/opt/mcp-tools/mcp-pandoc" {
			t.Fatalf("stdio server command mismatch: %q", s.Command)
		}
	}
	if !seen["exa"] || !seen["pandoc"] {
		t.Fatalf("expected both servers in enabled list, got %v", seen)
	}

	// http server 不允许携带 stdio 字段(domain 校验 + CHECK 双保险)。
	if _, err := store.CreateMCPServer(ctx, domain.MCPServerCreate{
		ServerKey: "bad-http", Name: "Bad", URL: "https://example.com/mcp",
		Command: "/opt/mcp-tools/whatever", TimeoutSeconds: 30,
	}); err == nil {
		t.Fatal("http transport with command must be rejected")
	}
}
