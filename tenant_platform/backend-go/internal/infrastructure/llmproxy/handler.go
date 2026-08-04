package llmproxy

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/domain"
)

// MaxWorkerRequestBytes bounds the Worker request body read by the Proxy.
const MaxWorkerRequestBytes = 4 * 1024 * 1024

func (s *Server) handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	s.handleProviderPath(w, r, domain.ProviderNativeOAI)
}

func (s *Server) handleResponses(w http.ResponseWriter, r *http.Request) {
	s.handleProviderPath(w, r, domain.ProviderNativeOAI)
}

func (s *Server) handleMessages(w http.ResponseWriter, r *http.Request) {
	s.handleProviderPath(w, r, domain.ProviderNativeClaude)
}

func (s *Server) handleProviderPath(
	w http.ResponseWriter,
	r *http.Request,
	wantType domain.LLMProviderType,
) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "ROUTE_NOT_ALLOWED", "use POST")
		return
	}
	claims, ok := s.validateCapability(w, r)
	if !ok {
		return
	}
	if !s.consumeBudget(w, r, claims) {
		return
	}
	provider, ok := s.loadBoundProvider(w, r, claims, wantType)
	if !ok {
		return
	}
	body, model, ok := readProxyRequestBody(w, r)
	if !ok {
		return
	}
	if !requestModelMatches(provider.ProviderType, claims.Model, model) {
		writeError(w, http.StatusConflict, "MODEL_MISMATCH", "request model does not match capability")
		return
	}
	// round9 审查: 在线 task/lease/成员联查放在全部 provider/请求语义检查
	// 之后——错误码优先级: 签名/撤销 > 预算 > provider 404/409 > body model
	// > 在线 REVOKED。已终态/被接管/成员移除的任务在此被拒绝, 不等 token
	// 自然过期(撤销/接管/成员变更的生效时间收敛到下一次调用)。
	if err := s.validator.CheckTaskActive(r.Context(), claims); err != nil {
		slog.WarnContext(r.Context(), "llm-proxy: task no longer active",
			"code", capabilityErrorCode(err), "jti", jwtClaimsID(extractBearer(r)), "err", err)
		writeError(w, http.StatusUnauthorized, capabilityErrorCode(err), "capability token rejected")
		return
	}
	target, err := ResolveUpstreamTarget(provider, r.URL)
	if err != nil {
		writeError(w, http.StatusBadGateway, "UPSTREAM_URL_REJECTED", "upstream URL is not allowed")
		return
	}
	realKey, err := s.decryptProviderKey(provider)
	if err != nil {
		slog.ErrorContext(r.Context(), "llmproxy: provider credential unavailable", "provider_id", provider.ID)
		writeError(w, http.StatusInternalServerError, "PROVIDER_CREDENTIAL_UNAVAILABLE", "provider credential unavailable")
		return
	}
	resetRequestBody(r, body)
	requestContext := &proxyRequestContext{
		Claims: claims, Provider: provider, Target: target, RealKey: realKey,
	}
	s.reverseProxy.ServeHTTP(w, attachProxyRequestContext(r, requestContext))
}

func requestModelMatches(providerType domain.LLMProviderType, configured, outbound string) bool {
	if outbound == configured {
		return true
	}
	if providerType != domain.ProviderNativeClaude || !strings.Contains(strings.ToLower(configured), "[1m]") {
		return false
	}
	gaOutbound := strings.ReplaceAll(configured, "[1m]", "")
	gaOutbound = strings.ReplaceAll(gaOutbound, "[1M]", "")
	return outbound == gaOutbound
}

func (s *Server) validateCapability(w http.ResponseWriter, r *http.Request) (CapabilityClaims, bool) {
	token := extractBearer(r)
	if token == "" {
		writeError(w, http.StatusUnauthorized, "CAPABILITY_INVALID", "Authorization Bearer required")
		return CapabilityClaims{}, false
	}
	claims, err := s.validator.ValidateTaskScoped(r.Context(), token)
	if err != nil {
		slog.WarnContext(r.Context(), "llm-proxy: capability rejected",
			"code", capabilityErrorCode(err), "jti", jwtClaimsID(token), "err", err)
		writeError(w, http.StatusUnauthorized, capabilityErrorCode(err), "capability token rejected")
		return CapabilityClaims{}, false
	}
	// 方案 §7: LLM 路由只接受 llm.chat 操作。
	if claims.Operation != "llm.chat" {
		writeError(w, http.StatusUnauthorized, "CAPABILITY_INVALID", "capability operation mismatch")
		return CapabilityClaims{}, false
	}
	return claims, true
}

// consumeBudget 按 claims.Budget 的 max_turns 计量本次调用(审查 R4-I9):
// 预算缺失/非法时拒绝(fail-closed), 超额时 429, 计量后端故障时 503——
// 任何情况下都不允许无界转发。
func (s *Server) consumeBudget(w http.ResponseWriter, r *http.Request, claims CapabilityClaims) bool {
	if s.cfg.UsageCounter == nil {
		writeError(w, http.StatusServiceUnavailable, "BUDGET_UNAVAILABLE", "capability usage counter not configured")
		return false
	}
	maxCalls, ok := ParseBudgetMaxTurns(claims.Budget)
	if !ok {
		writeError(w, http.StatusForbidden, "CAPABILITY_BUDGET_INVALID", "capability budget missing or invalid")
		return false
	}
	allowed, err := s.cfg.UsageCounter.ConsumeCapabilityCall(r.Context(), HashJTI(claims.ID), maxCalls)
	if err != nil {
		slog.ErrorContext(r.Context(), "llmproxy: budget counter failed", "jti", claims.ID, "err", err)
		writeError(w, http.StatusServiceUnavailable, "BUDGET_UNAVAILABLE", "capability budget counter unavailable")
		return false
	}
	if !allowed {
		writeError(w, http.StatusTooManyRequests, "CAPABILITY_BUDGET_EXCEEDED", "capability call budget exceeded")
		return false
	}
	return true
}

// ParseBudgetMaxTurns 从 capability budget JSON 提取 max_turns; 缺失或
// 非正数返回 ok=false(签发方必须填充, 见 worker_credential.go)。导出供
// Sophub proxy 等使用方复用同一预算语义(审查 F10)。
func ParseBudgetMaxTurns(budget string) (int64, bool) {
	if strings.TrimSpace(budget) == "" {
		return 0, false
	}
	var parsed struct {
		MaxTurns int64 `json:"max_turns"`
	}
	if err := json.Unmarshal([]byte(budget), &parsed); err != nil {
		return 0, false
	}
	if parsed.MaxTurns <= 0 {
		return 0, false
	}
	return parsed.MaxTurns, true
}

// jwtClaimsID 提取 JWT payload 的 jti 声明(仅调试日志; 解析失败返回空)。
func jwtClaimsID(token string) string {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return ""
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return ""
	}
	var claims struct {
		JTI string `json:"jti"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return ""
	}
	return claims.JTI
}

func capabilityErrorCode(err error) string {
	switch {
	case errors.Is(err, ErrCapabilityExpired):
		return "CAPABILITY_EXPIRED"
	case errors.Is(err, ErrCapabilityRevoked):
		return "CAPABILITY_REVOKED"
	case errors.Is(err, ErrCapabilityAudienceMismatch):
		return "CAPABILITY_AUDIENCE_MISMATCH"
	default:
		return "CAPABILITY_INVALID"
	}
}

func (s *Server) loadBoundProvider(
	w http.ResponseWriter,
	r *http.Request,
	claims CapabilityClaims,
	wantType domain.LLMProviderType,
) (domain.LLMProvider, bool) {
	provider, err := s.cfg.ProviderSource.GetProvider(r.Context(), claims.ProviderID)
	if err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			slog.ErrorContext(r.Context(), "llmproxy: provider lookup failed", "provider_id", claims.ProviderID)
			writeError(w, http.StatusInternalServerError, "PROVIDER_LOOKUP_FAILED", "provider lookup failed")
			return domain.LLMProvider{}, false
		}
		writeError(w, http.StatusNotFound, "PROVIDER_NOT_FOUND", "provider not found")
		return domain.LLMProvider{}, false
	}
	if provider.ID != claims.ProviderID {
		writeError(w, http.StatusNotFound, "PROVIDER_NOT_FOUND", "provider not found")
		return domain.LLMProvider{}, false
	}
	if !provider.IsActive() {
		writeError(w, http.StatusConflict, "PROVIDER_DISABLED", "provider is disabled")
		return domain.LLMProvider{}, false
	}
	if provider.Revision != claims.ProviderRevision {
		writeError(w, http.StatusConflict, "PROVIDER_REVISION_MISMATCH", "provider revision changed")
		return domain.LLMProvider{}, false
	}
	if provider.ProviderType != claims.ProviderType || claims.ProviderType != wantType {
		writeError(w, http.StatusConflict, "PROVIDER_TYPE_MISMATCH", "provider type does not match route")
		return domain.LLMProvider{}, false
	}
	if provider.Model != claims.Model {
		writeError(w, http.StatusConflict, "MODEL_MISMATCH", "provider model changed")
		return domain.LLMProvider{}, false
	}
	return provider, true
}

func readProxyRequestBody(w http.ResponseWriter, r *http.Request) ([]byte, string, bool) {
	body, err := io.ReadAll(io.LimitReader(r.Body, MaxWorkerRequestBytes+1))
	if err != nil {
		writeError(w, http.StatusBadRequest, "ROUTE_NOT_ALLOWED", "request body could not be read")
		return nil, "", false
	}
	if len(body) > MaxWorkerRequestBytes {
		writeError(w, http.StatusRequestEntityTooLarge, "BODY_TOO_LARGE", "request exceeds limit")
		return nil, "", false
	}
	model, err := decodeUniqueTopLevelModel(body)
	if err != nil {
		writeError(w, http.StatusBadRequest, "ROUTE_NOT_ALLOWED", "request body must contain one string model")
		return nil, "", false
	}
	return body, model, true
}

func decodeUniqueTopLevelModel(body []byte) (string, error) {
	decoder := json.NewDecoder(bytes.NewReader(body))
	opening, err := decoder.Token()
	if err != nil || opening != json.Delim('{') {
		return "", errors.New("request body must be a JSON object")
	}
	var model string
	found := false
	for decoder.More() {
		nameToken, err := decoder.Token()
		if err != nil {
			return "", err
		}
		name, ok := nameToken.(string)
		if !ok {
			return "", errors.New("request field name must be a string")
		}
		if name == "model" {
			if found {
				return "", errors.New("request contains duplicate model fields")
			}
			if err := decoder.Decode(&model); err != nil || model == "" {
				return "", errors.New("request model must be a non-empty string")
			}
			found = true
			continue
		}
		var ignored json.RawMessage
		if err := decoder.Decode(&ignored); err != nil {
			return "", err
		}
	}
	if _, err := decoder.Token(); err != nil {
		return "", err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return "", errors.New("request body contains trailing JSON")
	}
	if !found {
		return "", errors.New("request model is required")
	}
	return model, nil
}

func resetRequestBody(request *http.Request, body []byte) {
	_ = request.Body.Close()
	request.Body = io.NopCloser(bytes.NewReader(body))
	request.ContentLength = int64(len(body))
	request.GetBody = nil
}

func (s *Server) decryptProviderKey(provider domain.LLMProvider) (string, error) {
	version, err := strconv.Atoi(provider.APIKeyKeyVersion)
	if err != nil {
		return "", err
	}
	plaintext, err := s.cfg.Cipher.Decrypt(provider.APIKeyCiphertext, version)
	if err != nil {
		return "", err
	}
	realKey := string(plaintext)
	clear(plaintext)
	if realKey == "" {
		return "", errors.New("provider credential is empty")
	}
	return realKey, nil
}

func extractBearer(r *http.Request) string {
	header := r.Header.Get("Authorization")
	if !strings.HasPrefix(header, "Bearer ") {
		return ""
	}
	return strings.TrimPrefix(header, "Bearer ")
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"code": code, "message": message})
}
