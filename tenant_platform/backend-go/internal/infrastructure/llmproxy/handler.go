package llmproxy

import (
	"bytes"
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
		writeError(w, http.StatusUnauthorized, capabilityErrorCode(err), "capability token rejected")
		return CapabilityClaims{}, false
	}
	return claims, true
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
