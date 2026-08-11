package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/domain"
)

// fakeChannelService 是 ChannelConfigService 的测试替身。
type fakeChannelService struct {
	cfgs      []domain.ChannelConfig
	err       error
	savedType domain.ChannelType
	savedID   string
	unbound   domain.ChannelType
	startErr  error
}

func (f *fakeChannelService) ListBindings(_ context.Context, _ int64) ([]domain.ChannelConfig, error) {
	return f.cfgs, f.err
}

func (f *fakeChannelService) GetChannelConfig(_ context.Context, _ int64, _ domain.ChannelType) (domain.ChannelConfig, error) {
	return domain.ChannelConfig{}, f.err
}

func (f *fakeChannelService) UpsertCredentials(_ context.Context, _ int64, channelType domain.ChannelType, appID, appSecret string) (domain.ChannelConfig, error) {
	if f.startErr != nil {
		return domain.ChannelConfig{}, f.startErr
	}
	// 与真实服务一致的最小校验(app_secret 缺失 → 400 语义)。
	if appID == "" || appSecret == "" {
		return domain.ChannelConfig{}, errors.New("app_id/app_secret are required")
	}
	f.savedType = channelType
	f.savedID = appID
	return domain.ChannelConfig{
		BotUUID:     "uuid-feishu",
		ChannelType: channelType,
		OwnerID:     9,
		State:       domain.ChannelActive,
		CreatedAt:   time.Now().UTC(),
		UpdatedAt:   time.Now().UTC(),
	}, nil
}

func (f *fakeChannelService) Unbind(_ context.Context, _ int64, channelType domain.ChannelType) (domain.ChannelConfig, error) {
	if f.err != nil {
		return domain.ChannelConfig{}, f.err
	}
	f.unbound = channelType
	return domain.ChannelConfig{
		BotUUID:     "uuid-" + string(channelType),
		ChannelType: channelType,
		OwnerID:     9,
		State:       domain.ChannelDisabled,
		CreatedAt:   time.Now().UTC(),
	}, nil
}

// fakeCipher round-trips plaintext unchanged (Encrypt tags version 1).
type fakeCipher struct{}

func (fakeCipher) Encrypt(plaintext []byte) ([]byte, int, error) {
	return plaintext, 1, nil
}

func (fakeCipher) Decrypt(ciphertext []byte, _ int) ([]byte, error) {
	return ciphertext, nil
}

func imBindingsFixture(t *testing.T, svc *fakeChannelService) *Server {
	t.Helper()
	srv, err := NewServer(ServerConfig{
		Service:              &fakeTaskService{},
		Registry:             &fakeRegistry{},
		ChannelConfigService: svc,
		Invite:               &fixedInviteService{userID: 9},
		AdminToken:           "test-admin token",
		AdminUserID:          9,
		SessionKey:           "personal:9",
		Cipher:               fakeCipher{},
	})
	if err != nil {
		t.Fatalf("server: %v", err)
	}
	return srv
}

func TestGetOwnBindingsListsChannelsWithMaskedMeta(t *testing.T) {
	svc := &fakeChannelService{cfgs: []domain.ChannelConfig{
		{
			BotUUID: "b-wx", ChannelType: domain.ChannelWechat,
			IlinkBotID: "ilink-1", IlinkUserID: "ilink-user-123456",
			State: domain.ChannelActive, CreatedAt: time.Now().UTC(),
		},
		{
			BotUUID: "b-fs", ChannelType: domain.ChannelFeishu,
			ConfigCiphertext: []byte(`{"app_id":"cli_abcdefghij","app_secret":"s"}`),
			ConfigKeyVersion: 1, State: domain.ChannelActive, CreatedAt: time.Now().UTC(),
		},
	}}
	srv := imBindingsFixture(t, svc)

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/me/im-bindings", nil)
	srv.Handler().ServeHTTP(rr, bearerHeader(req))
	if rr.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rr.Code, rr.Body.String())
	}
	var out []map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("expected 2 bindings, got %d", len(out))
	}
	meta := out[0]["meta"].(map[string]any)
	if meta["ilink_bot_id"] != "ilink-1" {
		t.Fatalf("wechat meta: %+v", meta)
	}
	if got := meta["channel_account_id"].(string); got != "ili****456" {
		t.Fatalf("wechat account not masked: %q", got)
	}
	metaFS := out[1]["meta"].(map[string]any)
	if got := metaFS["app_id"].(string); got != "cli****hij" {
		t.Fatalf("feishu app_id not masked: %q", got)
	}
	if _, leaked := out[1]["meta"].(map[string]any)["app_secret"]; leaked {
		t.Fatal("app_secret leaked in binding reply")
	}
}

func TestSaveChannelBindingValidatesAndPersists(t *testing.T) {
	svc := &fakeChannelService{}
	srv := imBindingsFixture(t, svc)

	// wechat 不在凭据 CRUD 范围 → 400。
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/v1/me/im-bindings/wechat",
		strings.NewReader(`{"app_id":"a","app_secret":"s"}`))
	srv.Handler().ServeHTTP(rr, bearerHeader(req))
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("wechat PUT must be rejected, got %d", rr.Code)
	}

	// 未知渠道 → 400。
	rr = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPut, "/v1/me/im-bindings/telegram",
		strings.NewReader(`{"app_id":"a","app_secret":"s"}`))
	srv.Handler().ServeHTTP(rr, bearerHeader(req))
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("unknown channel must be rejected, got %d", rr.Code)
	}

	// 缺 app_secret → 400。
	rr = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPut, "/v1/me/im-bindings/feishu",
		strings.NewReader(`{"app_id":"a"}`))
	srv.Handler().ServeHTTP(rr, bearerHeader(req))
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("missing app_secret must be rejected, got %d", rr.Code)
	}

	// 正常保存 → 200 + binding。
	rr = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPut, "/v1/me/im-bindings/dingtalk",
		strings.NewReader(`{"app_id":"ding-app","app_secret":"ding-secret"}`))
	req.Header.Set("Content-Type", "application/json")
	srv.Handler().ServeHTTP(rr, bearerHeader(req))
	if rr.Code != http.StatusOK {
		t.Fatalf("save dingtalk: %d %s", rr.Code, rr.Body.String())
	}
	if svc.savedType != domain.ChannelDingTalk || svc.savedID != "ding-app" {
		t.Fatalf("service got type=%s app_id=%s", svc.savedType, svc.savedID)
	}
	var out map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out["channel_type"] != "dingtalk" || out["state"] != "active" {
		t.Fatalf("reply: %+v", out)
	}
}

func TestSaveWeComBindingPersists(t *testing.T) {
	svc := &fakeChannelService{}
	srv := imBindingsFixture(t, svc)

	// 企业微信智能机器人: bot_id/secret 走 app_id/app_secret 槽位。
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/v1/me/im-bindings/wecom",
		strings.NewReader(`{"app_id":"wecom-bot","app_secret":"wecom-secret"}`))
	req.Header.Set("Content-Type", "application/json")
	srv.Handler().ServeHTTP(rr, bearerHeader(req))
	if rr.Code != http.StatusOK {
		t.Fatalf("save wecom: %d %s", rr.Code, rr.Body.String())
	}
	if svc.savedType != domain.ChannelWecom || svc.savedID != "wecom-bot" {
		t.Fatalf("service got type=%s app_id=%s", svc.savedType, svc.savedID)
	}
	var out map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out["channel_type"] != "wecom" || out["state"] != "active" {
		t.Fatalf("reply: %+v", out)
	}
}

func TestUnbindChannelDisables(t *testing.T) {
	svc := &fakeChannelService{}
	srv := imBindingsFixture(t, svc)

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, "/v1/me/im-bindings/qq", nil)
	srv.Handler().ServeHTTP(rr, bearerHeader(req))
	if rr.Code != http.StatusOK {
		t.Fatalf("unbind qq: %d %s", rr.Code, rr.Body.String())
	}
	if svc.unbound != domain.ChannelQQ {
		t.Fatalf("service unbound %s, want qq", svc.unbound)
	}
	var out map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out["state"] != "disabled" {
		t.Fatalf("reply state: %+v", out)
	}

	// 不存在 → 404。
	svc.err = domain.ErrChannelBindingNotFound
	rr = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodDelete, "/v1/me/im-bindings/feishu", nil)
	srv.Handler().ServeHTTP(rr, bearerHeader(req))
	if rr.Code != http.StatusNotFound {
		t.Fatalf("unbind missing channel: %d", rr.Code)
	}
	_ = errors.New // keep errors import for parity
}

func TestAdminIMBindingsEndpoints(t *testing.T) {
	svc := &fakeChannelService{cfgs: []domain.ChannelConfig{}}
	srv := imBindingsFixture(t, svc)

	// admin GET 用 AdminUserID。
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/admin/me/im-bindings", nil)
	req.Header.Set("X-Platform-Admin-Token", "test-admin token")
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("admin list: %d %s", rr.Code, rr.Body.String())
	}

	// admin PUT。
	rr = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPut, "/v1/admin/me/im-bindings/qq",
		strings.NewReader(`{"app_id":"qq-app","app_secret":"qq-secret"}`))
	req.Header.Set("X-Platform-Admin-Token", "test-admin token")
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("admin save: %d %s", rr.Code, rr.Body.String())
	}
	if svc.savedType != domain.ChannelQQ {
		t.Fatalf("admin saved %s, want qq", svc.savedType)
	}

	// admin DELETE。
	rr = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodDelete, "/v1/admin/me/im-bindings/feishu", nil)
	req.Header.Set("X-Platform-Admin-Token", "test-admin token")
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("admin unbind: %d %s", rr.Code, rr.Body.String())
	}

	// 未认证 → 401。
	rr = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/v1/me/im-bindings", nil)
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated list: %d", rr.Code)
	}
}

// TestUpsertCredentialsPollerStartFailure 验证"保存即生效"的降级语义:
// 落库成功但 poller 启动失败时, API 返回 400 且错误可见(不静默成功)。
func TestUpsertCredentialsPollerStartFailure(t *testing.T) {
	svc := &fakeChannelService{startErr: errors.New("poller start failed")}
	srv := imBindingsFixture(t, svc)

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/v1/me/im-bindings/feishu",
		strings.NewReader(`{"app_id":"a","app_secret":"s"}`))
	req.Header.Set("Content-Type", "application/json")
	srv.Handler().ServeHTTP(rr, bearerHeader(req))
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 on poller start failure, got %d: %s", rr.Code, rr.Body.String())
	}
}
