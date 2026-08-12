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

// MCPTarget 是 proxy 解析出的上游目标。
// URL 为空字符串表示不存在; Headers 是平台侧持有的凭据头(Authorization/
// x-api-key 等), proxy 转发时注入上游——worker 快照永不携带(EPIC D8')。
type MCPTarget struct {
	URL     string
	Headers map[string]string
}

// WorkerMCPProxy 是 Runner 经 Platform 的受控 MCP 代理(Runner 仅 internal
// 网络, 无公网出口——外部 MCP Server 一律经此代理, 与 Sophub proxy 同模式):
//   - Runner 不持有任何外部凭据, 只持有短期 capability JWT(audience=ga-mcp-proxy);
//   - server_id → 目标的映射由 Platform 的启用中 MCP 表决定, 即白名单
//     (管理员未启用的 server 一律 404);
//   - 调用按 JTI 原子计量(预算耗尽 429, 无预算 fail-closed 拒绝);
//   - 计量语义(审查 Y5): 仅对"调用已发起"的请求消费 JTI 预算——客户端
//     错误(401/400)、白名单拒绝(404)、配额拒绝(429)与系统故障(503)
//     路径不消费; 上游调用失败(502)视为已发起, 消费。
//   - 仅转发 JSON-RPC 流(Streamable HTTP), 不缓存/不解析内容;
//   - 转发时注入平台侧持有的凭据头(MCPTarget.Headers, 来自 mcp_servers.headers);
//   - 配额强制: 每次调用按 owner(经 sessionKey 解析)对 day+month 周期原子
//     扣减, 任一耗尽 → 429 MCP_QUOTA_EXCEEDED(quota 为 nil 时跳过, 仅测试)。
//
// 单一 http.Client: 30s 响应头保护(挂死服务器快速失败; SSE 流式不受限)。
type WorkerMCPProxy struct {
	resolve  func(ctx context.Context, serverID string) (MCPTarget, bool, error)
	validate func(ctx context.Context, token string) (llmproxy.CapabilityClaims, error)
	consume  func(ctx context.Context, jtiHash [32]byte, maxCalls int64) (bool, error)
	// quotaCheck 是配额只读预检(MCPQuotaAvailable, 不扣减); quotaConsume
	// 是配额条件扣减(ConsumeMCPQuotas)。两阶段分离(审查 Y5 二轮): 预检
	// 在 JTI 消费之前, 扣减在 JTI 消费之后——任一拒绝路径都不产生任何
	// 扣减副作用(仅极端竞态下 JTI 可能白扣一次, 短期预算可接受)。
	quotaCheck  func(ctx context.Context, sessionKey, serverID string) (bool, error)
	quotaConsume func(ctx context.Context, sessionKey, serverID string) (bool, error)
	client      *http.Client // 30s 响应头超时(挂死服务器快速失败)
}

// NewWorkerMCPProxy wires the proxy to the enabled-MCP resolver, token
// validator, budget counter, quota pre-check and quota consumer.
// consume/quotaCheck/quotaConsume 为 nil 时跳过对应检查(仅测试)。
func NewWorkerMCPProxy(
	resolve func(ctx context.Context, serverID string) (MCPTarget, bool, error),
	validate func(ctx context.Context, token string) (llmproxy.CapabilityClaims, error),
	consume func(ctx context.Context, jtiHash [32]byte, maxCalls int64) (bool, error),
	quotaCheck func(ctx context.Context, sessionKey, serverID string) (bool, error),
	quotaConsume func(ctx context.Context, sessionKey, serverID string) (bool, error),
) *WorkerMCPProxy {
	if resolve == nil || validate == nil {
		return nil
	}
	client := &http.Client{
		Transport: &http.Transport{
			DialContext: (&net.Dialer{Timeout: 10 * time.Second}).DialContext,
			// 上游必须尽快开始响应; SSE 流式响应不受此限(头部到达即放行)。
			TLSHandshakeTimeout:   10 * time.Second,
			ResponseHeaderTimeout: 30 * time.Second,
			IdleConnTimeout:       90 * time.Second,
		},
	}
	return &WorkerMCPProxy{
		resolve:      resolve,
		validate:     validate,
		consume:      consume,
		quotaCheck:   quotaCheck,
		quotaConsume: quotaConsume,
		client:       client,
	}
}

// validateToken 只校验 capability, 不消费预算: Bearer 解析 + 签名/在线
// 校验 + audience/operation。返回 claims 与状态码(0 = 放行)。
func (p *WorkerMCPProxy) validateToken(r *http.Request) (llmproxy.CapabilityClaims, int) {
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
	return claims, 0
}

// consumeBudget 按 JTI 原子消费预算(审查 F10 同款 fail-closed)。
// 只应在调用即将发起时调用: 白名单/配额/客户端错误拒绝路径不得消费
// (审查 Y5——原 authenticate 合一, resolve 404 与配额 429 也烧 JTI)。
func (p *WorkerMCPProxy) consumeBudget(r *http.Request, claims llmproxy.CapabilityClaims) int {
	if p.consume == nil {
		return 0
	}
	maxCalls, ok := llmproxy.ParseBudgetMaxTurns(claims.Budget)
	if !ok {
		return http.StatusForbidden
	}
	allowed, err := p.consume(r.Context(), llmproxy.HashJTI(claims.ID), maxCalls)
	if err != nil {
		slog.ErrorContext(r.Context(), "mcp proxy: budget counter failed",
			"jti", claims.ID, "err", err)
		return http.StatusServiceUnavailable
	}
	if !allowed {
		return http.StatusTooManyRequests
	}
	return 0
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
	claims, status := p.validateToken(r)
	if status != 0 {
		writeMCPAuthFailure(w, status, tid)
		return
	}
	serverID := strings.TrimSpace(r.PathValue("server_id"))
	if serverID == "" {
		writeErr(w, http.StatusBadRequest, "MCP_SERVER_ID_REQUIRED", "server_id is required", tid)
		return
	}
	target, ok, err := p.resolve(r.Context(), serverID)
	if err != nil {
		slog.ErrorContext(r.Context(), "mcp proxy: resolve failed", "server_id", serverID, "error", err)
		writeErr(w, http.StatusServiceUnavailable, "MCP_RESOLVE_FAILED", "MCP server store unavailable", tid)
		return
	}
	if !ok {
		writeErr(w, http.StatusNotFound, "MCP_SERVER_NOT_FOUND", "MCP server is not enabled or does not exist", tid)
		return
	}

	upstreamURL := strings.TrimSpace(target.URL)
	if upstreamURL == "" {
		writeErr(w, http.StatusNotFound, "MCP_SERVER_NOT_FOUND", "MCP server is not enabled or does not exist", tid)
		return
	}

	// 配额强制(审查 Y5 二轮): 只读预检先于 JTI 消费——预检拒绝(429)不产生
	// 任何扣减; 条件扣减在 JTI 消费之后——JTI 拒绝(429/403/503)时配额未扣。
	// 两阶段分离保证任一拒绝路径都无扣减副作用(quotaCheck/quotaConsume
	// 为 nil 时跳过, 仅测试)。
	if p.quotaCheck != nil {
		allowed, err := p.quotaCheck(r.Context(), claims.Subject, serverID)
		if err != nil {
			slog.ErrorContext(r.Context(), "mcp proxy: quota check failed", "server_id", serverID, "error", err)
			writeErr(w, http.StatusServiceUnavailable, "MCP_QUOTA_UNAVAILABLE", "quota store unavailable", tid)
			return
		}
		if !allowed {
			writeErr(w, http.StatusTooManyRequests, "MCP_QUOTA_EXCEEDED", "user MCP quota exceeded for this period", tid)
			return
		}
	}

	// 调用即将发起: 消费 JTI 预算(审查 Y5——404/配额 429 路径不消费,
	// 上游调用失败 502 视为已发起, 消费)。
	if status := p.consumeBudget(r, claims); status != 0 {
		writeMCPAuthFailure(w, status, tid)
		return
	}

	// 配额条件扣减(预检已通过, 仅极端并发竞态下可能在此耗尽——此时
	// JTI 已消费一次, 属可接受的短期预算白扣; 用户配额从不错扣)。
	if p.quotaConsume != nil {
		allowed, err := p.quotaConsume(r.Context(), claims.Subject, serverID)
		if err != nil {
			slog.ErrorContext(r.Context(), "mcp proxy: quota consume failed", "server_id", serverID, "error", err)
			writeErr(w, http.StatusServiceUnavailable, "MCP_QUOTA_UNAVAILABLE", "quota store unavailable", tid)
			return
		}
		if !allowed {
			writeErr(w, http.StatusTooManyRequests, "MCP_QUOTA_EXCEEDED", "user MCP quota exceeded for this period", tid)
			return
		}
	}

	upstream, err := http.NewRequestWithContext(r.Context(), http.MethodPost, upstreamURL, r.Body)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "MCP_UPSTREAM_ERROR", "cannot build upstream request", tid)
		return
	}
	// 只转发 MCP 语义头(capability/身份头绝不外泄给第三方 MCP Server);
	// Mcp-Session-Id 是 Streamable HTTP 会话标识, 必须透传。
	for _, key := range []string{"Content-Type", "Accept", "MCP-Protocol-Version", "Mcp-Session-Id", "User-Agent"} {
		if v := r.Header.Get(key); v != "" {
			upstream.Header.Set(key, v)
		}
	}
	upstream.Header.Del("Authorization")
	// 平台侧持有的凭据头注入(D8'): 管理员配置的 headers(Authorization/
	// x-api-key 等)由 proxy 附加——worker 快照不含, 第三方只见凭据不见来源。
	for key, value := range target.Headers {
		upstream.Header.Set(key, value)
	}

	resp, err := p.client.Do(upstream)
	if err != nil {
		// 日志脱敏: 打 server_id 不打完整 URL(url 可能含 query 凭据)。
		slog.WarnContext(r.Context(), "mcp proxy: upstream request failed",
			"server_id", serverID, "error", err)
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
