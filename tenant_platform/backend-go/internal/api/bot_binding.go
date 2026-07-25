// Package api user-owned iLink bot binding handlers.
package api

import (
	"fmt"
	"net/http"
	"strings"
)

type bindOwnBotBody struct {
	IlinkBotID string `json:"ilink_bot_id"`
	Token      string `json:"token"`
}

func (s *Server) handleBindOwnBot(w http.ResponseWriter, r *http.Request) {
	tid := traceID()
	userID, ok := userIDFromContext(r.Context())
	if !ok {
		writeErr(w, http.StatusUnauthorized, "UNAUTHORIZED", "missing user context", tid)
		return
	}
	var body bindOwnBotBody
	if err := decodeStrict(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "INVALID_JSON", err.Error(), tid)
		return
	}
	if err := validateBindOwnBot(body); err != nil {
		writeErr(w, http.StatusBadRequest, "VALIDATION_ERROR", err.Error(), tid)
		return
	}
	cipherText, _, err := s.cipher.Encrypt([]byte(body.Token))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "ENCRYPT_FAILED", err.Error(), tid)
		return
	}
	bot, err := s.botSvc.BindOwnBot(r.Context(), userID, body.IlinkBotID, cipherText)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "BOT_BIND_FAILED", err.Error(), tid)
		return
	}
	writeJSON(w, http.StatusCreated, botReply(bot))
}

func (s *Server) handleGetOwnBot(w http.ResponseWriter, r *http.Request) {
	tid := traceID()
	userID, ok := userIDFromContext(r.Context())
	if !ok {
		writeErr(w, http.StatusUnauthorized, "UNAUTHORIZED", "missing user context", tid)
		return
	}
	bot, err := s.botSvc.GetBotByOwner(r.Context(), userID)
	if err != nil {
		writeErr(w, http.StatusNotFound, "NOT_FOUND", err.Error(), tid)
		return
	}
	writeJSON(w, http.StatusOK, botReply(bot))
}

func validateBindOwnBot(b bindOwnBotBody) error {
	if strings.TrimSpace(b.IlinkBotID) == "" {
		return fmt.Errorf("ilink_bot_id is required")
	}
	if strings.TrimSpace(b.Token) == "" {
		return fmt.Errorf("token is required")
	}
	return nil
}
