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
transport, command, args, isolation, max_instances,
enabled, revision, created_at, updated_at`

func (s *Store) CreateMCPServer(ctx context.Context, input domain.MCPServerCreate) (domain.MCPServer, error) {
	input = normalizeMCPServerInput(input)
	if err := domain.ValidateMCPServerInput(input); err != nil {
		return domain.MCPServer{}, err
	}
	query := `INSERT INTO mcp_servers (
		server_key, name, url, timeout_seconds,
		transport, command, args, isolation, max_instances
	) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9) RETURNING ` + mcpServerColumns
	var server domain.MCPServer
	argsJSON, err := marshalMCPArgs(input.Args)
	if err != nil {
		return domain.MCPServer{}, err
	}
	err = scanMCPServer(s.pool.QueryRow(ctx, query,
		input.ServerKey, input.Name, input.URL, input.TimeoutSeconds,
		input.Transport, mcpCommandParam(input), argsJSON, input.Isolation, input.MaxInstances,
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
		transport = $6,
		command = $7,
		args = $8,
		isolation = $9,
		max_instances = $10,
		revision = revision + CASE WHEN
			server_key IS DISTINCT FROM $2 OR
			name IS DISTINCT FROM $3 OR
			url IS DISTINCT FROM $4 OR
			timeout_seconds IS DISTINCT FROM $5 OR
			transport IS DISTINCT FROM $6 OR
			command IS DISTINCT FROM $7 OR
			args IS DISTINCT FROM $8 OR
			isolation IS DISTINCT FROM $9 OR
			max_instances IS DISTINCT FROM $10
		THEN 1 ELSE 0 END,
		updated_at = NOW()
	WHERE id = $1 RETURNING ` + mcpServerColumns
	var server domain.MCPServer
	argsJSON, err := marshalMCPArgs(input.Args)
	if err != nil {
		return domain.MCPServer{}, err
	}
	err = scanMCPServer(s.pool.QueryRow(ctx, query,
		id, input.ServerKey, input.Name, input.URL, input.TimeoutSeconds,
		input.Transport, mcpCommandParam(input.MCPServerCreate), argsJSON, input.Isolation, input.MaxInstances,
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

// mcpCommandParam 返回写入 DB 的 command 值: http transport 必须为 NULL
// (CHECK 约束), stdio 为白名单绝对路径。
func mcpCommandParam(input domain.MCPServerCreate) any {
	transport := input.Transport
	if transport == "" {
		transport = domain.MCPTransportHTTP
	}
	if transport != domain.MCPTransportStdio {
		return nil
	}
	return input.Command
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
		argsJSON []byte
		command  *string // http transport 的 command 列为 NULL
	)
	if err := row.Scan(
		&server.ID, &server.ServerKey, &server.Name, &server.URL,
		&server.TimeoutSeconds, &server.Transport, &command, &argsJSON,
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
	return nil
}
