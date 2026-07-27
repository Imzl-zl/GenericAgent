package llmproxy

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/domain"
)

// MaxWorkerRequestBytes bounds the Worker request body read by the Proxy.
const MaxWorkerRequestBytes = 4 * 1024 * 1024

// handleChatCompletions validates the capability_token and forwards the body
// upstream. The active provider must be NativeOAI for this path.
func (s *Server) handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	s.handleProviderPath(w, r, domain.ProviderNativeOAI)
}

// handleMessages is the Anthropic Messages API proxy path. The active provider
// must be NativeClaude for this path.
func (s *Server) handleMessages(w http.ResponseWriter, r *http.Request) {
	s.handleProviderPath(w, r, domain.ProviderNativeClaude)
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
	claims, err := s.validator.Validate(r.Context(), token)
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
		// Log full error server-side; return generic message to client so
		// provider URL, API key fragments, or quota details don't leak.
		slog.ErrorContext(r.Context(), "llmproxy: upstream forward failed",
			"status", status,
			"error", err.Error(),
		)
		writeError(w, status, "UPSTREAM_ERROR", "upstream request failed")
		return
	}
	// Only forward 2xx upstream bodies to the client. Non-2xx bodies may
	// contain provider account info, quota usage, or internal error traces.
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		slog.WarnContext(r.Context(), "llmproxy: upstream non-2xx",
			"status", resp.StatusCode,
			"body_len", len(resp.Body),
		)
		writeError(w, http.StatusBadGateway, "UPSTREAM_ERROR", "upstream returned non-success status")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(resp.Body)
	// claims (jti, session_key) available here for future audit logging.
	_ = claims
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
