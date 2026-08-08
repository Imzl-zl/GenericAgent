// Package api invite code and registration handlers.
package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/application"
)

type registerBody struct {
	Username   string `json:"username"`
	Password   string `json:"password"`
	InviteCode string `json:"invite_code"`
}

func (s *Server) handleRegister(w http.ResponseWriter, r *http.Request) {
	tid := traceID()
	var body registerBody
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "INVALID_JSON", err.Error(), tid)
		return
	}
	user, token, err := s.invite.RegisterWithInvite(r.Context(), body.Username, body.Password, body.InviteCode)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "REGISTER_FAILED", err.Error(), tid)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"user_id":  user.ID,
		"username": user.Username,
		"status":   string(user.Status),
		"token":    token,
	})
}

// handleGetMe returns the authenticated user's profile read fresh from the
// store. Clients use it to refresh the account status after admin approval
// (the status in the login/register response is a point-in-time snapshot).
func (s *Server) handleGetMe(w http.ResponseWriter, r *http.Request) {
	tid := traceID()
	userID, ok := userIDFromContext(r.Context())
	if !ok {
		writeErr(w, http.StatusUnauthorized, "UNAUTHORIZED", "missing user context", tid)
		return
	}
	id, username, status, err := s.users.GetUserByID(r.Context(), userID)
	if err != nil {
		writeErr(w, http.StatusNotFound, "NOT_FOUND", err.Error(), tid)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"user_id":  id,
		"username": username,
		"status":   string(status),
	})
}

type loginBody struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	tid := traceID()
	var body loginBody
	if err := decodeStrict(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "INVALID_JSON", err.Error(), tid)
		return
	}
	user, token, err := s.invite.Login(r.Context(), body.Username, body.Password)
	if err != nil {
		writeErr(w, http.StatusUnauthorized, "LOGIN_FAILED", err.Error(), tid)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"user_id":  user.ID,
		"username": user.Username,
		"status":   string(user.Status),
		"token":    token,
	})
}

func (s *Server) handleAdminCreateInviteCode(w http.ResponseWriter, r *http.Request) {
	tid := traceID()
	plaintext, ic, err := s.invite.GenerateInviteCode(r.Context(), s.adminUserID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "INVITE_GENERATE_FAILED", err.Error(), tid)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"code":       plaintext,
		"state":      string(ic.State),
		"expires_at": ic.ExpiresAt.UTC().Format(time.RFC3339),
		"created_at": ic.CreatedAt.UTC().Format(time.RFC3339),
	})
}

func (s *Server) handleAdminListInviteCodes(w http.ResponseWriter, r *http.Request) {
	tid := traceID()
	codes, err := s.invite.ListInviteCodes(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "INVITE_LIST_FAILED", err.Error(), tid)
		return
	}
	out := make([]map[string]any, 0, len(codes))
	for _, ic := range codes {
		item := map[string]any{
			"code":       ic.Code,
			"state":      string(ic.State),
			"created_by": ic.CreatedByUserID,
			"expires_at": ic.ExpiresAt.UTC().Format(time.RFC3339),
			"created_at": ic.CreatedAt.UTC().Format(time.RFC3339),
		}
		if ic.UsedByUserID != nil {
			item["used_by"] = *ic.UsedByUserID
		}
		if ic.UsedAt != nil {
			item["used_at"] = ic.UsedAt.UTC().Format(time.RFC3339)
		}
		out = append(out, item)
	}
	writeJSON(w, http.StatusOK, map[string]any{"invite_codes": out})
}

type deleteInviteCodesBody struct {
	Codes []string `json:"codes"`
}

func (s *Server) handleAdminDeleteInviteCodes(w http.ResponseWriter, r *http.Request) {
	tid := traceID()
	var body deleteInviteCodesBody
	if err := decodeStrict(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "INVALID_JSON", err.Error(), tid)
		return
	}
	deleted, err := s.invite.DeleteInviteCodes(r.Context(), body.Codes)
	if errors.Is(err, application.ErrInviteCodesRequired) {
		writeErr(w, http.StatusBadRequest, "INVITE_DELETE_FAILED", err.Error(), tid)
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "INVITE_DELETE_FAILED", "failed to delete invite codes", tid)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": deleted})
}

func (s *Server) handleAdminRevokeInviteCode(w http.ResponseWriter, r *http.Request) {
	tid := traceID()
	code := r.PathValue("code")
	if code == "" {
		writeErr(w, http.StatusBadRequest, "INVALID_CODE", "code is required", tid)
		return
	}
	if err := s.invite.RevokeInviteCode(r.Context(), code); err != nil {
		writeErr(w, http.StatusBadRequest, "INVITE_REVOKE_FAILED", err.Error(), tid)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"revoked": true})
}
