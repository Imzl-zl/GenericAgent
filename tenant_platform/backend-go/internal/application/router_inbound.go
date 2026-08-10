package application

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/domain"
)

// nowFunc is overridable for tests.
var nowFunc = func() time.Time { return time.Now().UTC() }

// persistInboundMedia 为已持久化的入站消息行补插 media_assets 审计
// (round10 审查 B7): 消息行本身已在路由时持久化(任务同事务 / 命令-relay
// claim), 此处只插入媒体资产元数据。插入幂等(UNIQUE on message_id +
// storage_path); 失败非致命——消息已持久化并路由, 缺失媒体审计行可接受。
func (r *router) persistInboundMedia(ctx context.Context, msg IncomingMessage, bot domain.ChannelConfig, msgRow domain.Message) error {
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
	return nil
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
