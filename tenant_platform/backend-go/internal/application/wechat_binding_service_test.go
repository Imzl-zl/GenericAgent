package application

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/domain"
	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/infrastructure/ilink"
)

// fakeQRStore implements both WechatQRSessionStore and BotQRStore in-memory.
type fakeQRStore struct {
	sess        domain.WechatQRSession
	existingBot domain.ChannelConfig // 重新绑定前 owner 已有的旧 bot(GetChannelConfigByOwnerAndType)
	lastToken   []byte               // UpdateWechatQRSessionStatus 收到的 token 密文(透传 cipher 下=明文)
}

func (f *fakeQRStore) CreateWechatQRSession(ctx context.Context, userID int64, ilinkQRCode, imgURL string, expiresAt time.Time) (domain.WechatQRSession, error) {
	return f.sess, nil
}

func (f *fakeQRStore) GetWechatQRSessionByQRCode(ctx context.Context, qrCode string) (domain.WechatQRSession, error) {
	return f.sess, nil
}

func (f *fakeQRStore) UpdateWechatQRSessionStatus(ctx context.Context, id string, status domain.WechatQRStatus,
	ilinkBotID, ilinkUserID, baseurl string, tokenCiphertext []byte) (domain.WechatQRSession, error) {
	f.sess.Status = status
	f.lastToken = tokenCiphertext
	return f.sess, nil
}

func (f *fakeQRStore) CreateChannelConfigFromQRSession(ctx context.Context, sess domain.WechatQRSession, tokenKeyVersion int) (domain.ChannelConfig, error) {
	// 模拟真实语义: 重新扫码总是生成全新 bot_uuid(ON CONFLICT 覆盖)。
	return domain.ChannelConfig{
		BotUUID:     "rebound-new-uuid",
		ID:          2,
		OwnerID:     sess.UserID,
		IlinkBotID:  sess.ILINKBotID,
		IlinkUserID: sess.ILINKUserID,
		State:       domain.ChannelActive,
		ChannelType: domain.ChannelWechat}, nil
}

func (f *fakeQRStore) GetBoundChannelConfigByIlinkUser(ctx context.Context, ilinkUserID string) (domain.ChannelConfig, error) {
	return domain.ChannelConfig{}, nil
}

func (f *fakeQRStore) GetChannelConfigByOwnerAndType(ctx context.Context, ownerID int64, channelType domain.ChannelType) (domain.ChannelConfig, error) {
	if f.existingBot.BotUUID == "" {
		return domain.ChannelConfig{}, pgx.ErrNoRows
	}
	return f.existingBot, nil
}

// fakeStaleBotStopper 记录 StopBot 调用, 模拟 bot lifecycle 的旧会话停止。
type fakeStaleBotStopper struct {
	stopped []string
}

func (f *fakeStaleBotStopper) StopBot(_ context.Context, botUUID string) error {
	f.stopped = append(f.stopped, botUUID)
	return nil
}

type fakeCipher struct{}

func (fakeCipher) Encrypt(plaintext []byte) ([]byte, int, error) { return plaintext, 0, nil }
func (fakeCipher) Decrypt(ciphertext []byte, keyVersion int) ([]byte, error) {
	return ciphertext, nil
}

// TestPollStatusPersistsTokenAsJSONContract: QR 绑定写入的凭据必须是
// {"token": ...} JSON(08-10 契约)——历史坑: 曾直接加密 iLink 裸 BotToken
// (xxx@im.bot:yyy), restore 时 poller marshal 失败; 2026-08-14 复发后根治。
func TestPollStatusPersistsTokenAsJSONContract(t *testing.T) {
	store := &fakeQRStore{sess: domain.WechatQRSession{
		ID: "sess-json", UserID: 79, ILINKQRCode: "qr-token",
		Status: domain.WechatQRWait, ExpiresAt: time.Now().UTC().Add(time.Hour),
	}}
	svc := newRebindTestService(t, store, &fakeStaleBotStopper{})
	if _, _, err := svc.PollStatus(context.Background(), "qr-token"); err != nil {
		t.Fatal(err)
	}
	if len(store.lastToken) == 0 {
		t.Fatal("token not persisted")
	}
	// fakeCipher 原样透传 → lastToken 即写入明文, 必须是合法 JSON {"token": ...}。
	var parsed struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(store.lastToken, &parsed); err != nil {
		t.Fatalf("persisted token is not JSON: %q (%v)", store.lastToken, err)
	}
	if parsed.Token != "token-1" {
		t.Fatalf("token = %q, want token-1", parsed.Token)
	}
}

// TestPollStatusLongPollTimeoutReportsLastKnownStatus is the regression guard
// for the 504: iLink get_qrcode_status long-polls (~30s) while a QR awaits a
// scan. The client caps each attempt and does not retry timeouts, so the
// service must report the last-known DB status (nil error) instead of failing
// the poll — otherwise the frontend poll loop aborts on every wait.
func TestPollStatusLongPollTimeoutReportsLastKnownStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(150 * time.Millisecond) // exceed the 30ms attempt timeout
	}))
	defer srv.Close()

	client, err := ilink.NewClient(ilink.ClientConfig{
		BaseURL:              srv.URL,
		StatusRequestTimeout: 30 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}

	store := &fakeQRStore{sess: domain.WechatQRSession{
		ID:          "sess-1",
		UserID:      1,
		ILINKQRCode: "qr-token",
		Status:      domain.WechatQRWait,
		ExpiresAt:   time.Now().UTC().Add(time.Hour),
	}}
	svc, err := NewWechatQRBindingService(WechatQRBindingConfig{
		Store:       store,
		BotStore:    store,
		ILinkClient: client,
		Cipher:      fakeCipher{},
	})
	if err != nil {
		t.Fatalf("new service: %v", err)
	}

	sess, bot, err := svc.PollStatus(context.Background(), "qr-token")
	if err != nil {
		t.Fatalf("long-poll timeout must not fail the poll: %v", err)
	}
	if sess.ID != "sess-1" {
		t.Errorf("session=%q, want last-known DB session sess-1", sess.ID)
	}
	if sess.Status != domain.WechatQRWait {
		t.Errorf("status=%q, want last-known wait", sess.Status)
	}
	if bot.ID != 0 {
		t.Errorf("unexpected bot %+v on timeout path", bot)
	}
}

// newConfirmedStatusServer 返回立即应答 confirmed 凭证的 iLink 测试服务。
func newConfirmedStatusServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"status":"confirmed","ret":0,"ilink_bot_id":"ilink-bot-1","bot_token":"token-1","ilink_user_id":"ilink-user-1"}`)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func newRebindTestService(t *testing.T, store *fakeQRStore, stopper StaleBotStopper) WechatQRBindingService {
	t.Helper()
	client, err := ilink.NewClient(ilink.ClientConfig{BaseURL: newConfirmedStatusServer(t).URL})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	svc, err := NewWechatQRBindingService(WechatQRBindingConfig{
		Store:       store,
		BotStore:    store,
		ILinkClient: client,
		Cipher:      fakeCipher{},
		StaleBots:   stopper,
	})
	if err != nil {
		t.Fatalf("new service: %v", err)
	}
	return svc
}

// 重新扫码绑定会生成全新 bot_uuid(ON CONFLICT (owner_id) DO UPDATE 覆盖),
// 旧 UUID 的 Poller 轮询会话必须被停止——否则旧会话继续推送带旧 UUID 的
// webhook, 平台查不到 → unknown bot; 且与新模式并存产生双回复。
func TestPollStatusRebindStopsStaleBotSession(t *testing.T) {
	stopper := &fakeStaleBotStopper{}
	store := &fakeQRStore{
		sess: domain.WechatQRSession{
			ID: "sess-1", UserID: 1, ILINKQRCode: "qr-token",
			Status: domain.WechatQRWait, ExpiresAt: time.Now().UTC().Add(time.Hour),
		},
		existingBot: domain.ChannelConfig{BotUUID: "old-uuid", ID: 1, OwnerID: 1, State: domain.ChannelActive, ChannelType: domain.ChannelWechat},
	}
	svc := newRebindTestService(t, store, stopper)

	_, bot, err := svc.PollStatus(context.Background(), "qr-token")
	if err != nil {
		t.Fatalf("rebind confirm failed: %v", err)
	}
	if bot.BotUUID != "rebound-new-uuid" {
		t.Fatalf("expected new bot uuid, got %q", bot.BotUUID)
	}
	if len(stopper.stopped) != 1 || stopper.stopped[0] != "old-uuid" {
		t.Fatalf("stale session must be stopped exactly once with old uuid, got %v", stopper.stopped)
	}
}

// 首次绑定(owner 无旧 bot)不得调用 StopBot。
func TestPollStatusFirstBindDoesNotStopStale(t *testing.T) {
	stopper := &fakeStaleBotStopper{}
	store := &fakeQRStore{
		sess: domain.WechatQRSession{
			ID: "sess-1", UserID: 1, ILINKQRCode: "qr-token",
			Status: domain.WechatQRWait, ExpiresAt: time.Now().UTC().Add(time.Hour),
		},
	}
	svc := newRebindTestService(t, store, stopper)

	if _, _, err := svc.PollStatus(context.Background(), "qr-token"); err != nil {
		t.Fatalf("first bind confirm failed: %v", err)
	}
	if len(stopper.stopped) != 0 {
		t.Fatalf("first bind must not stop any stale session, got %v", stopper.stopped)
	}
}
