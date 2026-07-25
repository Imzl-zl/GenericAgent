package llmproxy

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/domain"
)

// MaxWorkerRequestBytes bounds the Worker request body read by the Proxy.
const MaxWorkerRequestBytes = 4 * 1024 * 1024

// handleChatCompletions validates the capability_token and forwards the body
// upstream. The active provider must be OpenAI-compatible for this path.
func (s *Server) handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	s.handleProviderPath(w, r, domain.ProviderOpenAICompatible)
}

// handleMessages is the Anthropic Messages API proxy path. The active provider
// must be anthropic_messages for this path.
func (s *Server) handleMessages(w http.ResponseWriter, r *http.Request) {
	s.handleProviderPath(w, r, domain.ProviderAnthropicMessages)
}

func (s *Server) handleProviderPath(w http.ResponseWriter, r *http.Request, wantType domain.LLMProviderType) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "use POST")
		return
	}
	token := extractBearer(r)
	if token == "" {
		writeError(w, http.StatusUnauthorized, "MISSING_TOKEN", "Authorization Bearer required")
		return
	}
	// ValidateUnscoped: the LLM Proxy ingress does not have the caller's
	// session context (the Worker holds it). Signature + expiry + revocation
	// are still enforced. Per-session binding is enforced by the Worker before
	// it mints the token, and by the platform before it hands the token to the
	// Worker: a stolen token can only be used within its own TTL, not replayed
	// across sessions once the issuing session is revoked.
	claims, err := s.validator.ValidateUnscoped(token)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "INVALID_TOKEN", err.Error())
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, MaxWorkerRequestBytes+1))
	if err != nil {
		writeError(w, http.StatusBadRequest, "READ_BODY", err.Error())
		return
	}
	if len(body) > MaxWorkerRequestBytes {
		writeError(w, http.StatusRequestEntityTooLarge, "BODY_TOO_LARGE", "request exceeds limit")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), defaultUpstreamTimeout)
	defer cancel()
	p, err := s.currentProvider(ctx)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeError(w, http.StatusServiceUnavailable, "NO_PROVIDER", "no default LLM provider configured")
			return
		}
		writeError(w, http.StatusInternalServerError, "PROVIDER_RESOLVE", err.Error())
		return
	}
	if p.ProviderType != wantType {
		writeError(w, http.StatusConflict, "PROVIDER_MISMATCH",
			"active provider is "+string(p.ProviderType)+", expected "+string(wantType))
		return
	}

	resp, err := s.upstream.Forward(ctx, p, UpstreamRequest{Body: body})
	if err != nil {
		status := http.StatusBadGateway
		if ue, ok := err.(*UpstreamError); ok && ue.Code == http.StatusTooManyRequests {
			status = http.StatusTooManyRequests
		}
		writeError(w, status, "UPSTREAM_ERROR", err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(resp.Body)
	// claims (jti, session_key) available here for future audit logging.
	_ = claims
}

// handleRevoke is the internal endpoint the platform calls to revoke a
// session's token immediately (e.g. on task terminal/cancel). Loopback-only.
func (s *Server) handleRevoke(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "use POST")
		return
	}
	var req struct {
		Jti string `json:"jti"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "DECODE", err.Error())
		return
	}
	if req.Jti == "" {
		writeError(w, http.StatusBadRequest, "MISSING_JTI", "jti required")
		return
	}
	s.validator.Revoke(req.Jti)
	w.WriteHeader(http.StatusNoContent)
}

func extractBearer(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if !strings.HasPrefix(h, "Bearer ") {
		return ""
	}
	return strings.TrimPrefix(h, "Bearer ")
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": code, "message": message})
}
