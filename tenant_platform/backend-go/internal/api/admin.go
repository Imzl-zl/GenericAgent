// Package api admin user management handlers (spec §5: users lifecycle).
package api

import (
	"encoding/json"
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
	}
	if s.policies != nil {
		s.mux.HandleFunc("GET /v1/admin/commands", s.auth(s.handleListCommands))
		s.mux.HandleFunc("PUT /v1/admin/commands/{command_id}", s.auth(s.handleUpdateCommand))
		s.mux.HandleFunc("GET /v1/admin/tool-policies", s.auth(s.handleListToolPolicies))
		s.mux.HandleFunc("POST /v1/admin/tool-policies", s.auth(s.handleCreateToolPolicy))
		s.mux.HandleFunc("PUT /v1/admin/users/{user_id}/tool-policy", s.auth(s.handleUpdateUserToolPolicy))
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
