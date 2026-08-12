package api

import (
	"context"
	"log/slog"
	"net/http"
	"strings"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/domain"
	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/infrastructure/llmproxy"
)

// maxSophubInstallBytes 是单次 SOP 安装下载的内容字节上限(审查 F10:
// 下载字节预算的近似执行——fetch 结果超过上限直接拒绝)。
const maxSophubInstallBytes = 256 * 1024

// WorkerSophubProxy 是 Runner 经 Platform 的受控 Sophub 代理(方案 §5.2):
// - Runner 不持有 Sophub API Key, 只持有短期 capability JWT;
// - 仅允许公开 approved single-file markdown;
// - 结果由 Worker 写入当前工作区 memory/sops/, 不进入全局注册表;
// - 调用按 JTI 原子计量(审查 F10: token 预算耗尽后 429, 无预算 fail-closed)。
type WorkerSophubProxy struct {
	search   func(ctx context.Context, query string, page, pageSize int) (domain.SophubSearchResult, error)
	fetch    func(ctx context.Context, remoteSOPID string) (domain.SophubRemoteSOP, error)
	validate func(ctx context.Context, token string) (llmproxy.CapabilityClaims, error)
	consume  func(ctx context.Context, jtiHash [32]byte, maxCalls int64) (bool, error)
}

// NewWorkerSophubProxy wires the proxy to the Sophub service and token validator.
// consume 为 nil 时跳过计量(仅测试), 生产必须接线。
func NewWorkerSophubProxy(
	search func(ctx context.Context, query string, page, pageSize int) (domain.SophubSearchResult, error),
	fetch func(ctx context.Context, remoteSOPID string) (domain.SophubRemoteSOP, error),
	validate func(ctx context.Context, token string) (llmproxy.CapabilityClaims, error),
	consume func(ctx context.Context, jtiHash [32]byte, maxCalls int64) (bool, error),
) *WorkerSophubProxy {
	return &WorkerSophubProxy{search: search, fetch: fetch, validate: validate, consume: consume}
}

// validateToken 只校验 capability, 不消费预算: Bearer 解析 + 签名/在线
// 校验 + audience/operation。返回 claims 与状态码(0 = 放行)。
func (p *WorkerSophubProxy) validateToken(r *http.Request) (llmproxy.CapabilityClaims, int) {
	auth := strings.TrimSpace(r.Header.Get("Authorization"))
	token, ok := strings.CutPrefix(auth, "Bearer ")
	if !ok || strings.TrimSpace(token) == "" {
		return llmproxy.CapabilityClaims{}, http.StatusUnauthorized
	}
	claims, err := p.validate(r.Context(), strings.TrimSpace(token))
	if err != nil {
		slog.WarnContext(r.Context(), "sophub proxy: capability rejected", "error", err)
		return llmproxy.CapabilityClaims{}, http.StatusUnauthorized
	}
	if !claims.VerifyAudience(llmproxy.SophubAudience, true) || claims.Operation != "sophub" {
		return llmproxy.CapabilityClaims{}, http.StatusUnauthorized
	}
	return claims, 0
}

// consumeBudget 按 JTI 原子消费预算(审查 F10 同款 fail-closed)。
// 只应在调用即将发起时调用: 客户端错误(401/400)与系统故障(503)路径
// 不消费(审查 Y5——原 authenticate 合一, 参数校验 400 也烧 JTI);
// 上游调用失败(502)与 fetch 后判定(403)视为已发起, 消费。
func (p *WorkerSophubProxy) consumeBudget(r *http.Request, claims llmproxy.CapabilityClaims) int {
	if p.consume == nil {
		return 0
	}
	maxCalls, ok := llmproxy.ParseBudgetMaxTurns(claims.Budget)
	if !ok {
		// fail-closed(审查 F10): 无预算的 token 不允许调用代理。
		return http.StatusForbidden
	}
	allowed, err := p.consume(r.Context(), llmproxy.HashJTI(claims.ID), maxCalls)
	if err != nil {
		slog.ErrorContext(r.Context(), "sophub proxy: budget counter failed",
			"jti", claims.ID, "err", err)
		return http.StatusServiceUnavailable
	}
	if !allowed {
		return http.StatusTooManyRequests
	}
	return 0
}

// writeAuthFailure 把 authenticate 返回的状态码转成标准错误响应。
func writeAuthFailure(w http.ResponseWriter, status int, tid string) {
	switch status {
	case http.StatusTooManyRequests:
		writeErr(w, status, "SOPHUB_BUDGET_EXCEEDED", "capability call budget exceeded", tid)
	case http.StatusForbidden:
		writeErr(w, status, "SOPHUB_BUDGET_INVALID", "capability budget missing or invalid", tid)
	case http.StatusServiceUnavailable:
		writeErr(w, status, "SOPHUB_BUDGET_UNAVAILABLE", "capability budget counter unavailable", tid)
	default:
		writeErr(w, http.StatusUnauthorized, "SOPHUB_UNAUTHORIZED", "missing or invalid worker capability", tid)
	}
}

// ServeSearch 实现 GET /v1/worker/sophub/search?q=...
func (p *WorkerSophubProxy) ServeSearch(w http.ResponseWriter, r *http.Request) {
	tid := traceID()
	claims, status := p.validateToken(r)
	if status != 0 {
		writeAuthFailure(w, status, tid)
		return
	}
	query := strings.TrimSpace(r.URL.Query().Get("q"))
	if query == "" {
		writeErr(w, http.StatusBadRequest, "INVALID_QUERY", "q is required", tid)
		return
	}
	// 调用即将发起: 消费 JTI 预算(审查 Y5——参数校验 400 不消费)。
	if status := p.consumeBudget(r, claims); status != 0 {
		writeAuthFailure(w, status, tid)
		return
	}
	result, err := p.search(r.Context(), query, 1, 24)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "SOPHUB_SEARCH_FAILED", "sophub search failed", tid)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

// ServeInstall 实现 GET /v1/worker/sophub/install?id=...
// 仅返回公开 approved markdown 内容; 落盘由 Worker 在工作区完成。
func (p *WorkerSophubProxy) ServeInstall(w http.ResponseWriter, r *http.Request) {
	tid := traceID()
	claims, status := p.validateToken(r)
	if status != 0 {
		writeAuthFailure(w, status, tid)
		return
	}
	remoteID := strings.TrimSpace(r.URL.Query().Get("id"))
	if remoteID == "" {
		writeErr(w, http.StatusBadRequest, "INVALID_SOP_ID", "id is required", tid)
		return
	}
	// 调用即将发起: 消费 JTI 预算(审查 Y5——参数校验 400 不消费; fetch
	// 后 403 判定视为已发起, 消费)。
	if status := p.consumeBudget(r, claims); status != 0 {
		writeAuthFailure(w, status, tid)
		return
	}
	sop, err := p.fetch(r.Context(), remoteID)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "SOPHUB_FETCH_FAILED", "sophub fetch failed", tid)
		return
	}
	// 返回的 SOP 必须与请求 id 一致, 防止代理端混淆(方案 §5.2)。
	if sop.ID != remoteID {
		writeErr(w, http.StatusBadGateway, "SOPHUB_IDENTITY_MISMATCH", "sophub returned a different SOP", tid)
		return
	}
	// 仅公开 approved single-file markdown(方案 §5.2): public 属性由
	// 服务端 fetch 层保证(仅返回 approved 公开项), 此处校验类型与包形态。
	if !strings.EqualFold(sop.FileType, domain.SOPFileTypeMarkdown) ||
		!strings.EqualFold(sop.Status, "approved") ||
		!strings.EqualFold(sop.PackageType, "single_file") {
		writeErr(w, http.StatusForbidden, "SOPHUB_NOT_PUBLIC", "only public approved single-file markdown SOPs are downloadable", tid)
		return
	}
	// 下载字节上限(审查 F10): 防止无限拉取超大 SOP 内容。
	if len(sop.Content) > maxSophubInstallBytes {
		writeErr(w, http.StatusForbidden, "SOPHUB_CONTENT_TOO_LARGE",
			"sophub content exceeds download size limit", tid)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{
		"id":      sop.ID,
		"title":   sop.Title,
		"content": sop.Content,
	})
}

// NewWorkerSophubHandler 返回内部 listener 专用 handler(审查 R5-C1):
// 只注册 capability-protected 的 /v1/worker/sophub/* 路由, 不注册任何
// 管理/用户 API。供 --worker-internal-listen 显式启用的内部 listener 使用;
// proxy 为 nil 时返回 nil(调用方跳过内部 listener)。
func NewWorkerSophubHandler(proxy *WorkerSophubProxy) http.Handler {
	if proxy == nil {
		return nil
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/worker/sophub/search", proxy.ServeSearch)
	mux.HandleFunc("GET /v1/worker/sophub/install", proxy.ServeInstall)
	return mux
}

// WorkerSophubHandler 返回本 Server 的 capability-protected Worker Sophub
// handler(内部 listener 专用, 审查 R5-C1); sophubProxy 未接线时返回 nil。
func (s *Server) WorkerSophubHandler() http.Handler {
	return NewWorkerSophubHandler(s.sophubProxy)
}

// WorkerMCPHandler 返回本 Server 的 capability-protected Worker MCP
// handler(内部 listener 专用); mcpProxy 未接线时返回 nil。
func (s *Server) WorkerMCPHandler() http.Handler {
	return NewWorkerMCPHandler(s.mcpProxy)
}

// WorkerInternalHandler 合并内部 listener 的全部 worker 代理路由
// (/v1/worker/sophub/* 与 /v1/worker/mcp/*)。全部未接线时返回 nil。
func (s *Server) WorkerInternalHandler() http.Handler {
	var mux *http.ServeMux
	if h := s.WorkerSophubHandler(); h != nil {
		mux = http.NewServeMux()
		mux.Handle("/v1/worker/sophub/", h)
	}
	if h := s.WorkerMCPHandler(); h != nil {
		if mux == nil {
			mux = http.NewServeMux()
		}
		mux.Handle("/v1/worker/mcp/", h)
	}
	return mux
}
