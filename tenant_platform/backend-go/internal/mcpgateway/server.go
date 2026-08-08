package mcpgateway

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Gateway 是 stdio MCP 进程宿主的 HTTP 面(设计见
// tenant_platform/docs/MCP_GATEWAY_DESIGN.zh-CN.md):
//
//	POST /v1/mcp/{server_id}  — Streamable HTTP 语义的 MCP 端点
//	GET  /healthz             — 存活探针
//	GET  /metrics             — 进程/请求/崩溃计数
//
// 会话模型: gateway 无状态——不维护 MCP 会话表。进程的 MCP 会话状态在
// 子进程内存中, 由进程自身按协议管理; gateway 重启/进程重建不破坏任何
// 客户端会话。initialize 幂等: 池已握手则返回缓存响应(按客户端 id 改写),
// 避免对共享进程重复握手。
type Gateway struct {
	catalog  CatalogSource
	workRoot string // stdio 进程工作目录根(tmpfs 内, 每 server 一子目录)
	idleTTL  time.Duration

	mu     sync.Mutex
	pools  map[string]*stdioPool // serverID → pool
	nextID atomic.Uint64         // 内部 JSON-RPC id(进程握手重放等无客户端 id 场景)

	requests atomic.Int64 // 指标: 总请求数
	failures atomic.Int64 // 指标: 失败请求数
}

// Config 是 Gateway 构造参数。
type Config struct {
	Catalog CatalogSource
	// WorkRoot 是 stdio 子进程工作目录根(必须位于 tmpfs; 空 = os.TempDir())。
	WorkRoot string
	IdleTTL  time.Duration // 零值用 DefaultIdleTTL
}

// New 构造 Gateway。
func New(cfg Config) *Gateway {
	if cfg.Catalog == nil {
		panic("mcpgateway: catalog is required")
	}
	idleTTL := cfg.IdleTTL
	if idleTTL <= 0 {
		idleTTL = DefaultIdleTTL
	}
	workRoot := cfg.WorkRoot
	if workRoot == "" {
		workRoot = filepath.Join(os.TempDir(), "mcp-gateway")
	}
	return &Gateway{
		catalog:  cfg.Catalog,
		workRoot: workRoot,
		idleTTL:  idleTTL,
		pools:    make(map[string]*stdioPool),
	}
}

// Handler 返回网关 HTTP handler(路由 + 健康检查 + 指标)。
func (g *Gateway) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/mcp/{server_id}", g.handleMCP)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})
	mux.HandleFunc("GET /metrics", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(g.metrics())
	})
	return mux
}

// metrics 返回轻量运行指标。
func (g *Gateway) metrics() map[string]any {
	g.mu.Lock()
	procs := 0
	queued := int64(0)
	for _, pool := range g.pools {
		pool.mu.Lock()
		for _, proc := range pool.procs {
			if !proc.dead.Load() {
				procs++
				queued += proc.queue.Load()
			}
		}
		pool.mu.Unlock()
	}
	g.mu.Unlock()
	return map[string]any{
		"servers":   len(g.pools),
		"processes": procs,
		"queued":    queued,
		"requests":  g.requests.Load(),
		"failures":  g.failures.Load(),
	}
}

// Close 回收全部进程与服务退出路径。
func (g *Gateway) Close() {
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, pool := range g.pools {
		pool.close()
	}
	g.pools = make(map[string]*stdioPool)
}

// ReapLoop 周期性回收空闲进程; ctx 取消时退出。
func (g *Gateway) ReapLoop(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = 30 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			g.reap(now)
		}
	}
}

func (g *Gateway) reap(now time.Time) {
	g.mu.Lock()
	defer g.mu.Unlock()
	for serverID, pool := range g.pools {
		if pool.reapIdle(now, g.idleTTL) {
			slog.Debug("mcp gateway: reaped idle stdio processes", "server_id", serverID)
		}
	}
}

// poolFor 返回 server 的进程池; 存在时热更新配置(revision 变化 → 排空)。
func (g *Gateway) poolFor(def Server) *stdioPool {
	g.mu.Lock()
	defer g.mu.Unlock()
	pool, ok := g.pools[def.ServerID]
	if !ok {
		pool = &stdioPool{
			gateway: g,
			def:     def,
			workDir: filepath.Join(g.workRoot, def.ServerID),
		}
		_ = os.MkdirAll(pool.workDir, 0o700)
		g.pools[def.ServerID] = pool
		return pool
	}
	pool.refreshConfig(def)
	return pool
}

// nextInternalID 返回内部 JSON-RPC id(进程握手重放等无客户端 id 场景)。
// 原子计数, 不持锁——spawnLocked(持 pool.mu)路径调用, 避免锁序
// g.mu → pool.mu 与 pool.mu → g.mu 交叉死锁。
func (g *Gateway) nextInternalID() any {
	return g.nextID.Add(1)
}

func handleMCPError(w http.ResponseWriter, tid string, err error) {
	var catalogErr *CatalogError
	switch {
	case errors.As(err, &catalogErr):
		writeGatewayErr(w, http.StatusServiceUnavailable, "MCP_CATALOG_UNAVAILABLE", err.Error(), tid)
	case errors.Is(err, errUnknownServer):
		writeGatewayErr(w, http.StatusNotFound, "MCP_SERVER_NOT_FOUND", err.Error(), tid)
	case errors.Is(err, errInvalidRequest):
		writeGatewayErr(w, http.StatusBadRequest, "MCP_INVALID_REQUEST", err.Error(), tid)
	case errors.Is(err, errCircuitOpen):
		writeGatewayErr(w, http.StatusServiceUnavailable, "MCP_SERVER_CIRCUIT_OPEN", err.Error(), tid)
	case errors.Is(err, errBackoff) || errors.Is(err, errProcessDead):
		writeGatewayErr(w, http.StatusBadGateway, "MCP_UPSTREAM_ERROR", err.Error(), tid)
	default:
		writeGatewayErr(w, http.StatusBadGateway, "MCP_UPSTREAM_ERROR", err.Error(), tid)
	}
}

func writeGatewayErr(w http.ResponseWriter, status int, code, message, tid string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"jsonrpc": "2.0", "id": nil,
		"error": map[string]any{"code": -32000, "message": fmt.Sprintf("%s: %s", code, message)},
		"_tid":  tid,
	})
}

func traceID() string {
	var b [6]byte
	_, _ = rand.Read(b[:])
	return "gw-" + hex.EncodeToString(b[:])
}

func (g *Gateway) handleMCP(w http.ResponseWriter, r *http.Request) {
	g.requests.Add(1)
	tid := traceID()
	serverID := strings.TrimSpace(r.PathValue("server_id"))
	if serverID == "" {
		writeGatewayErr(w, http.StatusBadRequest, "MCP_SERVER_ID_REQUIRED", "server_id is required", tid)
		return
	}
	if r.Header.Get("Content-Type") != "application/json" {
		writeGatewayErr(w, http.StatusUnsupportedMediaType, "MCP_UNSUPPORTED_MEDIA", "Content-Type must be application/json", tid)
		return
	}
	// 白名单先行: 未知 server 一律 404(不解析 body, fail-closed)。
	servers, err := g.catalog.EnabledServers(r.Context())
	if err != nil {
		slog.ErrorContext(r.Context(), "mcp gateway: catalog failed", "error", err)
		handleMCPError(w, tid, &CatalogError{Err: err})
		return
	}
	var def *Server
	for i := range servers {
		if servers[i].ServerID == serverID {
			def = &servers[i]
			break
		}
	}
	if def == nil {
		handleMCPError(w, tid, errUnknownServer)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxRequestBytes))
	if err != nil {
		writeGatewayErr(w, http.StatusBadRequest, "MCP_BAD_BODY", err.Error(), tid)
		return
	}
	var req map[string]any
	if err := json.Unmarshal(body, &req); err != nil || req["method"] == nil {
		writeGatewayErr(w, http.StatusBadRequest, "MCP_INVALID_REQUEST", errInvalidRequest.Error(), tid)
		return
	}
	method, _ := req["method"].(string)
	pool := g.poolFor(*def)

	if method == "initialize" {
		g.handleInitialize(w, r.Context(), tid, pool, req)
		return
	}
	// 非 initialize: 直接转发到进程(进程按协议拒绝未初始化调用;
	// 会话头由客户端持有, gateway 不校验——进程是唯一会话状态所有者)。
	reqID, hasID := req["id"]
	if hasID && !isJSONRPCID(reqID) {
		handleMCPError(w, tid, errInvalidRequest)
		return
	}
	params, _ := req["params"].(map[string]any)
	proc, err := pool.acquire(r.Context())
	if err != nil {
		g.failures.Add(1)
		pool.noteAcquireFailure(err)
		slog.WarnContext(r.Context(), "mcp gateway: acquire failed", "server_id", def.ServerID, "error", err)
		handleMCPError(w, tid, err)
		return
	}
	// JSON-RPC 通知无 id: 只写不读。
	if isNotification(method, req) {
		if _, err := proc.runJSONRPC(r.Context(), nil, method, params, true); err != nil {
			pool.noteCrash()
			g.failures.Add(1)
			slog.WarnContext(r.Context(), "mcp gateway: notification failed", "server_id", def.ServerID, "method", method, "error", err)
			handleMCPError(w, tid, err)
			return
		}
		writeJSONResponse(w, map[string]any{"jsonrpc": "2.0"})
		return
	}
	resp, err := proc.runJSONRPC(r.Context(), reqID, method, params, false)
	if err != nil {
		pool.noteCrash()
		g.failures.Add(1)
		slog.WarnContext(r.Context(), "mcp gateway: call failed", "server_id", def.ServerID, "method", method, "error", err)
		handleMCPError(w, tid, err)
		return
	}
	pool.resetCrashes()
	writeJSONResponse(w, resp)
}

// handleInitialize 处理 initialize: 池未握手则转发握手(记录首个 params),
// 已握手则返回缓存响应并按当前客户端 id 改写(shared 隔离: 进程只握手
// 一次, 绑定首个客户端参数)。
func (g *Gateway) handleInitialize(
	w http.ResponseWriter, ctx context.Context, tid string, pool *stdioPool, req map[string]any,
) {
	clientID := req["id"]
	if !isJSONRPCID(clientID) {
		handleMCPError(w, tid, errInvalidRequest)
		return
	}
	params, _ := req["params"].(map[string]any)
	if _, err := pool.acquire(ctx); err != nil {
		g.failures.Add(1)
		pool.noteAcquireFailure(err)
		slog.WarnContext(ctx, "mcp gateway: acquire failed", "server_id", pool.def.ServerID, "error", err)
		handleMCPError(w, tid, err)
		return
	}
	resp, err := pool.ensureInitialized(ctx, params, clientID)
	if err != nil {
		pool.noteCrash()
		g.failures.Add(1)
		slog.WarnContext(ctx, "mcp gateway: initialize failed", "server_id", pool.def.ServerID, "error", err)
		handleMCPError(w, tid, err)
		return
	}
	pool.resetCrashes()
	// 缓存响应可能由另一客户端触发: 响应 id 必须回显当前客户端。
	writeJSONResponse(w, cloneWithID(resp, clientID))
}

// cloneWithID 返回 msg 的副本, id 字段改写为 reqID(幂等)。
func cloneWithID(msg map[string]any, reqID any) map[string]any {
	if msg == nil {
		return msg
	}
	cloned := make(map[string]any, len(msg)+1)
	for k, v := range msg {
		cloned[k] = v
	}
	cloned["id"] = reqID
	return cloned
}

// isNotification 判断 JSON-RPC 通知(以 notifications/ 开头或无 id)。
func isNotification(method string, req map[string]any) bool {
	if strings.HasPrefix(method, "notifications/") {
		return true
	}
	_, hasID := req["id"]
	return !hasID
}

// isJSONRPCID 校验 JSON-RPC id 类型(number | string, 非 null)。
func isJSONRPCID(id any) bool {
	switch id.(type) {
	case float64, string:
		return true
	case json.Number:
		return true
	}
	return false
}

func writeJSONResponse(w http.ResponseWriter, msg map[string]any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(msg)
}
