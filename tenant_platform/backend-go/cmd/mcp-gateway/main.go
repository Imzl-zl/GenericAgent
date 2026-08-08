// mcp-gateway 是 stdio/HTTP 统一 transport 网关(设计见
// tenant_platform/docs/MCP_GATEWAY_DESIGN.zh-CN.md)。
//
// 职责: 接收 Platform WorkerMCPProxy 转发的 Streamable HTTP MCP 请求,
// 按 server_id 路由 —— http server 直接反代(阶段 2), stdio server
// 经受管子进程桥(阶段 1)。
//
// 安全边界(与 Platform 审查基线一致):
//   - 只读 mcp_servers 表(启用中 stdio server 即白名单, 未知 404);
//   - 不接入任何 egress 网络, stdio 子进程继承无网(uv/npm 依赖构建期预装);
//   - 子进程工作目录为 tmpfs 空目录, 不挂载 workspace/config 卷;
//   - 不持任何凭据, 不落地任何租户数据。
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/mcpgateway"
)

// postgresCatalog 只读加载启用中的 stdio server 白名单。
type postgresCatalog struct {
	pool *pgxpool.Pool
}

func (c *postgresCatalog) EnabledServers(ctx context.Context) ([]mcpgateway.Server, error) {
	rows, err := c.pool.Query(ctx, `
		SELECT server_key, name, command, args, timeout_seconds, max_instances
		FROM mcp_servers
		WHERE enabled = TRUE AND transport = 'stdio'
		ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("query enabled stdio mcp servers: %w", err)
	}
	defer rows.Close()
	servers := make([]mcpgateway.Server, 0, 8)
	for rows.Next() {
		var (
			server       mcpgateway.Server
			argsJSON     []byte
			timeoutSecs  int
			maxInstances int
		)
		if err := rows.Scan(&server.ServerID, &server.Name, &server.Command,
			&argsJSON, &timeoutSecs, &maxInstances); err != nil {
			return nil, fmt.Errorf("scan mcp server: %w", err)
		}
		if len(argsJSON) > 0 {
			if err := json.Unmarshal(argsJSON, &server.Args); err != nil {
				return nil, fmt.Errorf("unmarshal args for %s: %w", server.ServerID, err)
			}
		}
		server.Timeout = time.Duration(timeoutSecs) * time.Second
		server.MaxInstance = maxInstances
		servers = append(servers, server)
	}
	return servers, rows.Err()
}

func main() {
	var (
		databaseURL = flag.String("database-url", "", "PostgreSQL URL (or DATABASE_URL)")
		listen      = flag.String("listen", "", "listen address (or GA_MCP_GATEWAY_LISTEN, default 0.0.0.0:8083)")
		workRoot    = flag.String("work-root", "", "stdio process work dir root (or GA_MCP_GATEWAY_WORK_ROOT)")
		idleTTL     = flag.Duration("idle-ttl", mcpgateway.DefaultIdleTTL, "stdio process idle reap TTL")
	)
	flag.Parse()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	dbURL := firstNonEmpty(*databaseURL, os.Getenv("DATABASE_URL"))
	if strings.TrimSpace(dbURL) == "" {
		slog.Error("DATABASE_URL is required")
		os.Exit(1)
	}
	addr := firstNonEmpty(*listen, os.Getenv("GA_MCP_GATEWAY_LISTEN"), "0.0.0.0:8083")
	workRootPath := firstNonEmpty(*workRoot, os.Getenv("GA_MCP_GATEWAY_WORK_ROOT"), "/tmp/mcp-gateway")

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		slog.Error("connect postgres", "error", err)
		os.Exit(1)
	}
	defer pool.Close()

	gateway := mcpgateway.New(mcpgateway.Config{
		Catalog:  &postgresCatalog{pool: pool},
		WorkRoot: workRootPath,
		IdleTTL:  *idleTTL,
	})
	defer gateway.Close()

	server := &http.Server{
		Addr:              addr,
		Handler:           gateway.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       90 * time.Second,
	}

	go func() {
		slog.Info("mcp-gateway listening", "addr", addr, "work_root", workRootPath)
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("mcp-gateway server failed", "error", err)
			stop()
		}
	}()

	reapCtx, reapCancel := context.WithCancel(context.Background())
	go gateway.ReapLoop(reapCtx, 30*time.Second)

	<-ctx.Done()
	slog.Info("mcp-gateway shutting down")
	reapCancel()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_ = server.Shutdown(shutdownCtx)
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}
