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
	"time"
)

// Gateway 是 stdio/HTTP 统一 transport 网关的 HTTP 面:
//
//	POST /v1/mcp/{server_id}  — Streamable HTTP 语义的 MCP 端点
//	GET  /healthz             — 存活探针
//
// 会话模型: 每个 worker 会话(initialize)映射一个 gateway session id;
// shared 隔离下所有 session 复用同一 stdio 进程(请求串行队列)。
type Gateway struct {
	catalog  CatalogSource
	workRoot string // stdio 进程工作目录根(tmpfs 内, 每 server 一子目录)
	idleTTL  time.Duration

	mu       sync.Mutex
	pools    map[string]*stdioPool // serverID → pool
	sessions map[string]*session   // sessionID → session
	nextID   uint64
}

type session struct {
	id       string
	serverID string
	lastUse  time.Time
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
		sessions: make(map[string]*session),
	}
}

// Handler 返回网关 HTTP handler(路由 + 健康检查)。
func (g *Gateway) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/mcp/{server_id}", g.handleMCP)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})
	return mux
}

// Close 回收全部进程与会话(服务退出路径)。
func (g *Gateway) Close() {
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, pool := range g.pools {
		pool.close()
	}
	g.pools = make(map[string]*stdioPool)
	g.sessions = make(map[string]*session)
}

// ReapLoop 周期性回收空闲进程与过期会话; ctx 取消时退出。
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
	// 会话 TTL 远长于进程 TTL: 进程可重建, 会话是 worker 长期持有的标识,
	// 误删会迫使 worker 重新 initialize(真实 MCP server 拒绝重复握手)。
	sessionTTL := g.idleTTL * 10
	if sessionTTL < time.Hour {
		sessionTTL = time.Hour
	}
	for id, s := range g.sessions {
		if now.Sub(s.lastUse) > sessionTTL {
			delete(g.sessions, id)
		}
	}
	for serverID, pool := range g.pools {
		if pool.reapIdle(now, g.idleTTL) {
			slog.Debug("mcp gateway: reaped idle stdio process", "server_id", serverID)
		}
	}
}

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
	}
	return pool
}

func handleMCPError(w http.ResponseWriter, tid string, err error) {
	var catalogErr *CatalogError
	switch {
	case errors.As(err, &catalogErr):
		writeGatewayErr(w, http.StatusServiceUnavailable, "MCP_CATALOG_UNAVAILABLE", err.Error(), tid)
	case errors.Is(err, errUnknownServer):
		writeGatewayErr(w, http.StatusNotFound, "MCP_SERVER_NOT_FOUND", err.Error(), tid)
	case errors.Is(err, errInvalidSession):
		writeGatewayErr(w, http.StatusBadRequest, "MCP_SESSION_INVALID", err.Error(), tid)
	case errors.Is(err, errInvalidRequest):
		writeGatewayErr(w, http.StatusBadRequest, "MCP_INVALID_REQUEST", err.Error(), tid)
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

var (
	errUnknownServer  = errors.New("MCP server is not enabled or does not exist")
	errInvalidSession = errors.New("missing or unknown Mcp-Session-Id (initialize first)")
	errInvalidRequest = errors.New("request body must be a JSON-RPC object")
)

func newSessionID() string {
	var b [12]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// sessionFor 返回(会话, 是否新建)。
func (g *Gateway) sessionFor(serverID, sessionID string) (*session, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if sessionID != "" {
		if s, ok := g.sessions[sessionID]; ok {
			s.lastUse = time.Now()
			return s, true
		}
		return nil, false
	}
	s := &session{id: newSessionID(), serverID: serverID, lastUse: time.Now()}
	g.sessions[s.id] = s
	return s, false
}

func (g *Gateway) handleMCP(w http.ResponseWriter, r *http.Request) {
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

	sessionID := strings.TrimSpace(r.Header.Get("Mcp-Session-Id"))
	if method == "initialize" {
		g.handleInitialize(w, r.Context(), tid, *def, sessionID, req)
		return
	}
	// 非 initialize 请求必须携带有效会话。
	s, ok := g.sessionFor(serverID, sessionID)
	if !ok {
		handleMCPError(w, tid, errInvalidSession)
		return
	}
	if s.serverID != serverID {
		handleMCPError(w, tid, errInvalidSession)
		return
	}
	pool := g.poolFor(*def)
	g.forwardToProcess(w, r.Context(), tid, pool, sessionID, method, req, false)
}

// handleInitialize 处理 initialize: 首次会话初始化进程, 后续会话复用缓存。
func (g *Gateway) handleInitialize(
	w http.ResponseWriter, ctx context.Context, tid string, def Server, sessionID string, req map[string]any,
) {
	pool := g.poolFor(def)
	params, _ := req["params"].(map[string]any)
	proc, err := pool.acquire(ctx)
	if err != nil {
		slog.WarnContext(ctx, "mcp gateway: acquire failed", "server_id", def.ServerID, "error", err)
		handleMCPError(w, tid, err)
		return
	}
	resp, err := pool.ensureInitialized(ctx, params)
	if err != nil {
		pool.noteCrash()
		slog.WarnContext(ctx, "mcp gateway: initialize failed", "server_id", def.ServerID, "error", err)
		handleMCPError(w, tid, err)
		return
	}
	// 复用已有会话的重复 initialize: 幂等返回缓存(不新建会话)。
	if sessionID != "" {
		if s, ok := g.sessionFor(def.ServerID, sessionID); ok && s.serverID == def.ServerID {
			w.Header().Set("Mcp-Session-Id", sessionID)
			writeJSONResponse(w, resp)
			return
		}
	}
	s, _ := g.sessionFor(def.ServerID, "")
	w.Header().Set("Mcp-Session-Id", s.id)
	writeJSONResponse(w, resp)
	_ = proc
}

// forwardToProcess 转发 tools/list / tools/call / 通知到 stdio 进程。
func (g *Gateway) forwardToProcess(
	w http.ResponseWriter, ctx context.Context, tid string, pool *stdioPool,
	sessionID, method string, req map[string]any, notification bool,
) {
	proc, err := pool.acquire(ctx)
	if err != nil {
		slog.WarnContext(ctx, "mcp gateway: acquire failed", "server_id", pool.def.ServerID, "error", err)
		handleMCPError(w, tid, err)
		return
	}
	if notification {
		params, _ := req["params"].(map[string]any)
		if _, err := proc.runJSONRPC(ctx, nil, method, params, true); err != nil {
			pool.noteCrash()
			slog.WarnContext(ctx, "mcp gateway: notification failed", "server_id", pool.def.ServerID, "method", method, "error", err)
			handleMCPError(w, tid, err)
		} else {
			writeJSONResponse(w, map[string]any{"jsonrpc": "2.0"})
		}
		return
	}
	reqID := g.nextJSONRPCID()
	params, _ := req["params"].(map[string]any)
	resp, err := proc.runJSONRPC(ctx, reqID, method, params, false)
	if err != nil {
		pool.noteCrash()
		slog.WarnContext(ctx, "mcp gateway: call failed", "server_id", pool.def.ServerID, "method", method, "error", err)
		handleMCPError(w, tid, err)
		return
	}
	writeJSONResponse(w, resp)
}

func (g *Gateway) nextJSONRPCID() uint64 {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.nextID++
	return g.nextID
}

func writeJSONResponse(w http.ResponseWriter, msg map[string]any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(msg)
}

func traceID() string {
	var b [6]byte
	_, _ = rand.Read(b[:])
	return "gw-" + hex.EncodeToString(b[:])
}

// maxRequestBytes 限制请求体(JSON-RPC 工具参数, 防内存放大)。
const maxRequestBytes = 1 << 20 // 1 MiB
