package application

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/domain"
)

type fakeChannelConfigStore struct {
	cfgs     map[domain.ChannelType]domain.ChannelConfig
	upserted domain.ChannelConfig
	disabled domain.ChannelType
	err      error
}

func newFakeChannelConfigStore() *fakeChannelConfigStore {
	return &fakeChannelConfigStore{cfgs: map[domain.ChannelType]domain.ChannelConfig{}}
}

func (f *fakeChannelConfigStore) GetChannelConfigsByOwner(_ context.Context, _ int64) ([]domain.ChannelConfig, error) {
	out := make([]domain.ChannelConfig, 0, len(f.cfgs))
	for _, c := range f.cfgs {
		out = append(out, c)
	}
	return out, f.err
}

func (f *fakeChannelConfigStore) GetChannelConfigByOwnerAndType(_ context.Context, _ int64, t domain.ChannelType) (domain.ChannelConfig, error) {
	c, ok := f.cfgs[t]
	if !ok {
		return domain.ChannelConfig{}, pgx.ErrNoRows
	}
	return c, f.err
}

func (f *fakeChannelConfigStore) UpsertChannelConfigCredentials(_ context.Context, _ int64, t domain.ChannelType, ct []byte, v int) (domain.ChannelConfig, error) {
	c := domain.ChannelConfig{
		BotUUID:          "uuid-" + string(t),
		ChannelType:      t,
		OwnerID:          9,
		ConfigCiphertext: ct,
		ConfigKeyVersion: v,
		State:            domain.ChannelActive,
	}
	f.upserted = c
	f.cfgs[t] = c
	return c, f.err
}

func (f *fakeChannelConfigStore) DisableChannelConfig(_ context.Context, _ int64, t domain.ChannelType) error {
	if _, ok := f.cfgs[t]; !ok {
		return pgx.ErrNoRows
	}
	f.disabled = t
	return nil
}

// fakeCipherNoop round-trips plaintext (test helper).
type fakeCipherNoop struct{}

func (fakeCipherNoop) Encrypt(plaintext []byte) ([]byte, int, error) {
	return plaintext, 3, nil
}

func (fakeCipherNoop) Decrypt(ciphertext []byte, _ int) ([]byte, error) {
	return ciphertext, nil
}

type recorderStopper struct {
	stopped []string
	started []domain.ChannelConfig
	err     error
}

func (r *recorderStopper) StopBot(_ context.Context, botUUID string) error {
	r.stopped = append(r.stopped, botUUID)
	return r.err
}

func TestChannelConfigServiceUpsertEncryptsAndStartsPoller(t *testing.T) {
	store := newFakeChannelConfigStore()
	rec := &recorderStopper{}
	svc, err := NewChannelConfigService(ChannelConfigServiceConfig{
		Store:  store,
		Cipher: fakeCipherNoop{},
		Start:  func(_ context.Context, c domain.ChannelConfig) error { rec.started = append(rec.started, c); return nil },
		Stop:   rec,
	})
	if err != nil {
		t.Fatal(err)
	}

	cfg, err := svc.UpsertCredentials(context.Background(), 9, domain.ChannelFeishu, "cli_app", "secret-1")
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}
	// 凭据 JSON 加密入库(app_secret 永不明文)。
	if string(cfg.ConfigCiphertext) == "" {
		t.Fatal("config ciphertext empty")
	}
	plain, _ := fakeCipherNoop{}.Decrypt(cfg.ConfigCiphertext, 3)
	if string(plain) != `{"app_id":"cli_app","app_secret":"secret-1"}` {
		t.Fatalf("config json = %s", plain)
	}
	if len(rec.started) != 1 || rec.started[0].BotUUID != cfg.BotUUID {
		t.Fatalf("poller start not triggered: %+v", rec.started)
	}

	// 凭据更新: 旧连接停止 + 新连接启动。
	store.cfgs[domain.ChannelFeishu] = domain.ChannelConfig{
		BotUUID: "uuid-old", ChannelType: domain.ChannelFeishu, OwnerID: 9,
	}
	rec.started = nil
	if _, err := svc.UpsertCredentials(context.Background(), 9, domain.ChannelFeishu, "cli_app", "secret-2"); err != nil {
		t.Fatalf("re-upsert: %v", err)
	}
	if len(rec.stopped) != 1 || rec.stopped[0] != "uuid-old" {
		t.Fatalf("stale connection not stopped: %+v", rec.stopped)
	}
}

func TestChannelConfigServiceValidation(t *testing.T) {
	store := newFakeChannelConfigStore()
	svc, err := NewChannelConfigService(ChannelConfigServiceConfig{
		Store: store, Cipher: fakeCipherNoop{},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	// wechat 不允许凭据配置(QR 专属)。
	if _, err := svc.UpsertCredentials(ctx, 9, domain.ChannelWechat, "a", "b"); err == nil {
		t.Fatal("wechat credentials must be rejected")
	}
	// 未知渠道。
	if _, err := svc.UpsertCredentials(ctx, 9, "telegram", "a", "b"); err == nil {
		t.Fatal("telegram must be rejected")
	}
	// 空 app_id/app_secret。
	if _, err := svc.UpsertCredentials(ctx, 9, domain.ChannelQQ, "", "b"); err == nil {
		t.Fatal("empty app_id must be rejected")
	}
	if _, err := svc.UpsertCredentials(ctx, 9, domain.ChannelQQ, "a", ""); err == nil {
		t.Fatal("empty app_secret must be rejected")
	}
	// 超长。
	long := make([]byte, 300)
	for i := range long {
		long[i] = 'x'
	}
	if _, err := svc.UpsertCredentials(ctx, 9, domain.ChannelQQ, "a", string(long)); err == nil {
		t.Fatal("oversized app_secret must be rejected")
	}
}

func TestChannelConfigServiceUnbindDisablesAndStops(t *testing.T) {
	store := newFakeChannelConfigStore()
	store.cfgs[domain.ChannelDingTalk] = domain.ChannelConfig{
		BotUUID: "uuid-dt", ChannelType: domain.ChannelDingTalk, OwnerID: 9,
		State: domain.ChannelActive,
	}
	rec := &recorderStopper{}
	svc, err := NewChannelConfigService(ChannelConfigServiceConfig{
		Store: store, Cipher: fakeCipherNoop{}, Stop: rec,
	})
	if err != nil {
		t.Fatal(err)
	}

	cfg, err := svc.Unbind(context.Background(), 9, domain.ChannelDingTalk)
	if err != nil {
		t.Fatalf("unbind: %v", err)
	}
	if cfg.State != domain.ChannelDisabled {
		t.Fatalf("state = %s", cfg.State)
	}
	if store.disabled != domain.ChannelDingTalk {
		t.Fatal("store disable not called")
	}
	if len(rec.stopped) != 1 || rec.stopped[0] != "uuid-dt" {
		t.Fatalf("poller stop not triggered: %+v", rec.stopped)
	}

	// 未绑定渠道 → ErrChannelBindingNotFound。
	if _, err := svc.Unbind(context.Background(), 9, domain.ChannelQQ); !errors.Is(err, domain.ErrChannelBindingNotFound) {
		t.Fatalf("unbind missing channel: %v", err)
	}
}
