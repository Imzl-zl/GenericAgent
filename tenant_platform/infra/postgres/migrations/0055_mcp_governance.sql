-- 0055_mcp_governance.sql
-- MCP 治理: headers 凭据注入(平台侧持有) + 每用户 × 每 server × 周期配额。
-- 设计: .tasks/mcp-governance/EPIC.md (D4'/D6/D7/D8'/D9)。

-- mcp_servers.headers: proxy 转发时注入上游的请求头(Authorization/x-api-key 等),
-- 平台侧持有, 绝不下发 worker 快照; admin API 回显掩码。
-- 明文 JSONB(历史 0030 移除加密列的先例: v1 只支持无认证端点; 本轮引入认证端点,
-- 防护面 = 掩码 + 不回显 + 快照不含, 不做列加密)。
ALTER TABLE mcp_servers
    ADD COLUMN IF NOT EXISTS headers JSONB;

-- 每用户 × 每 server × 周期限额(配额真值; 无行 = 默认放行)。
CREATE TABLE IF NOT EXISTS mcp_quota_limits (
    owner_key   TEXT NOT NULL,
    server_id   TEXT NOT NULL,
    period      TEXT NOT NULL CHECK (period IN ('day', 'month')),
    limit_count BIGINT NOT NULL CHECK (limit_count > 0),
    created_at  TIMESTAMPTZ NOT NULL DEFAULT timezone('utc', now()),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT timezone('utc', now()),
    PRIMARY KEY (owner_key, server_id, period)
);

-- 周期用量(原子扣减计数表; period_key: day='YYYY-MM-DD' / month='YYYY-MM')。
CREATE TABLE IF NOT EXISTS mcp_quota_usage (
    owner_key   TEXT NOT NULL,
    server_id   TEXT NOT NULL,
    period_key  TEXT NOT NULL,
    used_count  BIGINT NOT NULL DEFAULT 0 CHECK (used_count >= 0),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT timezone('utc', now()),
    PRIMARY KEY (owner_key, server_id, period_key)
);

CREATE TABLE IF NOT EXISTS migration_0055_mcp_governance_marker (
    id BOOLEAN PRIMARY KEY DEFAULT TRUE CHECK (id)
);
INSERT INTO migration_0055_mcp_governance_marker(id)
VALUES (TRUE)
ON CONFLICT (id) DO NOTHING;
