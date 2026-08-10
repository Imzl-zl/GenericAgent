// Package api router message ingestion handler (spec §6.1–§6.2).
package api

import (
	"net/http"
	"strings"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/application"
	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/domain"
)

// routerMessageBody is the service-to-service ingestion contract
// (IM_CHANNEL_BINDING §5): bot_uuid identifies the channel config instance,
// channel_type selects the channel, channel_account_id is the sender account
// (微信=ilink_user_id), conversation_id is the conversation unit bucket key
// (群 ID / 对端 ID; 微信恒空).
type routerMessageBody struct {
	BotUUID          string `json:"bot_uuid"`
	ChannelType      string `json:"channel_type"`
	ChannelAccountID string `json:"channel_account_id"`
	ConversationID   string `json:"conversation_id"`
	// ConversationType 是对话单元类型('private'|'group'); 空回退 'private'。
	// IM 流式转发判定维度(IM_STREAMING_DELIVERY §4.4: 群聊只发最终结果)。
	ConversationType string `json:"conversation_type"`
	MessageID        string `json:"message_id"`
	Text             string `json:"text"`
}

// handleRouterMessage ingests one channel message and dispatches it through
// the router pipeline (identity → status → command/task). Replies are sent via
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
	if strings.TrimSpace(body.ChannelType) == "" {
		writeErr(w, http.StatusBadRequest, "VALIDATION_ERROR", "channel_type is required", tid)
		return
	}
	if strings.TrimSpace(body.ChannelAccountID) == "" {
		writeErr(w, http.StatusBadRequest, "VALIDATION_ERROR", "channel_account_id is required", tid)
		return
	}
	if strings.TrimSpace(body.MessageID) == "" {
		writeErr(w, http.StatusBadRequest, "VALIDATION_ERROR", "message_id is required", tid)
		return
	}
	result, err := s.router.HandleMessage(r.Context(), application.IncomingMessage{
		BotUUID:          body.BotUUID,
		ChannelType:      body.ChannelType,
		ChannelAccountID: body.ChannelAccountID,
		ConversationID:   body.ConversationID,
		ConversationType: domain.NormalizeConversationType(body.ConversationType),
		MessageID:        body.MessageID,
		Text:             body.Text,
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
