package api

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/application"
	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/domain"
)

// imWebhookBody is the payload pushed by the Bot Poller to the platform.
// The Poller forwards every inbound channel message here. Two payload
// variants exist:
//   - normal message: bot_uuid, channel_type, channel_account_id,
//     conversation_id, message_id, text, updates_buf, context_token,
//     media_paths (absolute local paths for the worker),
//     media_items (metadata for media_assets persistence)
//   - auth-expired signal: bot_uuid, auth_expired=true (wechat iLink)
//
// 契约(IM_CHANNEL_BINDING §5): channel_type 标识渠道, conversation_id 是
// 对话单元分桶键(微信恒空), updates_buf/context_token 为微信专用。
type imWebhookBody struct {
	BotUUID          string `json:"bot_uuid"`
	ChannelType      string `json:"channel_type"`
	ChannelAccountID string `json:"channel_account_id"`
	ConversationID   string `json:"conversation_id"`
	// ConversationType 是对话单元类型('private'|'group'); 空回退 'private'。
	// IM 流式转发判定维度(IM_STREAMING_DELIVERY §4.4: 群聊只发最终结果)。
	ConversationType string         `json:"conversation_type"`
	MessageID        string         `json:"message_id"`
	Text             string         `json:"text"`
	UpdatesBuf       string         `json:"updates_buf"`
	ContextToken     string         `json:"context_token"`
	SourceMessageIDs []string       `json:"source_message_ids"`
	AuthExpired      bool           `json:"auth_expired"`
	MediaPaths       []string       `json:"media_paths"`
	MediaItems       []webhookMedia `json:"media_items"`
}

// webhookMedia is one media item's metadata forwarded by the Bot Poller.
// storage_path is RELATIVE to the poller's --media-dir so the DB row is
// portable across mount points (local disk -> NFS -> S3 mount).
type webhookMedia struct {
	FileName    string `json:"file_name"`
	StoragePath string `json:"storage_path"`
	ContentType string `json:"content_type"`
	Size        int64  `json:"size"`
}

// convertWebhookMedia maps the wire-format media items to the application
// layer's IncomingMediaItem. The split keeps the API struct free of
// application-layer imports.
func convertWebhookMedia(items []webhookMedia) []application.IncomingMediaItem {
	if len(items) == 0 {
		return nil
	}
	out := make([]application.IncomingMediaItem, 0, len(items))
	for _, it := range items {
		out = append(out, application.IncomingMediaItem{
			FileName:    it.FileName,
			StoragePath: it.StoragePath,
			ContentType: it.ContentType,
			Size:        it.Size,
		})
	}
	return out
}

// webhookSignatureHeader is the HTTP header carrying the hex-encoded
// HMAC-SHA256(secret, body) computed by the Bot Poller.
const webhookSignatureHeader = "X-Webhook-Signature"

// handleIMWebhook receives inbound messages from the Bot Poller and routes
// them through the router pipeline. The request must carry
// X-Webhook-Signature = hex(HMAC-SHA256(secret, body)); this prevents
// unauthenticated callers from injecting fake inbound messages. When
// webhookSecret is empty, every request is rejected (fail-closed). Configure
// --webhook-secret (even a dummy value for dev/test) to allow inbound.
func (s *Server) handleIMWebhook(w http.ResponseWriter, r *http.Request) {
	tid := traceID()
	bodyBytes := readBody(r)
	if bodyBytes == nil {
		writeErr(w, http.StatusBadRequest, "READ_BODY", "failed to read request body", tid)
		return
	}
	if !s.verifyWebhookSignature(bodyBytes, r.Header.Get(webhookSignatureHeader)) {
		writeErr(w, http.StatusUnauthorized, "INVALID_SIGNATURE", "webhook signature mismatch", tid)
		return
	}
	var body imWebhookBody
	if err := decodeStrictBytes(bodyBytes, &body); err != nil {
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

	// round9 审查: cursor 持久化从"路由前"移到"路由成功后"。旧顺序在
	// cursor 已提交但消息尚未路由时崩溃, 会永久跳过该消息(丢消息);
	// 新顺序中任何中间崩溃都由重试协议收敛: 任务/消息行唯一键保证路由
	// 幂等, 路由成功后再提交 cursor, 2xx 语义 = 消息已处理且 cursor 已确认。
	result, err := s.router.HandleMessage(r.Context(), application.IncomingMessage{
		BotUUID:          body.BotUUID,
		ChannelType:      body.ChannelType,
		ChannelAccountID: body.ChannelAccountID,
		ConversationID:   body.ConversationID,
		ConversationType: domain.NormalizeConversationType(body.ConversationType),
		MessageID:        body.MessageID,
		Text:             body.Text,
		MediaPaths:       body.MediaPaths,
		MediaItems:       convertWebhookMedia(body.MediaItems),
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "ROUTER_ERROR", err.Error(), tid)
		return
	}
	// 路由成功后才持久化 cursor。失败返回 5xx: Poller 重试时路由幂等
	// (消息行/任务唯一键短路), 重试会再次尝试持久化 cursor, 不丢不重。
	if s.botLifecycle != nil && body.UpdatesBuf != "" {
		if err := s.botLifecycle.PersistUpdatesBuf(r.Context(), body.BotUUID, body.UpdatesBuf); err != nil {
			slog.ErrorContext(r.Context(), "im_webhook: cursor persist failed; returning 5xx so Poller retries",
				"bot_uuid", body.BotUUID,
				"trace_id", tid,
				"error", err)
			writeErr(w, http.StatusServiceUnavailable, "CURSOR_PERSIST_FAILED",
				"failed to persist updates cursor; message routed idempotently, retry will converge", tid)
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"action":  string(result.Action),
		"reply":   result.Reply,
		"user_id": result.UserID,
	})
}

// verifyWebhookSignature returns true when the request signature matches
// HMAC-SHA256(secret, body). Empty secret is fail-closed: every request is
// rejected. Configure --webhook-secret (even a dummy value for dev/test) to
// allow inbound webhooks.
func (s *Server) verifyWebhookSignature(body []byte, sigHex string) bool {
	if s.webhookSecret == "" {
		return false
	}
	if sigHex == "" {
		return false
	}
	want, err := hex.DecodeString(sigHex)
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, []byte(s.webhookSecret))
	mac.Write(body)
	got := mac.Sum(nil)
	return hmac.Equal(got, want)
}

func validateIMWebhookBody(body imWebhookBody) error {
	if strings.TrimSpace(body.ChannelType) == "" {
		return errors.New("channel_type is required")
	}
	if strings.TrimSpace(body.ChannelAccountID) == "" {
		return errors.New("channel_account_id is required")
	}
	if strings.TrimSpace(body.MessageID) == "" {
		return errors.New("message_id is required")
	}
	return nil
}
