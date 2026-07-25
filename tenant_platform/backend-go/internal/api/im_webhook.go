package api

import (
	"errors"
	"net/http"
	"strings"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/application"
)

// imWebhookBody is the message payload pushed by iLink to the platform.
type imWebhookBody struct {
	BotUUID     string `json:"bot_uuid"`
	IlinkUserID string `json:"ilink_user_id"`
	MessageID   string `json:"message_id"`
	Text        string `json:"text"`
}

// handleIMWebhook receives inbound messages from iLink and routes them through
// the router pipeline. It is intentionally unauthenticated at the HTTP layer:
// identity and binding verification happen inside the router using bot_uuid +
// ilink_user_id (spec §6.1 step 2). A shared webhook secret can be added later
// for transport-level authentication if iLink supports it.
func (s *Server) handleIMWebhook(w http.ResponseWriter, r *http.Request) {
	tid := traceID()
	var body imWebhookBody
	if err := decodeStrict(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "INVALID_JSON", err.Error(), tid)
		return
	}
	if err := validateIMWebhookBody(body); err != nil {
		writeErr(w, http.StatusBadRequest, "VALIDATION_ERROR", err.Error(), tid)
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

func validateIMWebhookBody(body imWebhookBody) error {
	if strings.TrimSpace(body.BotUUID) == "" {
		return errors.New("bot_uuid is required")
	}
	if strings.TrimSpace(body.IlinkUserID) == "" {
		return errors.New("ilink_user_id is required")
	}
	if strings.TrimSpace(body.MessageID) == "" {
		return errors.New("message_id is required")
	}
	return nil
}
