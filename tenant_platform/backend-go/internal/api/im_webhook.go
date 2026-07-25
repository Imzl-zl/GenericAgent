package api

import (
	"errors"
	"net/http"
	"strings"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/application"
)

// imWebhookBody is the payload pushed by the Bot Poller to the platform.
// The Poller reuses GA Core's WxBotClient and forwards every inbound message
// here. Two payload variants exist:
//   - normal message: bot_uuid, ilink_user_id, message_id, text, updates_buf,
//     context_token
//   - auth-expired signal: bot_uuid, auth_expired=true
type imWebhookBody struct {
	BotUUID      string `json:"bot_uuid"`
	IlinkUserID  string `json:"ilink_user_id"`
	MessageID    string `json:"message_id"`
	Text         string `json:"text"`
	UpdatesBuf   string `json:"updates_buf"`
	ContextToken string `json:"context_token"`
	AuthExpired  bool   `json:"auth_expired"`
}

// handleIMWebhook receives inbound messages from the Bot Poller and routes them
// through the router pipeline. It is intentionally unauthenticated at the HTTP
// layer: identity and binding verification happen inside the router using
// bot_uuid + ilink_user_id (spec §6.1 step 2). A shared webhook secret can be
// added later for transport-level authentication if iLink supports it.
func (s *Server) handleIMWebhook(w http.ResponseWriter, r *http.Request) {
	tid := traceID()
	var body imWebhookBody
	if err := decodeStrict(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "INVALID_JSON", err.Error(), tid)
		return
	}
	if strings.TrimSpace(body.BotUUID) == "" {
		writeErr(w, http.StatusBadRequest, "VALIDATION_ERROR", "bot_uuid is required", tid)
		return
	}

	// Auth-expired signal: the Poller detected that the bot token is no longer
	// valid (iLink errcode -14). Mark the bot expired and stop routing.
	if body.AuthExpired {
		if s.botLifecycle != nil {
			if err := s.botLifecycle.HandleAuthExpired(r.Context(), body.BotUUID); err != nil {
				writeErr(w, http.StatusInternalServerError, "AUTH_EXPIRED_HANDLE", err.Error(), tid)
				return
			}
		}
		writeJSON(w, http.StatusOK, map[string]any{"action": "auth_expired"})
		return
	}

	if err := validateIMWebhookBody(body); err != nil {
		writeErr(w, http.StatusBadRequest, "VALIDATION_ERROR", err.Error(), tid)
		return
	}

	// Persist the cursor pushed by the Poller so the platform can resume
	// long-polling from the right position after a restart. The Go side owns
	// encryption; the Poller sends plaintext (it never touches disk).
	if s.botLifecycle != nil && body.UpdatesBuf != "" {
		if err := s.botLifecycle.PersistUpdatesBuf(r.Context(), body.BotUUID, body.UpdatesBuf); err != nil {
			// Cursor persistence failure is non-fatal: the message is still
			// routed. The next successful persist will catch up.
			_ = err
		}
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
	if strings.TrimSpace(body.IlinkUserID) == "" {
		return errors.New("ilink_user_id is required")
	}
	if strings.TrimSpace(body.MessageID) == "" {
		return errors.New("message_id is required")
	}
	return nil
}
