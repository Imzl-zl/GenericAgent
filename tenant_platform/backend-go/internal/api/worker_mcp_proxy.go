package api

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/infrastructure/llmproxy"
)

// WorkerMCPProxy 是 Runner 经 Platform 的受控 MCP 代理(Runner 仅 internal
// 网络, 无公网出口——外部 MCP Server 一律经此代理, 与 Sophub proxy 同模式):
//   - Runner 不持有任何外部凭据, 只持有短期 capability JWT(audience=ga-mcp-proxy);
//   - server_id → URL 的映射由 Platform 的启用中 MCP 表决定, 即白名单
//     (管理员未启用的 server 一律 404);
//   - 调用按 JTI 原子计量(预算耗尽 429, 无预算 fail-closed 拒绝);
//   - 仅转发 JSON-RPC 流(Streamable HTTP), 不缓存/不解析内容。
type WorkerMCPProxy struct {
	resolve  func(ctx context.Context, serverID string) (string, bool, error)
	validate func(ctx context.Context, token string) (llmproxy.CapabilityClaims, error)
	consume  func(ctx context.Context, jtiHash [32]byte, maxCalls int64) (bool, error)
	client   *http.Client
}

// NewWorkerMCPProxy wires the proxy to the enabled-MCP resolver, token
// validator and budget counter. consume 为 nil 时跳过计量(仅测试)。
func NewWorkerMCPProxy(
	resolve func(ctx context.Context, serverID string) (string, bool, error),
	validate func(ctx context.Context, token string) (llmproxy.CapabilityClaims, error),
	consume func(ctx context.Context, jtiHash [32]byte, maxCalls int64) (bool, error),
) *WorkerMCPProxy {
	if resolve == nil || validate == nil {
		return nil
	}
	return &WorkerMCPProxy{
		resolve:  resolve,
		validate: validate,
		consume:  consume,
		client: &http.Client{
			Transport: &http.Transport{
				DialContext: (&net.Dialer{Timeout: 10 * time.Second}).DialContext,
				// 上游必须尽快开始响应; SSE 流式响应不受此限(头部到达即放行)。
				TLSHandshakeTimeout: 10 * time.Second,
				ResponseHeaderTimeout: 30 * time.Second,
				IdleConnTimeout:      90 * time.Second,
			},
		},
	}
}

// authenticate 校验 capability 并按 JTI 原子消费预算(审查 F10 同款 fail-closed)。
func (p *WorkerMCPProxy) authenticate(w http.ResponseWriter, r *http.Request) (llmproxy.CapabilityClaims, int) {
	auth := strings.TrimSpace(r.Header.Get("Authorization"))
	token, ok := strings.CutPrefix(auth, "Bearer ")
	if !ok || strings.TrimSpace(token) == "" {
		return llmproxy.CapabilityClaims{}, http.StatusUnauthorized
	}
	claims, err := p.validate(r.Context(), strings.TrimSpace(token))
	if err != nil {
		slog.WarnContext(r.Context(), "mcp proxy: capability rejected", "error", err)
		return llmproxy.CapabilityClaims{}, http.StatusUnauthorized
	}
	if !claims.VerifyAudience(llmproxy.MCPAudience, true) || claims.Operation != "mcp" {
		return llmproxy.CapabilityClaims{}, http.StatusUnauthorized
	}
	if p.consume != nil {
		maxCalls, ok := llmproxy.ParseBudgetMaxTurns(claims.Budget)
		if !ok {
			return llmproxy.CapabilityClaims{}, http.StatusForbidden
		}
		allowed, err := p.consume(r.Context(), llmproxy.HashJTI(claims.ID), maxCalls)
		if err != nil {
			slog.ErrorContext(r.Context(), "mcp proxy: budget counter failed",
				"jti", claims.ID, "err", err)
			return llmproxy.CapabilityClaims{}, http.StatusServiceUnavailable
		}
		if !allowed {
			return llmproxy.CapabilityClaims{}, http.StatusTooManyRequests
		}
	}
	return claims, 0
}

func writeMCPAuthFailure(w http.ResponseWriter, status int, tid string) {
	switch status {
	case http.StatusTooManyRequests:
		writeErr(w, status, "MCP_BUDGET_EXCEEDED", "capability call budget exceeded", tid)
	case http.StatusForbidden:
		writeErr(w, status, "MCP_BUDGET_INVALID", "capability budget missing or invalid", tid)
	case http.StatusServiceUnavailable:
		writeErr(w, status, "MCP_BUDGET_UNAVAILABLE", "capability budget counter unavailable", tid)
	default:
		writeErr(w, http.StatusUnauthorized, "MCP_UNAUTHORIZED", "missing or invalid worker capability", tid)
	}
}

// hop-by-hop 头不转发(HTTP/1.1 语义, 代理侧自管连接)。
var mcpProxyHopHeaders = map[string]struct{}{
	"Connection":          {},
	"Keep-Alive":          {},
	"Proxy-Authenticate":  {},
	"Proxy-Authorization": {},
	"Te":                  {},
	"Trailer":             {},
	"Transfer-Encoding":   {},
	"Upgrade":             {},
}

// ServeProxy 实现 POST /v1/worker/mcp/{server_id}: 校验 capability 后把
// JSON-RPC 请求体原样转发到已启用 MCP Server 的真实 URL, 响应(含 SSE)流式回传。
func (p *WorkerMCPProxy) ServeProxy(w http.ResponseWriter, r *http.Request) {
	tid := traceID()
	if _, status := p.authenticate(w, r); status != 0 {
		writeMCPAuthFailure(w, status, tid)
		return
	}
	serverID := strings.TrimSpace(r.PathValue("server_id"))
	if serverID == "" {
		writeErr(w, http.StatusBadRequest, "MCP_SERVER_ID_REQUIRED", "server_id is required", tid)
		return
	}
	upstreamURL, ok, err := p.resolve(r.Context(), serverID)
	if err != nil {
		slog.ErrorContext(r.Context(), "mcp proxy: resolve failed", "server_id", serverID, "error", err)
		writeErr(w, http.StatusServiceUnavailable, "MCP_RESOLVE_FAILED", "MCP server store unavailable", tid)
		return
	}
	if !ok {
		writeErr(w, http.StatusNotFound, "MCP_SERVER_NOT_FOUND", "MCP server is not enabled or does not exist", tid)
		return
	}

	upstream, err := http.NewRequestWithContext(r.Context(), http.MethodPost, upstreamURL, r.Body)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "MCP_UPSTREAM_ERROR", "cannot build upstream request", tid)
		return
	}
	// 只转发 MCP 语义头; capability/身份头绝不外泄给第三方 MCP Server。
	for _, key := range []string{"Content-Type", "Accept", "MCP-Protocol-Version", "User-Agent"} {
		if v := r.Header.Get(key); v != "" {
			upstream.Header.Set(key, v)
		}
	}
	upstream.Header.Del("Authorization")

	resp, err := p.client.Do(upstream)
	if err != nil {
		slog.WarnContext(r.Context(), "mcp proxy: upstream request failed",
			"server_id", serverID, "url", upstreamURL, "error", err)
		writeErr(w, http.StatusBadGateway, "MCP_UPSTREAM_ERROR", "MCP server unreachable", tid)
		return
	}
	defer resp.Body.Close()

	for key, values := range resp.Header {
		if _, hop := mcpProxyHopHeaders[key]; hop {
			continue
		}
		if key == "Content-Length" || strings.EqualFold(key, "Connection") {
			continue
		}
		for _, v := range values {
			w.Header().Add(key, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	flush, _ := w.(http.Flusher)
	buf := make([]byte, 32*1024)
	for {
		n, readErr := resp.Body.Read(buf)
		if n > 0 {
			if _, writeErr := w.Write(buf[:n]); writeErr != nil {
				return
			}
			if flush != nil {
				flush.Flush()
			}
		}
		if readErr != nil {
			return
		}
	}
}

// NewWorkerMCPHandler 返回内部 listener 专用 handler: 只注册
// capability-protected 的 /v1/worker/mcp/* 路由(审查 R5-C1 同款), 不注册
// 任何管理/用户 API。proxy 为 nil 时返回 nil(调用方跳过内部 listener)。
func NewWorkerMCPHandler(proxy *WorkerMCPProxy) http.Handler {
	if proxy == nil {
		return nil
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/worker/mcp/{server_id}", proxy.ServeProxy)
	return mux
}
