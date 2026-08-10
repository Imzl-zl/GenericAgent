package application

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/domain"
	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/infrastructure/secret"
)

// ChannelConfigStore is the persistence port for channel config records.
type ChannelConfigStore interface {
	GetChannelConfigsByOwner(ctx context.Context, ownerID int64) ([]domain.ChannelConfig, error)
	GetChannelConfigByOwnerAndType(ctx context.Context, ownerID int64, channelType domain.ChannelType) (domain.ChannelConfig, error)
	UpsertChannelConfigCredentials(ctx context.Context, ownerID int64, channelType domain.ChannelType, configCiphertext []byte, configKeyVersion int) (domain.ChannelConfig, error)
	DisableChannelConfig(ctx context.Context, ownerID int64, channelType domain.ChannelType) error
}

// ChannelTransportStopper stops a channel's poller connection when its config
// changes or is unbound. nil = no poller wired (tests).
type ChannelTransportStopper interface {
	StopBot(ctx context.Context, botUUID string) error
}

// ChannelConfigService exposes channel binding management for the
// /v1/me/im-bindings API: list all channels, save/update credentials
// (feishu/dingtalk/qq), and unbind. WeChat binding stays on the QR flow.
type ChannelConfigService interface {
	ListBindings(ctx context.Context, ownerID int64) ([]domain.ChannelConfig, error)
	GetChannelConfig(ctx context.Context, ownerID int64, channelType domain.ChannelType) (domain.ChannelConfig, error)
	UpsertCredentials(ctx context.Context, ownerID int64, channelType domain.ChannelType, appID, appSecret string) (domain.ChannelConfig, error)
	Unbind(ctx context.Context, ownerID int64, channelType domain.ChannelType) (domain.ChannelConfig, error)
}

// ChannelConfigServiceConfig wires the service.
type ChannelConfigServiceConfig struct {
	Store  ChannelConfigStore
	Cipher secret.TokenCipher
	// Start starts (or restarts) a channel's poller connection after the
	// credentials changed. nil = no poller wired (tests).
	Start func(ctx context.Context, cfg domain.ChannelConfig) error
	// Stop stops a channel's poller connection when unbound. nil = no poller.
	Stop ChannelTransportStopper
}

type channelConfigService struct {
	store  ChannelConfigStore
	cipher secret.TokenCipher
	start  func(ctx context.Context, cfg domain.ChannelConfig) error
	stop   ChannelTransportStopper
}

// NewChannelConfigService validates config and returns the service.
func NewChannelConfigService(cfg ChannelConfigServiceConfig) (ChannelConfigService, error) {
	if cfg.Store == nil {
		return nil, errors.New("channel config store is required")
	}
	if cfg.Cipher == nil {
		return nil, errors.New("cipher is required")
	}
	return &channelConfigService{
		store:  cfg.Store,
		cipher: cfg.Cipher,
		start:  cfg.Start,
		stop:   cfg.Stop,
	}, nil
}

// ListBindings returns every channel config owned by the user.
func (s *channelConfigService) ListBindings(ctx context.Context, ownerID int64) ([]domain.ChannelConfig, error) {
	return s.store.GetChannelConfigsByOwner(ctx, ownerID)
}

// GetChannelConfig returns one channel config for (ownerID, channelType).
func (s *channelConfigService) GetChannelConfig(ctx context.Context, ownerID int64, channelType domain.ChannelType) (domain.ChannelConfig, error) {
	return s.store.GetChannelConfigByOwnerAndType(ctx, ownerID, channelType)
}

// UpsertCredentials encrypts {app_id, app_secret} into the config JSON and
// saves it for (ownerID, channelType). On success the poller connection is
// restarted with the new credentials (保存即生效).
func (s *channelConfigService) UpsertCredentials(ctx context.Context, ownerID int64, channelType domain.ChannelType, appID, appSecret string) (domain.ChannelConfig, error) {
	if ownerID <= 0 {
		return domain.ChannelConfig{}, errors.New("owner id must be positive")
	}
	if !domain.IsValidChannelType(string(channelType)) || channelType == domain.ChannelWechat {
		return domain.ChannelConfig{}, fmt.Errorf("channel type %q is not credential-configurable", channelType)
	}
	if err := validateChannelCredentials(appID, appSecret); err != nil {
		return domain.ChannelConfig{}, err
	}
	configJSON, err := json.Marshal(map[string]string{
		"app_id":     appID,
		"app_secret": appSecret,
	})
	if err != nil {
		return domain.ChannelConfig{}, fmt.Errorf("marshal channel config: %w", err)
	}
	ct, version, err := s.cipher.Encrypt(configJSON)
	if err != nil {
		return domain.ChannelConfig{}, fmt.Errorf("encrypt channel config: %w", err)
	}
	// upsert 前捕获旧配置(ON CONFLICT DO UPDATE 会覆盖 bot_uuid, 事后无法
	// 再取旧 UUID 停止其 poller 会话)。
	old, oldErr := s.store.GetChannelConfigByOwnerAndType(ctx, ownerID, channelType)
	cfg, err := s.store.UpsertChannelConfigCredentials(ctx, ownerID, channelType, ct, version)
	if err != nil {
		return domain.ChannelConfig{}, err
	}
	// 保存即生效: 停旧连接(换 UUID 时)后以新凭据启动。失败不阻断保存——
	// 配置已落库, poller 后续重启/热推会收敛。
	if s.stop != nil && oldErr == nil && old.BotUUID != "" && old.BotUUID != cfg.BotUUID {
		_ = s.stop.StopBot(ctx, old.BotUUID)
	}
	if s.start != nil {
		if err := s.start(ctx, cfg); err != nil {
			return cfg, fmt.Errorf("poller start: %w", err)
		}
	}
	return cfg, nil
}

// Unbind marks the channel config disabled and stops its poller connection.
func (s *channelConfigService) Unbind(ctx context.Context, ownerID int64, channelType domain.ChannelType) (domain.ChannelConfig, error) {
	if ownerID <= 0 {
		return domain.ChannelConfig{}, errors.New("owner id must be positive")
	}
	if !domain.IsValidChannelType(string(channelType)) || channelType == domain.ChannelWechat {
		return domain.ChannelConfig{}, fmt.Errorf("channel type %q cannot be unbound via this API", channelType)
	}
	cfg, err := s.store.GetChannelConfigByOwnerAndType(ctx, ownerID, channelType)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.ChannelConfig{}, domain.ErrChannelBindingNotFound
	}
	if err != nil {
		return domain.ChannelConfig{}, err
	}
	if err := s.store.DisableChannelConfig(ctx, ownerID, channelType); err != nil {
		return domain.ChannelConfig{}, err
	}
	// 解绑: 通知 poller 断开连接。
	if s.stop != nil {
		if err := s.stop.StopBot(ctx, cfg.BotUUID); err != nil {
			return cfg, fmt.Errorf("poller stop: %w", err)
		}
	}
	cfg.State = domain.ChannelDisabled
	return cfg, nil
}

// validateChannelCredentials enforces the app_id/app_secret shape before any
// persistence. Bounded length keeps ciphertext and JSON payloads small.
func validateChannelCredentials(appID, appSecret string) error {
	if appID == "" {
		return errors.New("app_id is required")
	}
	if appSecret == "" {
		return errors.New("app_secret is required")
	}
	if len(appID) > 128 || len(appSecret) > 256 {
		return errors.New("app_id/app_secret too long")
	}
	return nil
}
