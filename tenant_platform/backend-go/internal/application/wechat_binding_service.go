package application

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/domain"
	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/ilink"
	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/secret"
)

// QRSessionTTL is how long a WeChat QR login session remains valid.
// iLink QR codes expire around 5 minutes; we keep a small safety margin.
const QRSessionTTL = 4 * time.Minute

// WechatQRSessionStore persists QR login attempts.
type WechatQRSessionStore interface {
	CreateWechatQRSession(ctx context.Context, userID int64, ilinkQRCode, imgURL string, expiresAt time.Time) (domain.WechatQRSession, error)
	GetWechatQRSessionByQRCode(ctx context.Context, qrCode string) (domain.WechatQRSession, error)
	UpdateWechatQRSessionStatus(ctx context.Context, id string, status domain.WechatQRStatus,
		ilinkBotID, ilinkUserID, baseurl string, tokenCiphertext []byte) (domain.WechatQRSession, error)
}

// BotQRStore creates and resolves bots produced from QR sessions.
type BotQRStore interface {
	CreateBotFromQRSession(ctx context.Context, sess domain.WechatQRSession, tokenKeyVersion int) (domain.Bot, error)
	GetBoundBotByIlinkUser(ctx context.Context, ilinkUserID string) (domain.Bot, error)
}

// WechatQRBindingService manages official iLink QR-code binding.
type WechatQRBindingService interface {
	GenerateQRCode(ctx context.Context, userID int64) (domain.WechatQRSession, error)
	PollStatus(ctx context.Context, qrCode string) (domain.WechatQRSession, domain.Bot, error)
}

// WechatQRBindingConfig wires the service.
type WechatQRBindingConfig struct {
	Store       WechatQRSessionStore
	BotStore    BotQRStore
	ILinkClient *ilink.Client
	Cipher      secret.TokenCipher
}

type wechatQRBindingService struct {
	store    WechatQRSessionStore
	botStore BotQRStore
	client   *ilink.Client
	cipher   secret.TokenCipher
}

// NewWechatQRBindingService constructs the service.
func NewWechatQRBindingService(cfg WechatQRBindingConfig) (WechatQRBindingService, error) {
	if cfg.Store == nil {
		return nil, errors.New("store is required")
	}
	if cfg.BotStore == nil {
		return nil, errors.New("bot store is required")
	}
	if cfg.ILinkClient == nil {
		return nil, errors.New("ilink client is required")
	}
	if cfg.Cipher == nil {
		return nil, errors.New("cipher is required")
	}
	return &wechatQRBindingService{
		store:    cfg.Store,
		botStore: cfg.BotStore,
		client:   cfg.ILinkClient,
		cipher:   cfg.Cipher,
	}, nil
}

func (s *wechatQRBindingService) GenerateQRCode(ctx context.Context, userID int64) (domain.WechatQRSession, error) {
	if userID <= 0 {
		return domain.WechatQRSession{}, fmt.Errorf("user id must be positive")
	}
	resp, err := s.client.GetBotQRCode(ctx)
	if err != nil {
		return domain.WechatQRSession{}, fmt.Errorf("ilink get qrcode: %w", err)
	}
	expiresAt := time.Now().UTC().Add(QRSessionTTL)
	return s.store.CreateWechatQRSession(ctx, userID, resp.QRCode, resp.QRCodeImgContent, expiresAt)
}

func (s *wechatQRBindingService) PollStatus(ctx context.Context, qrCode string) (domain.WechatQRSession, domain.Bot, error) {
	if qrCode == "" {
		return domain.WechatQRSession{}, domain.Bot{}, fmt.Errorf("qrcode is required")
	}
	sess, err := s.store.GetWechatQRSessionByQRCode(ctx, qrCode)
	if err != nil {
		return domain.WechatQRSession{}, domain.Bot{}, fmt.Errorf("get session: %w", err)
	}
	if sess.Status == domain.WechatQRConfirmed {
		bot, err := s.loadBoundBot(ctx, sess)
		return sess, bot, err
	}
	if sess.IsExpired(time.Now().UTC()) {
		updated, err := s.store.UpdateWechatQRSessionStatus(ctx, sess.ID, domain.WechatQRExpired, "", "", "", nil)
		if err != nil {
			return domain.WechatQRSession{}, domain.Bot{}, err
		}
		return updated, domain.Bot{}, fmt.Errorf("qrcode expired")
	}

	statusResp, err := s.client.GetQRCodeStatus(ctx, qrCode)
	if err != nil {
		return domain.WechatQRSession{}, domain.Bot{}, fmt.Errorf("ilink status: %w", err)
	}
	status := mapILinkStatus(statusResp.Status)

	if status != domain.WechatQRConfirmed {
		updated, err := s.store.UpdateWechatQRSessionStatus(ctx, sess.ID, status, "", "", "", nil)
		return updated, domain.Bot{}, err
	}

	creds, ok := statusResp.ConfirmedCredentials()
	if !ok {
		return domain.WechatQRSession{}, domain.Bot{}, fmt.Errorf("ilink confirmed but credentials incomplete")
	}

	cipher, version, err := s.cipher.Encrypt([]byte(creds.BotToken))
	if err != nil {
		return domain.WechatQRSession{}, domain.Bot{}, fmt.Errorf("encrypt token: %w", err)
	}

	baseURL := creds.BaseURL
	if baseURL == "" {
		baseURL = sess.BaseURL
	}

	updated, err := s.store.UpdateWechatQRSessionStatus(ctx, sess.ID, status, creds.ILinkBotID, creds.ILinkUserID, baseURL, cipher)
	if err != nil {
		return domain.WechatQRSession{}, domain.Bot{}, fmt.Errorf("update session: %w", err)
	}

	bot, err := s.botStore.CreateBotFromQRSession(ctx, updated, version)
	if err != nil {
		return updated, domain.Bot{}, fmt.Errorf("create bot: %w", err)
	}
	return updated, bot, nil
}

func (s *wechatQRBindingService) loadBoundBot(ctx context.Context, sess domain.WechatQRSession) (domain.Bot, error) {
	if sess.ILINKUserID == "" {
		return domain.Bot{}, fmt.Errorf("session missing ilink user id")
	}
	return s.botStore.GetBoundBotByIlinkUser(ctx, sess.ILINKUserID)
}

func mapILinkStatus(status ilink.QRCodeStatus) domain.WechatQRStatus {
	switch status {
	case ilink.StatusScaned:
		return domain.WechatQRScaned
	case ilink.StatusScanedButRedirect:
		return domain.WechatQRRedirect
	case ilink.StatusExpired:
		return domain.WechatQRExpired
	case ilink.StatusConfirmed:
		return domain.WechatQRConfirmed
	default:
		return domain.WechatQRWait
	}
}
