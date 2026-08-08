-- 0049_mcp_gateway.sql
-- MCP Gateway transport 演进(MCP_GATEWAY_DESIGN.zh-CN.md §4):
--   mcp_servers 从 http-only 扩展为 transport 感知(http | stdio)。
--   - http  : url 必填, 现有行为不变(向后兼容, 现有行 transport='http')。
--   - stdio : command(白名单绝对路径, 镜像预装工具集) + args(JSONB)。
--   v1 仅允许 isolation='shared' 且无凭据(env 预留, domain 层先拒绝)。
--   url 在 stdio 分支允许为空, 快照下发时由 Platform 改写为 gateway 地址。

ALTER TABLE mcp_servers
    ADD COLUMN IF NOT EXISTS transport TEXT NOT NULL DEFAULT 'http'
        CHECK (transport IN ('http', 'stdio')),
    ADD COLUMN IF NOT EXISTS command TEXT,
    ADD COLUMN IF NOT EXISTS args JSONB,
    ADD COLUMN IF NOT EXISTS isolation TEXT NOT NULL DEFAULT 'shared'
        CHECK (isolation IN ('shared', 'workspace')),
    ADD COLUMN IF NOT EXISTS max_instances INTEGER NOT NULL DEFAULT 1
        CHECK (max_instances BETWEEN 1 AND 16);

-- http: 不允许携带 stdio 字段; stdio: url 可为空但 command 必填。
ALTER TABLE mcp_servers
    ADD CONSTRAINT mcp_servers_transport_fields CHECK (
        (transport = 'http' AND command IS NULL AND args IS NULL)
        OR
        (transport = 'stdio' AND command IS NOT NULL)
    );

CREATE INDEX IF NOT EXISTS idx_mcp_servers_enabled ON mcp_servers(enabled, id);

CREATE TABLE IF NOT EXISTS migration_0049_mcp_gateway_marker (
    id BOOLEAN PRIMARY KEY DEFAULT TRUE CHECK (id)
);
INSERT INTO migration_0049_mcp_gateway_marker(id) VALUES (TRUE) ON CONFLICT DO NOTHING;
