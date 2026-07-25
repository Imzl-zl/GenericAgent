// Package api binding + activation handlers (spec §5.1: binding flow).
package api

import (
	"net/http"
	"strings"
)

// handleCreateBinding generates a one-time binding code for the authenticated user.
// The plaintext code is returned ONCE; only its SHA-256 hash is persisted.
func (s *Server) handleCreateBinding(w http.ResponseWriter, r *http.Request) {
	tid := traceID()
	userID, ok := userIDFromContext(r.Context())
	if !ok {
		writeErr(w, http.StatusUnauthorized, "UNAUTHORIZED", "missing user context", tid)
		return
	}
	code, attempt, err := s.binding.GenerateBindingCode(r.Context(), userID)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "BINDING_GENERATE_FAILED", err.Error(), tid)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"code":        code,
		"binding_id":  attempt.ID,
		"user_id":     attempt.UserID,
		"state":       string(attempt.State),
		"expires_at":  attempt.ExpiresAt.UTC().Format("2006-01-02T15:04:05Z07:00"),
	})
}

type activateBody struct {
	Code         string `json:"code"`
	BotUUID      string `json:"bot_uuid"`
	IlinkUserID  string `json:"ilink_user_id"`
}

// handleActivate consumes a binding code and pairs a bot with an ilink_user_id.
// This is the HTTP equivalent of the /activate bot command (spec §6.2),
// exposed for testing and non-IM activation flows.
func (s *Server) handleActivate(w http.ResponseWriter, r *http.Request) {
	tid := traceID()
	var body activateBody
	if err := decodeStrict(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "INVALID_JSON", err.Error(), tid)
		return
	}
	if strings.TrimSpace(body.Code) == "" {
		writeErr(w, http.StatusBadRequest, "VALIDATION_ERROR", "code is required", tid)
		return
	}
	if strings.TrimSpace(body.BotUUID) == "" {
		writeErr(w, http.StatusBadRequest, "VALIDATION_ERROR", "bot_uuid is required", tid)
		return
	}
	if strings.TrimSpace(body.IlinkUserID) == "" {
		writeErr(w, http.StatusBadRequest, "VALIDATION_ERROR", "ilink_user_id is required", tid)
		return
	}
	attempt, err := s.binding.Activate(r.Context(), body.Code, body.BotUUID, body.IlinkUserID)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "ACTIVATE_FAILED", err.Error(), tid)
		return
	}
	reply := map[string]any{
		"binding_id": attempt.ID,
		"user_id":    attempt.UserID,
		"state":      string(attempt.State),
	}
	if attempt.ActivatedAt != nil {
		reply["activated_at"] = attempt.ActivatedAt.UTC().Format("2006-01-02T15:04:05Z07:00")
	}
	writeJSON(w, http.StatusOK, reply)
}
