package application

import (
	"context"
	"fmt"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/domain"
)

// BotBindingStore is the persistence port for bot records.
type BotBindingStore interface {
	CreateBot(ctx context.Context, ilinkBotID string, ownerID int64, tokenCiphertext []byte) (domain.Bot, error)
	GetBotByOwner(ctx context.Context, ownerID int64) (domain.Bot, error)
	GetBotByIlinkBotID(ctx context.Context, ilinkBotID string) (domain.Bot, error)
}

// BotService exposes read-only access to the user's bound iLink bot.
// Bot creation happens exclusively through the WeChat QR code official flow
// (wechat_binding_service.CreateBotFromQRSession); manual BotID/Token input
// is prohibited by the iLink binding hard constraint.
type BotService interface {
	GetBotByOwner(ctx context.Context, ownerID int64) (domain.Bot, error)
}

type botService struct {
	store BotBindingStore
}

// NewBotService constructs the service.
func NewBotService(store BotBindingStore) (BotService, error) {
	if store == nil {
		return nil, fmt.Errorf("bot store is required")
	}
	return &botService{store: store}, nil
}

func (s *botService) GetBotByOwner(ctx context.Context, ownerID int64) (domain.Bot, error) {
	return s.store.GetBotByOwner(ctx, ownerID)
}
