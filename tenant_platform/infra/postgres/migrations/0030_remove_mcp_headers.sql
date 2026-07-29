-- 0030_remove_mcp_headers.sql
-- Cleanup for development databases that applied an early 0029 version with
-- encrypted MCP headers. V1 supports only unauthenticated MCP endpoints.

ALTER TABLE IF EXISTS mcp_servers
    DROP COLUMN IF EXISTS headers_ciphertext,
    DROP COLUMN IF EXISTS headers_key_version;

CREATE TABLE IF NOT EXISTS migration_0030_remove_mcp_headers_marker (
    id BOOLEAN PRIMARY KEY DEFAULT TRUE CHECK (id)
);
INSERT INTO migration_0030_remove_mcp_headers_marker(id)
VALUES (TRUE) ON CONFLICT DO NOTHING;
