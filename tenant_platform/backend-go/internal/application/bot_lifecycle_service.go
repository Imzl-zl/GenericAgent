package application

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5"
	"golang.org/x/sync/errgroup"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/domain"
	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/infrastructure/poller"
	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/infrastructure/secret"
)

// BotLifecycleStore is the persistence port for bot lifecycle orchestration.
// The Go platform owns encryption + persistence; the Python Poller owns the
// iLink protocol I/O. This store bridges them.
type BotLifecycleStore interface {
	GetChannelConfigByUUID(ctx context.Context, botUUID string) (domain.ChannelConfig, error)
	GetBotTransportState(ctx context.Context, botID int64) (domain.BotTransportState, error)
	UpsertBotTransportState(ctx context.Context, botID int64, cursorCiphertext []byte, cursorKeyVersion int, reconnectState, lastErrorCode string) error
	ListActiveChannelConfigs(ctx context.Context) ([]domain.ChannelConfig, error)
	UpdateChannelConfigState(ctx context.Context, botUUID string, state domain.ChannelConfigState) error
}

// BotLifecycleService orchestrates bot start/stop/restore against the Bot
// Poller. It decrypts bot tokens and cursors before handing them to the
// Poller, and re-encrypts/persists cursors returned by the Poller.
type BotLifecycleService interface {
	StartChannelConfig(ctx context.Context, cfg domain.ChannelConfig) error
	StopBot(ctx context.Context, botUUID string) error
	RestoreActiveBots(ctx context.Context) error
	PersistUpdatesBuf(ctx context.Context, botUUID, plaintextBuf string) error
	HandleAuthExpired(ctx context.Context, botUUID string) error
}

// BotLifecycleConfig wires the lifecycle service.
type BotLifecycleConfig struct {
	Store      BotLifecycleStore
	Cipher     secret.TokenCipher
	Poller     *poller.Client
	WebhookURL string // platform's /v1/im/webhook endpoint, told to the Poller
	// RestoreConcurrency caps parallel bot restoration on startup. 0 = 1 (serial).
	// Recommended: 4 for 10-20 bots.
	RestoreConcurrency int
}

type botLifecycleService struct {
	store              BotLifecycleStore
	cipher             secret.TokenCipher
	poller             *poller.Client
	webhookURL         string
	restoreConcurrency int
}

// NewBotLifecycleService validates config and returns a lifecycle service.
func NewBotLifecycleService(cfg BotLifecycleConfig) (BotLifecycleService, error) {
	if cfg.Store == nil {
		return nil, errors.New("store is required")
	}
	if cfg.Cipher == nil {
		return nil, errors.New("cipher is required")
	}
	if cfg.Poller == nil {
		return nil, errors.New("poller client is required")
	}
	if cfg.WebhookURL == "" {
		return nil, errors.New("webhook url is required")
	}
	if cfg.RestoreConcurrency <= 0 {
		cfg.RestoreConcurrency = 1
	}
	return &botLifecycleService{
		store:              cfg.Store,
		cipher:             cfg.Cipher,
		poller:             cfg.Poller,
		webhookURL:         cfg.WebhookURL,
		restoreConcurrency: cfg.RestoreConcurrency,
	}, nil
}

// StartChannelConfig decrypts the channel config JSON and any persisted
// cursor, then tells the Poller to begin polling for this channel. Safe to
// call on a fresh config (no cursor yet) or after a platform restart
// (cursor resumes from DB for wechat).
func (s *botLifecycleService) StartChannelConfig(ctx context.Context, cfg domain.ChannelConfig) error {
	if !cfg.IsBound() {
		return fmt.Errorf("channel config %s is not bound", cfg.BotUUID)
	}
	configJSON, err := s.cipher.Decrypt(cfg.ConfigCiphertext, cfg.ConfigKeyVersion)
	if err != nil {
		return fmt.Errorf("decrypt channel config: %w", err)
	}
	cursor, err := s.resolveCursor(ctx, cfg.ID)
	if err != nil {
		return err
	}
	return s.poller.StartBot(ctx, poller.StartBotRequest{
		BotUUID:     cfg.BotUUID,
		ChannelType: string(cfg.ChannelType),
		ConfigJSON:  configJSON,
		BaseURL:     cfg.BaseURL,
		UpdatesBuf:  cursor,
		WebhookURL:  s.webhookURL,
	})
}

// resolveCursor decrypts the persisted update cursor for a bot using the
// stored key version. Returns empty string when no cursor exists yet.
func (s *botLifecycleService) resolveCursor(ctx context.Context, botID int64) (string, error) {
	st, err := s.store.GetBotTransportState(ctx, botID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("get transport state: %w", err)
	}
	if len(st.UpdateCursorCiphertext) == 0 {
		return "", nil
	}
	if st.UpdateCursorKeyVersion <= 0 {
		// Defensive: pre-0012 rows default to 1 via the migration.
		st.UpdateCursorKeyVersion = 1
	}
	plain, err := s.cipher.Decrypt(st.UpdateCursorCiphertext, st.UpdateCursorKeyVersion)
	if err != nil {
		return "", fmt.Errorf("decrypt cursor: %w", err)
	}
	return string(plain), nil
}

// StopBot tells the Poller to stop long-polling, then encrypts and persists the
// final cursor returned by the Poller.
// 行不存在(pgx.ErrNoRows)时仍必须转发给 Poller: 重新绑定已用新 bot_uuid 覆盖
// bots 行(ON CONFLICT DO UPDATE), 旧 UUID 从 DB 消失但 Poller 侧旧会话仍在
// 运行——不停止则旧会话继续推送 webhook(unknown bot / 双回复)。无行可持久化
// cursor, 直接忽略返回值。
func (s *botLifecycleService) StopBot(ctx context.Context, botUUID string) error {
	bot, err := s.store.GetChannelConfigByUUID(ctx, botUUID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			if _, stopErr := s.poller.StopBot(ctx, botUUID); stopErr != nil {
				return fmt.Errorf("poller stop: %w", stopErr)
			}
			slog.WarnContext(ctx, "bot_lifecycle: stopped poller session for removed bot row",
				"bot_uuid", botUUID)
			return nil
		}
		return fmt.Errorf("resolve bot: %w", err)
	}
	resp, err := s.poller.StopBot(ctx, botUUID)
	if err != nil {
		return fmt.Errorf("poller stop: %w", err)
	}
	return s.persistCursor(ctx, bot.ID, resp.UpdatesBuf, "stopped", "")
}

// RestoreActiveBots re-registers every active bound bot with the Poller after a
// platform restart. Called once during startup. Failures are logged, not fatal,
// so one bad bot does not block the rest. Bots are restored in parallel up to
// RestoreConcurrency to avoid 10-30s serial startup on 10-20 bots.
func (s *botLifecycleService) RestoreActiveBots(ctx context.Context) error {
	cfgs, err := s.store.ListActiveChannelConfigs(ctx)
	if err != nil {
		return fmt.Errorf("list active channels: %w", err)
	}
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(s.restoreConcurrency)
	for _, cfg := range cfgs {
		cfg := cfg // capture
		g.Go(func() error {
			if err := s.StartChannelConfig(gctx, cfg); err != nil {
				slog.ErrorContext(gctx, "bot_lifecycle: restore channel failed",
					"bot_uuid", cfg.BotUUID,
					"bot_id", cfg.ID,
					"channel_type", cfg.ChannelType,
					"owner_user_id", cfg.OwnerID,
					"error", err)
				return nil // do not cancel the group; one bad bot must not block others
			}
			slog.InfoContext(gctx, "bot_lifecycle: restored channel",
				"bot_uuid", cfg.BotUUID,
				"bot_id", cfg.ID,
				"channel_type", cfg.ChannelType,
				"owner_user_id", cfg.OwnerID)
			return nil
		})
	}
	return g.Wait()
}

// PersistUpdatesBuf encrypts and stores the plaintext cursor pushed by the
// Poller alongside an inbound message. Called by the IM webhook handler.
// Empty buffer is a no-op (Poller has not advanced its cursor).
// bot 行不存在(pgx.ErrNoRows)时返回 nil 而不是错误: 消息已被路由层永久拒绝
// (unknown bot 僵尸会话), cursor 无处可存——若返回错误, webhook 回 503,
// Poller 按契约每 60s 重试同一条消息, 用户每 60s 收到一条拒绝回复(无限循环)。
func (s *botLifecycleService) PersistUpdatesBuf(ctx context.Context, botUUID, plaintextBuf string) error {
	if plaintextBuf == "" {
		return nil
	}
	cfg, err := s.store.GetChannelConfigByUUID(ctx, botUUID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			slog.WarnContext(ctx, "bot_lifecycle: skip cursor persist for unknown bot",
				"bot_uuid", botUUID)
			return nil
		}
		return fmt.Errorf("resolve bot: %w", err)
	}
	return s.persistCursor(ctx, cfg.ID, plaintextBuf, "polling", "")
}

// HandleAuthExpired marks the bot as expired and records the error in the
// transport state. The Poller has already stopped its loop for this bot.
func (s *botLifecycleService) HandleAuthExpired(ctx context.Context, botUUID string) error {
	cfg, err := s.store.GetChannelConfigByUUID(ctx, botUUID)
	if err != nil {
		return fmt.Errorf("resolve bot: %w", err)
	}
	if err := s.store.UpdateChannelConfigState(ctx, botUUID, domain.ChannelExpired); err != nil {
		return fmt.Errorf("mark bot expired: %w", err)
	}
	// Pass keyVersion=1 (the default) instead of 0 to avoid violating the NOT NULL
	// constraint when this is the first transport_state insert for this bot.
	// cursorCiphertext=nil means "keep existing cursor", which works for both
	// INSERT (no cursor yet) and UPDATE (preserve last known cursor).
	return s.store.UpsertBotTransportState(ctx, cfg.ID, nil, 1, "error", "AUTH_EXPIRED")
}

// persistCursor encrypts the plaintext cursor and upserts the transport state.
// The AES key version returned by Encrypt is stored alongside the ciphertext so
// future key rotation can still decrypt old cursors.
func (s *botLifecycleService) persistCursor(ctx context.Context, botID int64, plaintextBuf, reconnectState, errorCode string) error {
	if plaintextBuf == "" {
		// No cursor to persist; just update reconnect_state/error fields.
		// Pass keyVersion=1 (the default) instead of 0 to avoid violating NOT NULL
		// constraint on first insert. cursorCiphertext=nil means "keep existing".
		return s.store.UpsertBotTransportState(ctx, botID, nil, 1, reconnectState, errorCode)
	}
	ct, version, err := s.cipher.Encrypt([]byte(plaintextBuf))
	if err != nil {
		return fmt.Errorf("encrypt cursor: %w", err)
	}
	return s.store.UpsertBotTransportState(ctx, botID, ct, version, reconnectState, errorCode)
}

// Compile-time guard: botLifecycleService must satisfy BotLifecycleService.
var _ BotLifecycleService = (*botLifecycleService)(nil)
