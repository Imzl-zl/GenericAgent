// Package api user-owned iLink bot binding handlers.
package api

import (
	"net/http"
)

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
