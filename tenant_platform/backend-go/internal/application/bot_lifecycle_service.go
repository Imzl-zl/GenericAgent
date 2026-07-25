package application

import (
	"context"
	"errors"
	"fmt"
	"log"

	"github.com/jackc/pgx/v5"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/domain"
	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/poller"
	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/secret"
)

// BotLifecycleStore is the persistence port for bot lifecycle orchestration.
// The Go platform owns encryption + persistence; the Python Poller owns the
// iLink protocol I/O. This store bridges them.
type BotLifecycleStore interface {
	GetBotByUUID(ctx context.Context, botUUID string) (domain.Bot, error)
	GetBotTransportState(ctx context.Context, botID int64) (domain.BotTransportState, error)
	UpsertBotTransportState(ctx context.Context, botID int64, cursorCiphertext []byte, reconnectState, lastErrorCode string) error
	ListActiveBoundBots(ctx context.Context) ([]domain.Bot, error)
	UpdateBotState(ctx context.Context, botUUID string, state domain.BotState) error
}

// BotLifecycleService orchestrates bot start/stop/restore against the Bot
// Poller. It decrypts bot tokens and cursors before handing them to the
// Poller, and re-encrypts/persists cursors returned by the Poller.
type BotLifecycleService interface {
	StartBotForBoundUser(ctx context.Context, bot domain.Bot) error
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
}

type botLifecycleService struct {
	store      BotLifecycleStore
	cipher     secret.TokenCipher
	poller     *poller.Client
	webhookURL string
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
	return &botLifecycleService{
		store:      cfg.Store,
		cipher:     cfg.Cipher,
		poller:     cfg.Poller,
		webhookURL: cfg.WebhookURL,
	}, nil
}

// StartBotForBoundUser decrypts the bot token and any persisted cursor, then
// tells the Poller to begin long-polling for this bot. Safe to call on a fresh
// bot (no cursor yet) or after a platform restart (cursor resumes from DB).
func (s *botLifecycleService) StartBotForBoundUser(ctx context.Context, bot domain.Bot) error {
	if !bot.IsBound() {
		return fmt.Errorf("bot %s is not bound", bot.BotUUID)
	}
	token, err := s.cipher.Decrypt(bot.TokenCiphertext, bot.TokenKeyVersion)
	if err != nil {
		return fmt.Errorf("decrypt bot token: %w", err)
	}
	cursor, err := s.resolveCursor(ctx, bot.ID)
	if err != nil {
		return err
	}
	return s.poller.StartBot(ctx, poller.StartBotRequest{
		BotUUID:    bot.BotUUID,
		BotToken:   string(token),
		ILinkBotID: bot.IlinkBotID,
		BaseURL:    bot.BaseURL,
		UpdatesBuf: cursor,
		WebhookURL: s.webhookURL,
	})
}

// resolveCursor decrypts the persisted update cursor for a bot. Returns empty
// string when no cursor exists yet (first start).
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
	plain, err := s.cipher.Decrypt(st.UpdateCursorCiphertext, 1)
	if err != nil {
		return "", fmt.Errorf("decrypt cursor: %w", err)
	}
	return string(plain), nil
}

// StopBot tells the Poller to stop long-polling, then encrypts and persists the
// final cursor returned by the Poller.
func (s *botLifecycleService) StopBot(ctx context.Context, botUUID string) error {
	bot, err := s.store.GetBotByUUID(ctx, botUUID)
	if err != nil {
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
// so one bad bot does not block the rest.
func (s *botLifecycleService) RestoreActiveBots(ctx context.Context) error {
	bots, err := s.store.ListActiveBoundBots(ctx)
	if err != nil {
		return fmt.Errorf("list active bots: %w", err)
	}
	for _, bot := range bots {
		if err := s.StartBotForBoundUser(ctx, bot); err != nil {
			log.Printf("bot_lifecycle: restore bot %s failed: %v", bot.BotUUID, err)
			continue
		}
		log.Printf("bot_lifecycle: restored bot %s", bot.BotUUID)
	}
	return nil
}

// PersistUpdatesBuf encrypts and stores the plaintext cursor pushed by the
// Poller alongside an inbound message. Called by the IM webhook handler.
func (s *botLifecycleService) PersistUpdatesBuf(ctx context.Context, botUUID, plaintextBuf string) error {
	if plaintextBuf == "" {
		return nil
	}
	bot, err := s.store.GetBotByUUID(ctx, botUUID)
	if err != nil {
		return fmt.Errorf("resolve bot: %w", err)
	}
	return s.persistCursor(ctx, bot.ID, plaintextBuf, "polling", "")
}

// HandleAuthExpired marks the bot as expired and records the error in the
// transport state. The Poller has already stopped its loop for this bot.
func (s *botLifecycleService) HandleAuthExpired(ctx context.Context, botUUID string) error {
	bot, err := s.store.GetBotByUUID(ctx, botUUID)
	if err != nil {
		return fmt.Errorf("resolve bot: %w", err)
	}
	if err := s.store.UpdateBotState(ctx, botUUID, domain.BotExpired); err != nil {
		return fmt.Errorf("mark bot expired: %w", err)
	}
	return s.store.UpsertBotTransportState(ctx, bot.ID, nil, "error", "AUTH_EXPIRED")
}

// persistCursor encrypts the plaintext cursor and upserts the transport state.
func (s *botLifecycleService) persistCursor(ctx context.Context, botID int64, plaintextBuf, reconnectState, errorCode string) error {
	if plaintextBuf == "" {
		return s.store.UpsertBotTransportState(ctx, botID, nil, reconnectState, errorCode)
	}
	ct, _, err := s.cipher.Encrypt([]byte(plaintextBuf))
	if err != nil {
		return fmt.Errorf("encrypt cursor: %w", err)
	}
	return s.store.UpsertBotTransportState(ctx, botID, ct, reconnectState, errorCode)
}
