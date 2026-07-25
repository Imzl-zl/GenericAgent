// Package api router message ingestion handler (spec §6.1–§6.2).
package api

import (
	"net/http"
	"strings"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/application"
)

type routerMessageBody struct {
	BotUUID     string `json:"bot_uuid"`
	IlinkUserID string `json:"ilink_user_id"`
	MessageID   string `json:"message_id"`
	Text        string `json:"text"`
}

// handleRouterMessage ingests one bot message and dispatches it through the
// router pipeline (identity → status → command/task). Replies are sent via
// the configured BotTransportAdapter; the HTTP response carries the router's
// classification (action) for observability.
func (s *Server) handleRouterMessage(w http.ResponseWriter, r *http.Request) {
	tid := traceID()
	var body routerMessageBody
	if err := decodeStrict(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "INVALID_JSON", err.Error(), tid)
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
	if strings.TrimSpace(body.MessageID) == "" {
		writeErr(w, http.StatusBadRequest, "VALIDATION_ERROR", "message_id is required", tid)
		return
	}
	result, err := s.router.HandleMessage(r.Context(), application.IncomingMessage{
		BotUUID:     body.BotUUID,
		IlinkUserID: body.IlinkUserID,
		MessageID:   body.MessageID,
		Text:        body.Text,
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "ROUTER_ERROR", err.Error(), tid)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"action":  string(result.Action),
		"reply":   result.Reply,
		"user_id": result.UserID,
	})
}
