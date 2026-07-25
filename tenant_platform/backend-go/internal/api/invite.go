// Package api invite code and registration handlers.
package api

import (
	"encoding/json"
	"net/http"
	"time"
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
		"user_id": user.ID,
		"username": user.Username,
		"status":   string(user.Status),
		"token":    token,
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
	plaintext, ic, err := s.invite.GenerateInviteCode(r.Context(), s.devUserID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "INVITE_GENERATE_FAILED", err.Error(), tid)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"code":      plaintext,
		"state":     string(ic.State),
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
