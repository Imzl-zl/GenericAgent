package transport

import (
	"context"
	"errors"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/poller"
)

// ILinkAdapterConfig wires the iLink transport adapter.
//
// The adapter delegates all iLink protocol I/O (getupdates, sendmessage) to
// the Python Bot Poller, which reuses GA Core's verified WxBotClient. The Go
// platform never re-implements the iLink protocol directly.
type ILinkAdapterConfig struct {
	Poller *poller.Client
}

// ILinkAdapter is the production BotTransportAdapter for iLink.
// It forwards SendMessage to the Bot Poller and keeps an in-memory idempotency
// cache sharded by botUUID|messageID. The cache is TTL-bounded so long-running
// processes don't leak memory. For multi-instance deployments the idempotency
// store must be externalized (spec §6.1); the DB UNIQUE(bot_id, message_id)
// index is the cross-instance backstop.
type ILinkAdapter struct {
	poller      *poller.Client
	idempotency *idempotencyCache
}

// NewILinkAdapter validates config and returns a production adapter.
func NewILinkAdapter(cfg ILinkAdapterConfig) (*ILinkAdapter, error) {
	if cfg.Poller == nil {
		return nil, errors.New("Poller is required")
	}
	return &ILinkAdapter{
		poller:      cfg.Poller,
		idempotency: newIdempotencyCache(),
	}, nil
}

// SendMessage delivers a text reply via the Bot Poller (iLink sendmessage).
func (a *ILinkAdapter) SendMessage(ctx context.Context, botUUID, ilinkUserID, text string) error {
	if botUUID == "" || ilinkUserID == "" || text == "" {
		return errors.New("bot uuid, ilink user id, and text are required")
	}
	return a.poller.SendMessage(ctx, poller.SendMessageRequest{
		BotUUID:     botUUID,
		ILinkUserID: ilinkUserID,
		Text:        text,
	})
}

// RecordMessageIdempotency returns true the first time a message is seen
// within the TTL window. Sharded by key hash so concurrent bots don't contend
// on a single mutex.
func (a *ILinkAdapter) RecordMessageIdempotency(_ context.Context, botUUID, messageID string) (bool, error) {
	if botUUID == "" || messageID == "" {
		return false, errors.New("bot uuid and message id are required")
	}
	return a.idempotency.Record(botUUID, messageID), nil
}
