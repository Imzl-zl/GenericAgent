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
)

// imWebhookBody is the payload pushed by the Bot Poller to the platform.
// The Poller reuses GA Core's WxBotClient and forwards every inbound message
// here. Two payload variants exist:
//   - normal message: bot_uuid, ilink_user_id, message_id, text, updates_buf,
//     context_token, media_paths (absolute local paths for the worker),
//     media_items (metadata for media_assets persistence)
//   - auth-expired signal: bot_uuid, auth_expired=true
type imWebhookBody struct {
	BotUUID      string         `json:"bot_uuid"`
	IlinkUserID  string         `json:"ilink_user_id"`
	MessageID    string         `json:"message_id"`
	Text         string         `json:"text"`
	UpdatesBuf   string         `json:"updates_buf"`
	ContextToken string         `json:"context_token"`
	AuthExpired  bool           `json:"auth_expired"`
	MediaPaths   []string       `json:"media_paths"`
	MediaItems   []webhookMedia `json:"media_items"`
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

	// Persist the cursor pushed by the Poller BEFORE routing the message.
	// This is a hard sync: if the cursor can't be durably stored, we must not
	// route the message — because a platform restart after routing would
	// resume polling from the OLD cursor and re-deliver this same message,
	// breaking exactly-once inbound delivery. Surfacing 5xx here lets the
	// Poller retry the whole webhook; UpsertBotTransportState is idempotent
	// (ON CONFLICT DO UPDATE) so a retry that re-sends the same cursor is safe.
	if s.botLifecycle != nil && body.UpdatesBuf != "" {
		if err := s.botLifecycle.PersistUpdatesBuf(r.Context(), body.BotUUID, body.UpdatesBuf); err != nil {
			slog.ErrorContext(r.Context(), "im_webhook: cursor persist failed; returning 5xx so Poller retries",
				"bot_uuid", body.BotUUID,
				"trace_id", tid,
				"error", err)
			writeErr(w, http.StatusServiceUnavailable, "CURSOR_PERSIST_FAILED",
				"failed to persist updates cursor; message not routed to avoid duplicate delivery on restart", tid)
			return
		}
	}

	result, err := s.router.HandleMessage(r.Context(), application.IncomingMessage{
		BotUUID:     body.BotUUID,
		IlinkUserID: body.IlinkUserID,
		MessageID:   body.MessageID,
		Text:        body.Text,
		MediaPaths:  body.MediaPaths,
		MediaItems:  convertWebhookMedia(body.MediaItems),
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
	if strings.TrimSpace(body.IlinkUserID) == "" {
		return errors.New("ilink_user_id is required")
	}
	if strings.TrimSpace(body.MessageID) == "" {
		return errors.New("message_id is required")
	}
	return nil
}
