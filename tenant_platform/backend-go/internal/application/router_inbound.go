package application

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/domain"
)

// nowFunc is overridable for tests.
var nowFunc = func() time.Time { return time.Now().UTC() }

// personalSessionKey returns the session key for a user's personal workspace.
func personalSessionKey(userID int64) string {
	return fmt.Sprintf("personal:%d", userID)
}

// persistInbound writes the received message to the message store and inserts
// a media_assets row for each media item. The first media path (if any) is
// persisted alongside the text body for quick access. Media asset insertions
// are best-effort: failures are logged but do not block message routing, and
// duplicates (ErrDuplicateMediaAsset) are silent successes (the cross-instance
// UNIQUE constraint already recorded the file).
func (r *router) persistInbound(ctx context.Context, msg IncomingMessage, bot domain.Bot, sessionKey string) (domain.Message, error) {
	mediaPath := ""
	if len(msg.MediaPaths) > 0 {
		mediaPath = msg.MediaPaths[0]
	}
	msgRow, err := r.messages.InsertInboundMessage(ctx, domain.Message{
		UserID:      bot.OwnerID,
		BotID:       bot.ID,
		SessionKey:  sessionKey,
		MessageID:   msg.MessageID,
		MessageType: inferMessageType(msg.MediaPaths),
		Content:     msg.Text,
		MediaPath:   mediaPath,
	})
	if err != nil {
		return msgRow, err
	}
	// Insert media_assets metadata. Idempotent (UNIQUE on message_id +
	// storage_path). Failure is non-fatal: the message is already persisted
	// and routed; a missing media audit row is acceptable.
	for _, item := range msg.MediaItems {
		_, merr := r.messages.InsertMediaAsset(ctx, domain.MediaAsset{
			UserID:      bot.OwnerID,
			BotID:       bot.ID,
			MessageID:   msgRow.ID,
			FileName:    item.FileName,
			StoragePath: item.StoragePath,
			ContentType: item.ContentType,
			SizeBytes:   item.Size,
			Direction:   domain.MessageInbound,
		})
		if merr != nil && !errors.Is(merr, domain.ErrDuplicateMediaAsset) {
			slog.ErrorContext(ctx, "router: persist media asset failed",
				"storage_path", item.StoragePath,
				"file_name", item.FileName,
				"error", merr)
		}
	}
	return msgRow, nil
}

// inferMessageType maps media presence to a coarse type. The Poller does not
// currently forward the iLink item type, so all media is labelled "file".
// Upgrading the webhook body to carry item type will enable image/voice/video.
func inferMessageType(mediaPaths []string) string {
	if len(mediaPaths) == 0 {
		return domain.MessageTypeText
	}
	return domain.MessageTypeFile
}
