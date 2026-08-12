package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/domain"
	"github.com/jackc/pgx/v5"
)

const mcpServerColumns = `id, server_key, name, url, timeout_seconds,
headers, transport, command, args, isolation, max_instances,
enabled, revision, created_at, updated_at`

func (s *Store) CreateMCPServer(ctx context.Context, input domain.MCPServerCreate) (domain.MCPServer, error) {
	input = normalizeMCPServerInput(input)
	if err := domain.ValidateMCPServerInput(input); err != nil {
		return domain.MCPServer{}, err
	}
	query := `INSERT INTO mcp_servers (
		server_key, name, url, timeout_seconds,
		headers, transport, command, args, isolation, max_instances
	) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10) RETURNING ` + mcpServerColumns
	var server domain.MCPServer
	argsJSON, err := marshalMCPArgs(input.Args)
	if err != nil {
		return domain.MCPServer{}, err
	}
	headersJSON, err := marshalMCPHeaders(input.Headers)
	if err != nil {
		return domain.MCPServer{}, err
	}
	err = scanMCPServer(s.pool.QueryRow(ctx, query,
		input.ServerKey, input.Name, input.URL, input.TimeoutSeconds,
		headersJSON, input.Transport, nil, argsJSON, input.Isolation, input.MaxInstances,
	), &server)
	return server, classifyMCPServerStoreError(err)
}

func (s *Store) GetMCPServer(ctx context.Context, id int64) (domain.MCPServer, error) {
	var server domain.MCPServer
	err := scanMCPServer(s.pool.QueryRow(ctx,
		`SELECT `+mcpServerColumns+` FROM mcp_servers WHERE id = $1`, id,
	), &server)
	return server, classifyMCPServerStoreError(err)
}

func (s *Store) ListMCPServers(ctx context.Context) ([]domain.MCPServer, error) {
	return s.listMCPServers(ctx, false)
}

func (s *Store) ListEnabledMCPServers(ctx context.Context) ([]domain.MCPServer, error) {
	return s.listMCPServers(ctx, true)
}

func (s *Store) listMCPServers(ctx context.Context, enabledOnly bool) ([]domain.MCPServer, error) {
	query := `SELECT ` + mcpServerColumns + ` FROM mcp_servers`
	if enabledOnly {
		query += ` WHERE enabled = TRUE`
	}
	query += ` ORDER BY id`
	rows, err := s.pool.Query(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	servers := make([]domain.MCPServer, 0)
	for rows.Next() {
		var server domain.MCPServer
		if err := scanMCPServer(rows, &server); err != nil {
			return nil, err
		}
		servers = append(servers, server)
	}
	return servers, rows.Err()
}

func (s *Store) UpdateMCPServer(ctx context.Context, id int64, input domain.MCPServerUpdate) (domain.MCPServer, error) {
	input.MCPServerCreate = normalizeMCPServerInput(input.MCPServerCreate)
	if err := domain.ValidateMCPServerInput(input.MCPServerCreate); err != nil {
		return domain.MCPServer{}, err
	}
	query := `UPDATE mcp_servers SET
		server_key = $2,
		name = $3,
		url = $4,
		timeout_seconds = $5,
		headers = $6,
		transport = $7,
		command = $8,
		args = $9,
		isolation = $10,
		max_instances = $11,
		revision = revision + CASE WHEN
			server_key IS DISTINCT FROM $2 OR
			name IS DISTINCT FROM $3 OR
			url IS DISTINCT FROM $4 OR
			timeout_seconds IS DISTINCT FROM $5 OR
			headers IS DISTINCT FROM $6 OR
			transport IS DISTINCT FROM $7 OR
			command IS DISTINCT FROM $8 OR
			args IS DISTINCT FROM $9 OR
			isolation IS DISTINCT FROM $10 OR
			max_instances IS DISTINCT FROM $11
		THEN 1 ELSE 0 END,
		updated_at = NOW()
	WHERE id = $1 RETURNING ` + mcpServerColumns
	var server domain.MCPServer
	argsJSON, err := marshalMCPArgs(input.Args)
	if err != nil {
		return domain.MCPServer{}, err
	}
	headersJSON, err := marshalMCPHeaders(input.Headers)
	if err != nil {
		return domain.MCPServer{}, err
	}
	err = scanMCPServer(s.pool.QueryRow(ctx, query,
		id, input.ServerKey, input.Name, input.URL, input.TimeoutSeconds,
		headersJSON, input.Transport, nil, argsJSON, input.Isolation, input.MaxInstances,
	), &server)
	return server, classifyMCPServerStoreError(err)
}

func (s *Store) SetMCPServerEnabled(ctx context.Context, id int64, enabled bool) (domain.MCPServer, error) {
	var server domain.MCPServer
	err := s.withTx(ctx, func(tx pgx.Tx) error {
		var current domain.MCPServer
		if err := scanMCPServer(tx.QueryRow(ctx,
			`SELECT `+mcpServerColumns+` FROM mcp_servers WHERE id = $1 FOR UPDATE`, id,
		), &current); err != nil {
			return err
		}
		if current.Enabled == enabled {
			server = current
			return nil
		}
		return scanMCPServer(tx.QueryRow(ctx,
			`UPDATE mcp_servers SET enabled = $2, revision = revision + 1, updated_at = NOW()
			 WHERE id = $1 RETURNING `+mcpServerColumns, id, enabled,
		), &server)
	})
	return server, classifyMCPServerStoreError(err)
}

func (s *Store) DeleteMCPServer(ctx context.Context, id int64) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM mcp_servers WHERE id = $1`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w: id %d", domain.ErrMCPServerNotFound, id)
	}
	return nil
}

func normalizeMCPServerInput(input domain.MCPServerCreate) domain.MCPServerCreate {
	input.ServerKey = strings.TrimSpace(input.ServerKey)
	input.Name = strings.TrimSpace(input.Name)
	input.URL = strings.TrimSpace(input.URL)
	input.Transport = strings.TrimSpace(input.Transport)
	if input.Transport == "" {
		input.Transport = domain.MCPTransportHTTP
	}
	input.Command = strings.TrimSpace(input.Command)
	input.Isolation = strings.TrimSpace(input.Isolation)
	if input.Isolation == "" {
		input.Isolation = domain.MCPIsolationShared
	}
	if input.MaxInstances == 0 {
		input.MaxInstances = domain.DefaultMCPMaxInstances
	}
	return input
}

// marshalMCPArgs 把 args 编码为 JSONB; nil/空数组统一为 NULL。
func marshalMCPArgs(args []string) ([]byte, error) {
	if len(args) == 0 {
		return nil, nil
	}
	encoded, err := json.Marshal(args)
	if err != nil {
		return nil, fmt.Errorf("marshal mcp args: %w", err)
	}
	return encoded, nil
}

// marshalMCPHeaders 把 headers 编码为 JSONB; nil/空 map 统一为 NULL。
func marshalMCPHeaders(headers map[string]string) ([]byte, error) {
	if len(headers) == 0 {
		return nil, nil
	}
	encoded, err := json.Marshal(headers)
	if err != nil {
		return nil, fmt.Errorf("marshal mcp headers: %w", err)
	}
	return encoded, nil
}

func classifyMCPServerStoreError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("%w: %v", domain.ErrMCPServerNotFound, err)
	}
	if IsUniqueViolation(err) {
		return fmt.Errorf("%w: server_key already exists", domain.ErrMCPServerConflict)
	}
	return err
}

func scanMCPServer(row pgx.Row, server *domain.MCPServer) error {
	var (
		argsJSON    []byte
		headersJSON []byte
		command     *string // http transport 的 command 列为 NULL
	)
	if err := row.Scan(
		&server.ID, &server.ServerKey, &server.Name, &server.URL,
		&server.TimeoutSeconds, &headersJSON, &server.Transport, &command, &argsJSON,
		&server.Isolation, &server.MaxInstances, &server.Enabled, &server.Revision,
		&server.CreatedAt, &server.UpdatedAt,
	); err != nil {
		return err
	}
	if command != nil {
		server.Command = *command
	}
	if len(argsJSON) > 0 {
		if err := json.Unmarshal(argsJSON, &server.Args); err != nil {
			return fmt.Errorf("unmarshal mcp args: %w", err)
		}
	}
	if len(headersJSON) > 0 {
		if err := json.Unmarshal(headersJSON, &server.Headers); err != nil {
			return fmt.Errorf("unmarshal mcp headers: %w", err)
		}
	}
	return nil
}

// SetMCPQuotaLimit upsert 每用户 × 每 server × 周期的配额限额。
// period: "day" | "month"; limitCount 必须为正。
func (s *Store) SetMCPQuotaLimit(ctx context.Context, ownerKey, serverID, period string, limitCount int64) error {
	if err := validateQuotaPeriod(period); err != nil {
		return err
	}
	if limitCount <= 0 {
		return fmt.Errorf("quota limit must be positive: %d", limitCount)
	}
	if strings.TrimSpace(ownerKey) == "" || strings.TrimSpace(serverID) == "" {
		return fmt.Errorf("quota owner_key and server_id are required")
	}
	_, err := s.pool.Exec(ctx, `
		INSERT INTO mcp_quota_limits (owner_key, server_id, period, limit_count, updated_at)
		VALUES ($1, $2, $3, $4, timezone('utc', now()))
		ON CONFLICT (owner_key, server_id, period) DO UPDATE
		SET limit_count = EXCLUDED.limit_count,
		    updated_at = timezone('utc', now())
	`, ownerKey, serverID, period, limitCount)
	return err
}

// ConsumeMCPQuota 原子扣减周期配额(照搬 ConsumeCapabilityCall 原子模式):
// 有限额行时首调插行 used=1, 后续 +1; 已到 limit 不再更新并返回 (false, nil)。
// 无限额行 = 默认放行(D6), 不写用量。
//
// 已废弃(2026-08 审查): 生产调用已全部切换到 ConsumeMCPQuotas(day+month
// 单事务整体扣减, 无部分扣减副作用)。本方法仅存于测试; 单语句路径不锁
// 限额行, 若与事务版并发调用会破坏串行化不变量——禁止在生产代码中恢复
// 使用, 如需单周期扣减请走 ConsumeMCPQuotas。
func (s *Store) ConsumeMCPQuota(ctx context.Context, ownerKey, serverID, period string) (bool, error) {
	if err := validateQuotaPeriod(period); err != nil {
		return false, err
	}
	var limitCount int64
	err := s.pool.QueryRow(ctx, `
		SELECT limit_count FROM mcp_quota_limits
		WHERE owner_key = $1 AND server_id = $2 AND period = $3
	`, ownerKey, serverID, period).Scan(&limitCount)
	if errors.Is(err, pgx.ErrNoRows) {
		return true, nil // 无限额行 = 默认放行
	}
	if err != nil {
		return false, err
	}
	var used int64
	err = s.pool.QueryRow(ctx, `
		INSERT INTO mcp_quota_usage (owner_key, server_id, period_key, used_count)
		VALUES ($1, $2, `+quotaPeriodKeyExpr(period)+`, 1)
		ON CONFLICT (owner_key, server_id, period_key) DO UPDATE
		SET used_count = mcp_quota_usage.used_count + 1,
		    updated_at = timezone('utc', now())
		WHERE mcp_quota_usage.used_count < $3
		RETURNING used_count
	`, ownerKey, serverID, limitCount).Scan(&used)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil // 已到限额
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

func validateQuotaPeriod(period string) error {
	if period != "day" && period != "month" {
		return fmt.Errorf("quota period must be day or month: %q", period)
	}
	return nil
}

// errQuotaExhaustedInTxn 是 ConsumeMCPQuotas 事务内的哨兵错误: 扣减循环
// 防御分支(理论上不可达, 限额行已 FOR UPDATE)触发时返回它——withTx 遇
// error 回滚(全部周期不扣), 外层把哨兵映射回 (false, nil) = 配额耗尽 429
// 语义, 而不是系统故障 503(审查三轮: 原防御分支先 return nil 提交造成
// 部分扣减, 后改 return error 又漂移为 503 错误码)。
var errQuotaExhaustedInTxn = errors.New("quota exhausted in transaction")

// quotaPeriodKeyExpr 生成 UTC 周期键: day='YYYY-MM-DD' / month='YYYY-MM'。
// 仅接受 validateQuotaPeriod 校验过的 day/month, 无注入面。
func quotaPeriodKeyExpr(period string) string {
	if period == "month" {
		return `to_char(timezone('utc', now()), 'YYYY-MM')`
	}
	return `to_char(timezone('utc', now()), 'YYYY-MM-DD')`
}

// MCPQuotaAvailable 判断某用户对某 server 是否仍可用配额:
// - 无限额行(day/month 均无) → 可用(默认放行);
// - 任一有限额周期已耗尽 → 不可用(即使另一周期仍有剩余);
// - 全部有限额周期未耗尽 → 可用。
func (s *Store) MCPQuotaAvailable(ctx context.Context, ownerKey, serverID string) (bool, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT period FROM mcp_quota_limits
		WHERE owner_key = $1 AND server_id = $2
	`, ownerKey, serverID)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	periods := make([]string, 0, 2)
	for rows.Next() {
		var period string
		if err := rows.Scan(&period); err != nil {
			return false, err
		}
		periods = append(periods, period)
	}
	if err := rows.Err(); err != nil {
		return false, err
	}
	if len(periods) == 0 {
		return true, nil
	}
	for _, period := range periods {
		var limitCount, usedCount int64
		err := s.pool.QueryRow(ctx, `
			SELECT l.limit_count, COALESCE(u.used_count, 0)
			FROM mcp_quota_limits l
			LEFT JOIN mcp_quota_usage u
				ON u.owner_key = l.owner_key AND u.server_id = l.server_id
				AND u.period_key = `+quotaPeriodKeyExpr(period)+`
			WHERE l.owner_key = $1 AND l.server_id = $2 AND l.period = $3
		`, ownerKey, serverID, period).Scan(&limitCount, &usedCount)
		if err != nil {
			return false, err
		}
		if usedCount >= limitCount {
			return false, nil
		}
	}
	return true, nil
}

// ConsumeMCPQuotas 组合扣减 day + month 两个周期(proxy 每次调用执行):
// 任一周期耗尽 → false(整体拒绝, 不产生任何用量)。无限额行周期自动放行
// 不计数。
//
// 审查 Y2: 原实现为两个独立语句(先 day 后 month)——day 成功而 month 耗尽
// 时返回 false 但 day 已 +1, 被拒绝的调用持续烧 day 计数, 可能把 day 配额
// 烧尽导致后续合法调用被误拒。现改为单事务: 按固定顺序(day→month)对
// 限额行 FOR UPDATE 串行化同 (owner, server) 的并发扣减, 先整体校验再
// 整体递增——要么都扣, 要么都不扣, 无部分扣减副作用, 也无死锁
// (所有事务按相同顺序加锁)。
func (s *Store) ConsumeMCPQuotas(ctx context.Context, ownerKey, serverID string) (bool, error) {
	var allowed bool
	err := s.withTx(ctx, func(tx pgx.Tx) error {
		// 1. 固定顺序锁定两个周期的限额行(无限额行 = 默认放行, 不写用量)。
		limits := map[string]int64{}
		for _, period := range []string{"day", "month"} {
			var limit int64
			err := tx.QueryRow(ctx, `
SELECT limit_count FROM mcp_quota_limits
WHERE owner_key = $1 AND server_id = $2 AND period = $3
FOR UPDATE`, ownerKey, serverID, period).Scan(&limit)
			if errors.Is(err, pgx.ErrNoRows) {
				continue
			}
			if err != nil {
				return err
			}
			limits[period] = limit
		}
		// 2. 任一有限额周期已耗尽 → 整体拒绝(未产生任何用量)。
		for period, limit := range limits {
			var used int64
			err := tx.QueryRow(ctx, `
SELECT used_count FROM mcp_quota_usage
WHERE owner_key = $1 AND server_id = $2 AND period_key = `+quotaPeriodKeyExpr(period)+`
FOR UPDATE`, ownerKey, serverID).Scan(&used)
			if errors.Is(err, pgx.ErrNoRows) {
				continue
			}
			if err != nil {
				return err
			}
			if used >= limit {
				return nil // allowed = false
			}
		}
		// 3. 全部未耗尽 → 两个周期各 +1(仅限有限额行的周期; 限额行已加锁,
		//    并发下不可能在此处耗尽, ErrNoRows 仅防御)。防御触发返回哨兵
		//    error 触发回滚(全部周期不扣)——返回 nil 会提交事务造成部分扣减;
		//    哨兵在外层映射为配额耗尽(429)而非系统故障(503)。
		for period, limit := range limits {
			var used int64
			err := tx.QueryRow(ctx, `
INSERT INTO mcp_quota_usage (owner_key, server_id, period_key, used_count)
VALUES ($1, $2, `+quotaPeriodKeyExpr(period)+`, 1)
ON CONFLICT (owner_key, server_id, period_key) DO UPDATE
SET used_count = mcp_quota_usage.used_count + 1,
    updated_at = timezone('utc', now())
WHERE mcp_quota_usage.used_count < $3
RETURNING used_count
`, ownerKey, serverID, limit).Scan(&used)
			if errors.Is(err, pgx.ErrNoRows) {
				return errQuotaExhaustedInTxn
			}
			if err != nil {
				return err
			}
		}
		allowed = true
		return nil
	})
	if errors.Is(err, errQuotaExhaustedInTxn) {
		// 防御触发: 事务已回滚(无部分扣减), 语义 = 配额耗尽(429), 非故障(503)。
		return false, nil
	}
	return allowed, err
}

// ListMCPQuotaLimits 列出某用户的全部配额限额。
func (s *Store) ListMCPQuotaLimits(ctx context.Context, ownerKey string) ([]domain.MCPQuotaLimit, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT owner_key, server_id, period, limit_count
		FROM mcp_quota_limits
		WHERE owner_key = $1
		ORDER BY server_id, period
	`, ownerKey)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	limits := make([]domain.MCPQuotaLimit, 0)
	for rows.Next() {
		var l domain.MCPQuotaLimit
		if err := rows.Scan(&l.OwnerKey, &l.ServerID, &l.Period, &l.LimitCount); err != nil {
			return nil, err
		}
		limits = append(limits, l)
	}
	return limits, rows.Err()
}

// DeleteMCPQuotaLimit 删除某用户的某 server 某周期限额(删除后默认放行)。
func (s *Store) DeleteMCPQuotaLimit(ctx context.Context, ownerKey, serverID, period string) error {
	if err := validateQuotaPeriod(period); err != nil {
		return err
	}
	_, err := s.pool.Exec(ctx, `
		DELETE FROM mcp_quota_limits
		WHERE owner_key = $1 AND server_id = $2 AND period = $3
	`, ownerKey, serverID, period)
	return err
}

// GetWorkspaceOwner 返回 session_key 对应的 workspace 属主用户 ID
// (配额 owner 解析: capability Subject=sessionKey → owner_user_id)。
func (s *Store) GetWorkspaceOwner(ctx context.Context, sessionKey string) (int64, error) {
	var owner int64
	err := s.pool.QueryRow(ctx, `
		SELECT owner_user_id FROM workspaces WHERE session_key = $1
	`, sessionKey).Scan(&owner)
	if err != nil {
		return 0, fmt.Errorf("resolve workspace owner for session %q: %w", sessionKey, err)
	}
	return owner, nil
}
