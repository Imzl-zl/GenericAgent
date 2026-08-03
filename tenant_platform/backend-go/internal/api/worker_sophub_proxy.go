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

// authenticate 校验 capability 并按 JTI 原子消费预算(审查 F10)。
// 返回 claims 与状态码: 0 = 放行; 非 0 = 已写出错误响应, 调用方直接返回。
// 预算缺失/非法时 fail-closed 拒绝。
func (p *WorkerSophubProxy) authenticate(w http.ResponseWriter, r *http.Request) (llmproxy.CapabilityClaims, int) {
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
	if p.consume != nil {
		maxCalls, ok := llmproxy.ParseBudgetMaxTurns(claims.Budget)
		if !ok {
			// fail-closed(审查 F10): 无预算的 token 不允许调用代理。
			return llmproxy.CapabilityClaims{}, http.StatusForbidden
		}
		allowed, err := p.consume(r.Context(), llmproxy.HashJTI(claims.ID), maxCalls)
		if err != nil {
			slog.ErrorContext(r.Context(), "sophub proxy: budget counter failed",
				"jti", claims.ID, "err", err)
			return llmproxy.CapabilityClaims{}, http.StatusServiceUnavailable
		}
		if !allowed {
			return llmproxy.CapabilityClaims{}, http.StatusTooManyRequests
		}
	}
	return claims, 0
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
	if _, status := p.authenticate(w, r); status != 0 {
		writeAuthFailure(w, status, tid)
		return
	}
	query := strings.TrimSpace(r.URL.Query().Get("q"))
	if query == "" {
		writeErr(w, http.StatusBadRequest, "INVALID_QUERY", "q is required", tid)
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
	if _, status := p.authenticate(w, r); status != 0 {
		writeAuthFailure(w, status, tid)
		return
	}
	remoteID := strings.TrimSpace(r.URL.Query().Get("id"))
	if remoteID == "" {
		writeErr(w, http.StatusBadRequest, "INVALID_SOP_ID", "id is required", tid)
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
