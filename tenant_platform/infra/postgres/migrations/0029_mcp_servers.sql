-- 0029_mcp_servers.sql
-- Administrator-managed global unauthenticated Streamable HTTP MCP servers.

CREATE TABLE IF NOT EXISTS mcp_servers (
    id BIGSERIAL PRIMARY KEY,
    server_key TEXT NOT NULL UNIQUE CHECK (server_key ~ '^[A-Za-z0-9_]{1,32}$'),
    name TEXT NOT NULL,
    url TEXT NOT NULL,
    timeout_seconds INTEGER NOT NULL DEFAULT 30 CHECK (timeout_seconds BETWEEN 1 AND 300),
    enabled BOOLEAN NOT NULL DEFAULT FALSE,
    revision BIGINT NOT NULL DEFAULT 1 CHECK (revision > 0),
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_mcp_servers_enabled ON mcp_servers(enabled, id);

CREATE TABLE IF NOT EXISTS migration_0029_mcp_servers_marker (
    id BOOLEAN PRIMARY KEY DEFAULT TRUE CHECK (id)
);
INSERT INTO migration_0029_mcp_servers_marker(id) VALUES (TRUE) ON CONFLICT DO NOTHING;
