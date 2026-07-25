package api

import (
	"net/http"
	"time"
)

// handleCreateWechatQRCode requests a new iLink QR code for the authenticated user.
func (s *Server) handleCreateWechatQRCode(w http.ResponseWriter, r *http.Request) {
	tid := traceID()
	userID, ok := userIDFromContext(r.Context())
	if !ok {
		writeErr(w, http.StatusUnauthorized, "UNAUTHORIZED", "missing user context", tid)
		return
	}

	sess, err := s.wechatBinding.GenerateQRCode(r.Context(), userID)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "ILINK_QR_FAILED", err.Error(), tid)
		return
	}

	writeJSON(w, http.StatusCreated, map[string]any{
		"qrcode_token":   sess.ILINKQRCode,
		"qrcode_url":     sess.QRCodeImgURL,
		"status":         string(sess.Status),
		"expires_at":     sess.ExpiresAt.UTC().Format(time.RFC3339),
	})
}

// handleGetWechatQRCodeStatus polls the iLink scan status and creates the bot on confirmation.
func (s *Server) handleGetWechatQRCodeStatus(w http.ResponseWriter, r *http.Request) {
	tid := traceID()
	qrCode := r.URL.Query().Get("qrcode_token")
	if qrCode == "" {
		writeErr(w, http.StatusBadRequest, "VALIDATION_ERROR", "qrcode_token is required", tid)
		return
	}

	sess, bot, err := s.wechatBinding.PollStatus(r.Context(), qrCode)
	if err != nil {
		// Non-confirmed statuses return the session without a bot.
		if sess.ID != "" {
			writeJSON(w, http.StatusOK, map[string]any{
				"qrcode_token": sess.ILINKQRCode,
				"status":       string(sess.Status),
				"expires_at":   sess.ExpiresAt.UTC().Format(time.RFC3339),
				"bound":        false,
			})
			return
		}
		writeErr(w, http.StatusBadGateway, "ILINK_STATUS_FAILED", err.Error(), tid)
		return
	}

	reply := map[string]any{
		"qrcode_token": sess.ILINKQRCode,
		"status":       string(sess.Status),
		"expires_at":   sess.ExpiresAt.UTC().Format(time.RFC3339),
		"bound":        bot.ID != 0,
	}
	if bot.ID != 0 {
		// Binding just confirmed: register the bot with the Bot Poller so it
		// begins long-polling iLink for inbound messages. The Poller's start
		// is idempotent, so repeated confirmed-status polls are safe.
		if s.botLifecycle != nil {
			if err := s.botLifecycle.StartBotForBoundUser(r.Context(), bot); err != nil {
				writeErr(w, http.StatusBadGateway, "BOT_START_FAILED", err.Error(), tid)
				return
			}
		}
		reply["bot"] = map[string]any{
			"bot_uuid":      bot.BotUUID,
			"ilink_bot_id":  bot.IlinkBotID,
			"ilink_user_id": bot.IlinkUserID,
			"baseurl":       bot.BaseURL,
			"state":         string(bot.State),
			"created_at":    bot.CreatedAt.UTC().Format(time.RFC3339),
		}
	}
	writeJSON(w, http.StatusOK, reply)
}
