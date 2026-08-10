package application

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/domain"
	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/infrastructure/ilink"
	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/infrastructure/secret"
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

// BotQRStore creates and resolves wechat channel configs produced from QR
// sessions.
type BotQRStore interface {
	CreateChannelConfigFromQRSession(ctx context.Context, sess domain.WechatQRSession, configKeyVersion int) (domain.ChannelConfig, error)
	GetBoundChannelConfigByIlinkUser(ctx context.Context, ilinkUserID string) (domain.ChannelConfig, error)
	GetChannelConfigByOwnerAndType(ctx context.Context, ownerID int64, channelType domain.ChannelType) (domain.ChannelConfig, error)
}

// StaleBotStopper 停止重新绑定前的旧 bot 轮询会话。重新扫码会生成全新
// bot_uuid 并覆盖 bots 行(ON CONFLICT (owner_id) DO UPDATE), 旧 UUID 的
// Poller 长轮询线程不会自动退出——不停止的话旧会话继续推送带旧 UUID 的
// webhook, 平台查不到旧 UUID → 用户收到 unknown-bot 回复; 若 iLink 侧
// bot_id 未变则新旧会话并存, 同一条消息被双会话轮询 → 双回复/cursor 竞争。
// 由 cmd 层注入 bot lifecycle 实现; nil = 不停止(测试/无 Poller 环境)。
type StaleBotStopper interface {
	StopBot(ctx context.Context, botUUID string) error
}

// WechatQRBindingService manages official iLink QR-code binding.
type WechatQRBindingService interface {
	GenerateQRCode(ctx context.Context, userID int64) (domain.WechatQRSession, error)
	PollStatus(ctx context.Context, qrCode string) (domain.WechatQRSession, domain.ChannelConfig, error)
}

// WechatQRBindingConfig wires the service.
type WechatQRBindingConfig struct {
	Store       WechatQRSessionStore
	BotStore    BotQRStore
	ILinkClient *ilink.Client
	Cipher      secret.TokenCipher
	// StaleBots 停止重新绑定产生的旧会话(见 StaleBotStopper)。nil = 不停止。
	StaleBots StaleBotStopper
}

type wechatQRBindingService struct {
	store     WechatQRSessionStore
	botStore  BotQRStore
	client    *ilink.Client
	cipher    secret.TokenCipher
	staleBots StaleBotStopper

	// qrMu guards qrLocks. Each qr_code gets its own *sync.Mutex so concurrent
	// polls of the same QR session are serialized: the first caller advances
	// the iLink state machine and creates the bot; later callers see the
	// updated session and return without re-calling iLink or re-creating the bot.
	qrMu    sync.Mutex
	qrLocks map[string]*sync.Mutex
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
		store:     cfg.Store,
		botStore:  cfg.BotStore,
		client:    cfg.ILinkClient,
		cipher:    cfg.Cipher,
		staleBots: cfg.StaleBots,
		qrLocks:   make(map[string]*sync.Mutex),
	}, nil
}

// lockForQR returns a per-qr_code mutex and a release function. Callers must
// hold the mutex while mutating the QR session state machine so concurrent
// polls of the same qr_code do not race. The mutex map grows unboundedly with
// QR sessions; P0 has few sessions (4-minute TTL × handful of active bindings).
// If this becomes a problem, evict expired entries on access.
func (s *wechatQRBindingService) lockForQR(qrCode string) func() {
	s.qrMu.Lock()
	mu, ok := s.qrLocks[qrCode]
	if !ok {
		mu = &sync.Mutex{}
		s.qrLocks[qrCode] = mu
	}
	s.qrMu.Unlock()
	mu.Lock()
	return mu.Unlock
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

func (s *wechatQRBindingService) PollStatus(ctx context.Context, qrCode string) (domain.WechatQRSession, domain.ChannelConfig, error) {
	if qrCode == "" {
		return domain.WechatQRSession{}, domain.ChannelConfig{}, fmt.Errorf("qrcode is required")
	}
	// Serialize concurrent polls of the same qr_code. The first caller runs the
	// iLink state machine + bot creation; later callers observe the committed
	// session state and short-circuit. Without this lock, two concurrent polls
	// could both see status=confirmed, both call iLink, and both attempt to
	// create a bot for the same owner (the bots UNIQUE(owner_id) constraint
	// would reject the second, surfacing an ugly duplicate-key error).
	unlock := s.lockForQR(qrCode)
	defer unlock()

	sess, err := s.store.GetWechatQRSessionByQRCode(ctx, qrCode)
	if err != nil {
		return domain.WechatQRSession{}, domain.ChannelConfig{}, fmt.Errorf("get session: %w", err)
	}
	if sess.Status == domain.WechatQRConfirmed {
		bot, err := s.loadBoundBot(ctx, sess)
		return sess, bot, err
	}
	if sess.IsExpired(time.Now().UTC()) {
		updated, err := s.store.UpdateWechatQRSessionStatus(ctx, sess.ID, domain.WechatQRExpired, "", "", "", nil)
		if err != nil {
			return domain.WechatQRSession{}, domain.ChannelConfig{}, err
		}
		return updated, domain.ChannelConfig{}, fmt.Errorf("qrcode expired")
	}

	statusResp, err := s.client.GetQRCodeStatus(ctx, qrCode)
	if err != nil {
		// The status endpoint long-polls while a QR awaits a scan; a timeout
		// means "no change yet" (the client caps and does not retry it), so
		// report the last-known DB status and let the frontend keep polling.
		// Any other error is a real failure and is surfaced as-is.
		if errors.Is(err, context.DeadlineExceeded) {
			return sess, domain.ChannelConfig{}, nil
		}
		return domain.WechatQRSession{}, domain.ChannelConfig{}, fmt.Errorf("ilink status: %w", err)
	}
	status := mapILinkStatus(statusResp.Status)

	if status != domain.WechatQRConfirmed {
		updated, err := s.store.UpdateWechatQRSessionStatus(ctx, sess.ID, status, "", "", "", nil)
		return updated, domain.ChannelConfig{}, err
	}

	creds, ok := statusResp.ConfirmedCredentials()
	if !ok {
		return domain.WechatQRSession{}, domain.ChannelConfig{}, fmt.Errorf("ilink confirmed but credentials incomplete")
	}

	cipher, version, err := s.cipher.Encrypt([]byte(creds.BotToken))
	if err != nil {
		return domain.WechatQRSession{}, domain.ChannelConfig{}, fmt.Errorf("encrypt token: %w", err)
	}

	baseURL := creds.BaseURL
	if baseURL == "" {
		baseURL = sess.BaseURL
	}

	updated, err := s.store.UpdateWechatQRSessionStatus(ctx, sess.ID, status, creds.ILinkBotID, creds.ILinkUserID, baseURL, cipher)
	if err != nil {
		return domain.WechatQRSession{}, domain.ChannelConfig{}, fmt.Errorf("update session: %w", err)
	}

	// 重新绑定检测: 建新 config 前捕获当前 owner 的旧 wechat config(若有)。
	// 重新扫码会用 ON CONFLICT (owner_id, channel_type) DO UPDATE 覆盖行并
	// 生成全新 bot_uuid, 覆盖后旧 UUID 无法再从 DB 查到——必须先取出来, 供
	// 随后停止旧 Poller 会话, 否则旧会话继续推送 → unknown bot / 双回复。
	oldCfg, oldErr := s.botStore.GetChannelConfigByOwnerAndType(ctx, sess.UserID, domain.ChannelWechat)
	if oldErr != nil && !errors.Is(oldErr, pgx.ErrNoRows) {
		return domain.WechatQRSession{}, domain.ChannelConfig{}, fmt.Errorf("get existing bot: %w", oldErr)
	}

	cfg, err := s.botStore.CreateChannelConfigFromQRSession(ctx, updated, version)
	if err != nil {
		return updated, domain.ChannelConfig{}, fmt.Errorf("create bot: %w", err)
	}
	// 换 UUID 重新绑定: best-effort 停止旧会话。失败不阻断绑定——新会话由
	// handler 的 StartBotForBoundUser 启动, 停旧失败仅是短暂并存, 日志便于
	// 暴露 Poller 异常。
	if s.staleBots != nil && oldCfg.BotUUID != "" && oldCfg.BotUUID != cfg.BotUUID {
		if stopErr := s.staleBots.StopBot(ctx, oldCfg.BotUUID); stopErr != nil {
			slog.WarnContext(ctx, "wechat_binding: stop stale bot session failed",
				"old_bot_uuid", oldCfg.BotUUID,
				"new_bot_uuid", cfg.BotUUID,
				"owner_user_id", sess.UserID,
				"error", stopErr)
		}
	}
	return updated, cfg, nil
}

func (s *wechatQRBindingService) loadBoundBot(ctx context.Context, sess domain.WechatQRSession) (domain.ChannelConfig, error) {
	if sess.ILINKUserID == "" {
		return domain.ChannelConfig{}, fmt.Errorf("session missing ilink user id")
	}
	return s.botStore.GetBoundChannelConfigByIlinkUser(ctx, sess.ILINKUserID)
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
