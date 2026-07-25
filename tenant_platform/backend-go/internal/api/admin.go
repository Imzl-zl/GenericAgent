// Package api admin user management handlers (spec §5: users lifecycle).
package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/domain"
)

// registerLifecycleRoutes wires admin/binding/router routes when the
// corresponding services are present. Missing services skip registration
// so the foundation task-only API still works in tests that don't need them.
func (s *Server) registerLifecycleRoutes() {
	if s.users != nil {
		s.mux.HandleFunc("POST /v1/admin/users", s.auth(s.handleAdminCreateUser))
		s.mux.HandleFunc("POST /v1/admin/users/{user_id}/approve", s.auth(s.handleAdminApproveUser))
		s.mux.HandleFunc("POST /v1/admin/users/{user_id}/block", s.auth(s.handleAdminBlockUser))
		s.mux.HandleFunc("GET /v1/admin/users/pending", s.auth(s.handleAdminListPending))
	}
	if s.binding != nil {
		s.mux.HandleFunc("POST /v1/bindings", s.auth(s.handleCreateBinding))
		s.mux.HandleFunc("POST /v1/activate", s.auth(s.handleActivate))
	}
	if s.router != nil {
		s.mux.HandleFunc("POST /v1/router/messages", s.auth(s.handleRouterMessage))
		s.mux.HandleFunc("POST /v1/im/webhook", s.handleIMWebhook)
	}
	if s.policies != nil {
		s.mux.HandleFunc("GET /v1/admin/commands", s.auth(s.handleListCommands))
		s.mux.HandleFunc("PUT /v1/admin/commands/{command_id}", s.auth(s.handleUpdateCommand))
		s.mux.HandleFunc("GET /v1/admin/tool-policies", s.auth(s.handleListToolPolicies))
		s.mux.HandleFunc("POST /v1/admin/tool-policies", s.auth(s.handleCreateToolPolicy))
		s.mux.HandleFunc("PUT /v1/admin/users/{user_id}/tool-policy", s.auth(s.handleUpdateUserToolPolicy))
	}
	if s.bots != nil && s.cipher != nil {
		s.mux.HandleFunc("POST /v1/admin/bots", s.auth(s.handleAdminCreateBot))
	}
	if s.llmProviders != nil && s.cipher != nil {
		s.mux.HandleFunc("POST /v1/admin/llm-providers", s.auth(s.handleAdminCreateLLMProvider))
		s.mux.HandleFunc("GET /v1/admin/llm-providers", s.auth(s.handleAdminListLLMProviders))
		s.mux.HandleFunc("GET /v1/admin/llm-providers/{provider_id}", s.auth(s.handleAdminGetLLMProvider))
		s.mux.HandleFunc("PUT /v1/admin/llm-providers/{provider_id}", s.auth(s.handleAdminUpdateLLMProvider))
		s.mux.HandleFunc("DELETE /v1/admin/llm-providers/{provider_id}", s.auth(s.handleAdminDeleteLLMProvider))
		s.mux.HandleFunc("POST /v1/admin/llm-providers/{provider_id}/default", s.auth(s.handleAdminSetDefaultLLMProvider))
	}
}

type createUserBody struct {
	Username string `json:"username"`
}

func (s *Server) handleAdminCreateUser(w http.ResponseWriter, r *http.Request) {
	tid := traceID()
	var body createUserBody
	if err := decodeStrict(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "INVALID_JSON", err.Error(), tid)
		return
	}
	user, err := s.users.CreateUser(r.Context(), body.Username)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "USER_CREATE_FAILED", err.Error(), tid)
		return
	}
	writeJSON(w, http.StatusCreated, userReply(user))
}

func (s *Server) handleAdminApproveUser(w http.ResponseWriter, r *http.Request) {
	tid := traceID()
	uid, ok := parseUserID(w, r, tid)
	if !ok {
		return
	}
	user, err := s.users.ApproveUser(r.Context(), uid)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "USER_APPROVE_FAILED", err.Error(), tid)
		return
	}
	writeJSON(w, http.StatusOK, userReply(user))
}

func (s *Server) handleAdminBlockUser(w http.ResponseWriter, r *http.Request) {
	tid := traceID()
	uid, ok := parseUserID(w, r, tid)
	if !ok {
		return
	}
	user, err := s.users.BlockUser(r.Context(), uid)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "USER_BLOCK_FAILED", err.Error(), tid)
		return
	}
	writeJSON(w, http.StatusOK, userReply(user))
}

func (s *Server) handleAdminListPending(w http.ResponseWriter, r *http.Request) {
	tid := traceID()
	users, err := s.users.ListPendingUsers(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "LIST_PENDING_FAILED", err.Error(), tid)
		return
	}
	out := make([]map[string]any, 0, len(users))
	for _, u := range users {
		out = append(out, userReply(u))
	}
	writeJSON(w, http.StatusOK, map[string]any{"users": out})
}

type createBotBody struct {
	OwnerID int64  `json:"owner_id"`
	BotUUID string `json:"bot_uuid"`
	Token   string `json:"token"`
}

func (s *Server) handleAdminCreateBot(w http.ResponseWriter, r *http.Request) {
	tid := traceID()
	var body createBotBody
	if err := decodeStrict(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "INVALID_JSON", err.Error(), tid)
		return
	}
	if err := validateCreateBot(body); err != nil {
		writeErr(w, http.StatusBadRequest, "VALIDATION_ERROR", err.Error(), tid)
		return
	}
	cipherText, _, err := s.cipher.Encrypt([]byte(body.Token))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "ENCRYPT_FAILED", err.Error(), tid)
		return
	}
	bot, err := s.bots.CreateBot(r.Context(), body.BotUUID, body.OwnerID, cipherText)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "BOT_CREATE_FAILED", err.Error(), tid)
		return
	}
	writeJSON(w, http.StatusCreated, botReply(bot))
}

func validateCreateBot(b createBotBody) error {
	if b.OwnerID <= 0 {
		return fmt.Errorf("owner_id must be positive")
	}
	if strings.TrimSpace(b.BotUUID) == "" {
		return fmt.Errorf("bot_uuid is required")
	}
	if strings.TrimSpace(b.Token) == "" {
		return fmt.Errorf("token is required")
	}
	return nil
}

func botReply(b domain.Bot) map[string]any {
	return map[string]any{
		"bot_id":     b.ID,
		"bot_uuid":   b.BotUUID,
		"owner_id":   b.OwnerID,
		"state":      string(b.State),
		"created_at": b.CreatedAt.UTC().Format(time.RFC3339),
	}
}

// userReply serializes a domain.User into a JSON-friendly map.
func userReply(u domain.User) map[string]any {
	reply := map[string]any{
		"user_id":   u.ID,
		"username":  u.Username,
		"status":    string(u.Status),
		"created_at": u.CreatedAt.UTC().Format(time.RFC3339),
	}
	if u.ApprovedAt != nil {
		reply["approved_at"] = u.ApprovedAt.UTC().Format(time.RFC3339)
	}
	return reply
}

// decodeStrict decodes JSON with DisallowUnknownFields.
func decodeStrict(r *http.Request, v any) error {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}

// parseUserID extracts the {user_id} path value as int64.
func parseUserID(w http.ResponseWriter, r *http.Request, tid string) (int64, bool) {
	raw := r.PathValue("user_id")
	uid, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
	if err != nil || uid <= 0 {
		writeErr(w, http.StatusBadRequest, "INVALID_USER_ID", "user_id must be a positive integer", tid)
		return 0, false
	}
	return uid, true
}

// formatTimePtr formats a *time.Time as RFC3339 or empty string when nil.
func formatTimePtr(t *time.Time) string {
	if t == nil {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}
