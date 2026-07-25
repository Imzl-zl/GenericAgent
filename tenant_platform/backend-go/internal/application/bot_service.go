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

// BotService manages user-owned iLink bots.
type BotService interface {
	BindOwnBot(ctx context.Context, ownerID int64, ilinkBotID string, tokenCiphertext []byte) (domain.Bot, error)
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

func (s *botService) BindOwnBot(ctx context.Context, ownerID int64, ilinkBotID string, tokenCiphertext []byte) (domain.Bot, error) {
	if ownerID <= 0 {
		return domain.Bot{}, fmt.Errorf("owner id must be positive")
	}
	if ilinkBotID == "" {
		return domain.Bot{}, fmt.Errorf("ilink bot id is required")
	}
	if len(tokenCiphertext) == 0 {
		return domain.Bot{}, fmt.Errorf("token ciphertext is required")
	}
	return s.store.CreateBot(ctx, ilinkBotID, ownerID, tokenCiphertext)
}

func (s *botService) GetBotByOwner(ctx context.Context, ownerID int64) (domain.Bot, error) {
	return s.store.GetBotByOwner(ctx, ownerID)
}
