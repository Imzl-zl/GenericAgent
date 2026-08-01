package api

import (
	"context"
	"log/slog"
	"net/http"
	"strings"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/domain"
	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/infrastructure/llmproxy"
)

// WorkerSophubProxy 是 Runner 经 Platform 的受控 Sophub 代理(方案 §5.2):
// - Runner 不持有 Sophub API Key, 只持有短期 capability JWT;
// - 仅允许公开 approved single-file markdown;
// - 结果由 Worker 写入当前工作区 memory/sops/, 不进入全局注册表。
type WorkerSophubProxy struct {
	search   func(ctx context.Context, query string, page, pageSize int) (domain.SophubSearchResult, error)
	fetch    func(ctx context.Context, remoteSOPID string) (domain.SophubRemoteSOP, error)
	validate func(ctx context.Context, token string) (llmproxy.CapabilityClaims, error)
}

// NewWorkerSophubProxy wires the proxy to the Sophub service and token validator.
func NewWorkerSophubProxy(
	search func(ctx context.Context, query string, page, pageSize int) (domain.SophubSearchResult, error),
	fetch func(ctx context.Context, remoteSOPID string) (domain.SophubRemoteSOP, error),
	validate func(ctx context.Context, token string) (llmproxy.CapabilityClaims, error),
) *WorkerSophubProxy {
	return &WorkerSophubProxy{search: search, fetch: fetch, validate: validate}
}

func (p *WorkerSophubProxy) authenticate(r *http.Request) bool {
	auth := strings.TrimSpace(r.Header.Get("Authorization"))
	token, ok := strings.CutPrefix(auth, "Bearer ")
	if !ok || strings.TrimSpace(token) == "" {
		return false
	}
	claims, err := p.validate(r.Context(), strings.TrimSpace(token))
	if err != nil {
		slog.WarnContext(r.Context(), "sophub proxy: capability rejected", "error", err)
		return false
	}
	return claims.VerifyAudience(llmproxy.SophubAudience, true)
}

// ServeSearch 实现 GET /v1/worker/sophub/search?q=...
func (p *WorkerSophubProxy) ServeSearch(w http.ResponseWriter, r *http.Request) {
	tid := traceID()
	if !p.authenticate(r) {
		writeErr(w, http.StatusUnauthorized, "SOPHUB_UNAUTHORIZED", "missing or invalid worker capability", tid)
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
	if !p.authenticate(r) {
		writeErr(w, http.StatusUnauthorized, "SOPHUB_UNAUTHORIZED", "missing or invalid worker capability", tid)
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
	// 仅公开 approved single-file markdown(方案 §5.2)。
	if !strings.EqualFold(sop.FileType, domain.SOPFileTypeMarkdown) || !strings.EqualFold(sop.Status, "approved") {
		writeErr(w, http.StatusForbidden, "SOPHUB_NOT_PUBLIC", "only public approved markdown SOPs are downloadable", tid)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{
		"id":      sop.ID,
		"title":   sop.Title,
		"content": sop.Content,
	})
}
