package transport

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/domain"
	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/secret"
)

const defaultSendTimeout = 15 * time.Second

// BotResolver supplies bot metadata (including encrypted token) to the adapter.
type BotResolver interface {
	GetBotByUUID(ctx context.Context, botUUID string) (domain.Bot, error)
}

// ILinkAdapterConfig wires the iLink HTTP adapter dependencies.
type ILinkAdapterConfig struct {
	BaseURL   string
	Client    *http.Client
	Cipher    secret.TokenCipher
	Resolver  BotResolver
}

// ILinkAdapter is the production BotTransportAdapter for iLink.
// It resolves the bot token, calls the iLink send-message API, and keeps an
// in-memory idempotency window. For multi-instance deployments the
// idempotency store must be externalized (spec §6.1).
type ILinkAdapter struct {
	cfg       ILinkAdapterConfig
	mu        sync.Mutex
	seen      map[string]bool // key = botUUID + "|" + messageID
}

// NewILinkAdapter validates config and returns a production adapter.
func NewILinkAdapter(cfg ILinkAdapterConfig) (*ILinkAdapter, error) {
	if err := validateILinkConfig(cfg); err != nil {
		return nil, err
	}
	if cfg.Client == nil {
		cfg.Client = &http.Client{Timeout: defaultSendTimeout}
	}
	return &ILinkAdapter{
		cfg:  cfg,
		seen: make(map[string]bool),
	}, nil
}

func validateILinkConfig(cfg ILinkAdapterConfig) error {
	if strings.TrimSpace(cfg.BaseURL) == "" {
		return errors.New("BaseURL is required")
	}
	if cfg.Cipher == nil {
		return errors.New("Cipher is required")
	}
	if cfg.Resolver == nil {
		return errors.New("Resolver is required")
	}
	return nil
}

// SendMessage resolves the bot token and delivers text via iLink.
func (a *ILinkAdapter) SendMessage(ctx context.Context, botUUID, ilinkUserID, text string) error {
	if botUUID == "" || ilinkUserID == "" || text == "" {
		return errors.New("bot uuid, ilink user id, and text are required")
	}
	bot, err := a.cfg.Resolver.GetBotByUUID(ctx, botUUID)
	if err != nil {
		return fmt.Errorf("resolve bot: %w", err)
	}
	if bot.IlinkUserID != ilinkUserID {
		return errors.New("ilink user id mismatch")
	}
	token, err := a.cfg.Cipher.Decrypt(bot.TokenCiphertext, bot.TokenKeyVersion)
	if err != nil {
		return fmt.Errorf("decrypt bot token: %w", err)
	}
	return a.send(ctx, string(token), ilinkUserID, text)
}

func (a *ILinkAdapter) send(ctx context.Context, botToken, ilinkUserID, text string) error {
	url := strings.TrimRight(a.cfg.BaseURL, "/") + "/v1/messages/send"
	body := ilinkSendRequest{
		Token:   botToken,
		ToUser:  ilinkUserID,
		Content: text,
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("marshal request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	return a.doSend(req)
}

func (a *ILinkAdapter) doSend(req *http.Request) error {
	resp, err := a.cfg.Client.Do(req)
	if err != nil {
		return fmt.Errorf("ilink request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	msg, _ := io.ReadAll(resp.Body)
	return fmt.Errorf("ilink status %d: %s", resp.StatusCode, string(msg))
}

// RecordMessageIdempotency returns true the first time a message is seen.
func (a *ILinkAdapter) RecordMessageIdempotency(_ context.Context, botUUID, messageID string) (bool, error) {
	if botUUID == "" || messageID == "" {
		return false, errors.New("bot uuid and message id are required")
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	key := botUUID + "|" + messageID
	if a.seen[key] {
		return false, nil
	}
	a.seen[key] = true
	return true, nil
}

type ilinkSendRequest struct {
	Token   string `json:"token"`
	ToUser  string `json:"to_user"`
	Content string `json:"content"`
}
