package application

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/domain"
	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/infrastructure/poller"
	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/infrastructure/secret"
)

// reconcileFakeStore is an in-memory BotLifecycleStore for reconciliation
// tests. recheckState overrides the state returned by GetChannelConfigByUUID
// to simulate a config being deactivated between List and re-check.
type reconcileFakeStore struct {
	mu           sync.Mutex
	configs      []domain.ChannelConfig
	byUUID       map[string]domain.ChannelConfig
	recheckState map[string]domain.ChannelConfigState // uuid -> state seen at re-check time
	listErr      error
	upserted     []string
	updatedState []domain.ChannelConfigState
}

func newReconcileFakeStore(configs []domain.ChannelConfig) *reconcileFakeStore {
	byUUID := make(map[string]domain.ChannelConfig, len(configs))
	for _, c := range configs {
		byUUID[c.BotUUID] = c
	}
	return &reconcileFakeStore{configs: configs, byUUID: byUUID, recheckState: map[string]domain.ChannelConfigState{}}
}

func (s *reconcileFakeStore) GetChannelConfigByUUID(_ context.Context, botUUID string) (domain.ChannelConfig, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.byUUID[botUUID]
	if !ok {
		return domain.ChannelConfig{}, pgx.ErrNoRows
	}
	if st, ok := s.recheckState[botUUID]; ok {
		if st == "__deleted__" {
			return domain.ChannelConfig{}, pgx.ErrNoRows // simulate row removed after list
		}
		c.State = st
	}
	return c, nil
}

func (s *reconcileFakeStore) GetBotTransportState(_ context.Context, _ int64) (domain.BotTransportState, error) {
	return domain.BotTransportState{}, pgx.ErrNoRows
}

func (s *reconcileFakeStore) UpsertBotTransportState(_ context.Context, _ int64, _ []byte, _ int, _ string, _ string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.upserted = append(s.upserted, "x")
	return nil
}

func (s *reconcileFakeStore) ListActiveChannelConfigs(_ context.Context) ([]domain.ChannelConfig, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.listErr != nil {
		return nil, s.listErr
	}
	// Mirror the real SQL: state='active' AND (channel_type <> 'wechat' OR ilink_user_id IS NOT NULL).
	var active []domain.ChannelConfig
	for _, c := range s.configs {
		if c.State == domain.ChannelActive && (c.ChannelType != domain.ChannelWechat || c.IlinkUserID != "") {
			active = append(active, c)
		}
	}
	return active, nil
}

func (s *reconcileFakeStore) UpdateChannelConfigState(_ context.Context, botUUID string, state domain.ChannelConfigState) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.updatedState = append(s.updatedState, state)
	if c, ok := s.byUUID[botUUID]; ok {
		c.State = state
		s.byUUID[botUUID] = c
	}
	return nil
}

// fakePoller is an httptest poller recording /start and /stop calls.
type fakePoller struct {
	server     *httptest.Server
	mu         sync.Mutex
	activeBots []string
	starts     []string
	stops      []string
	healthErr  bool
}

func newFakePoller(activeBots []string) *fakePoller {
	fp := &fakePoller{activeBots: activeBots}
	fp.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fp.mu.Lock()
		defer fp.mu.Unlock()
		switch r.URL.Path {
		case "/health":
			if fp.healthErr {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"healthy":     true,
				"active_bots": fp.activeBots,
			})
		case "/start":
			var body struct {
				BotUUID string `json:"bot_uuid"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			fp.starts = append(fp.starts, body.BotUUID)
			_, _ = w.Write([]byte(`{"started":true}`))
		case "/stop":
			var body struct {
				BotUUID string `json:"bot_uuid"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			fp.stops = append(fp.stops, body.BotUUID)
			_, _ = w.Write([]byte(`{"stopped":true,"updates_buf":""}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	return fp
}

func (fp *fakePoller) close() { fp.server.Close() }
func (fp *fakePoller) recorded() (starts, stops []string) {
	fp.mu.Lock()
	defer fp.mu.Unlock()
	return append([]string(nil), fp.starts...), append([]string(nil), fp.stops...)
}

// newReconcileHarness builds a real BotLifecycleService wired to a fake poller
// and fake store. The ciphertext is produced with the real cipher so
// StartChannelConfig decrypts it like production.
func newReconcileHarness(t *testing.T, store *reconcileFakeStore, activeBots []string) (*botLifecycleService, *fakePoller) {
	t.Helper()
	cipher, err := secret.NewStaticKeyCipher(make([]byte, 32))
	if err != nil {
		t.Fatalf("cipher: %v", err)
	}
	fp := newFakePoller(activeBots)
	client, err := poller.NewClient(fp.server.URL, "")
	if err != nil {
		t.Fatalf("poller client: %v", err)
	}
	svc, err := NewBotLifecycleService(BotLifecycleConfig{
		Store:              store,
		Cipher:             cipher,
		Poller:             client,
		WebhookURL:         "http://web:8088/v1/im/webhook",
		RestoreConcurrency: 1,
	})
	if err != nil {
		t.Fatalf("lifecycle service: %v", err)
	}
	return svc.(*botLifecycleService), fp
}

func reconcileTestConfig(t *testing.T, cipher *secret.StaticKeyCipher, botUUID string, id int64) domain.ChannelConfig {
	t.Helper()
	ct, _, err := cipher.Encrypt([]byte(`{"token":"abc"}`))
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	return domain.ChannelConfig{
		ID:               id,
		BotUUID:          botUUID,
		ChannelType:      domain.ChannelWechat,
		OwnerID:          1,
		IlinkUserID:      "u-" + botUUID[:8],
		ConfigCiphertext: ct,
		ConfigKeyVersion: 1,
		State:            domain.ChannelActive,
	}
}

func TestReconcileBotsRestartsMissingBot(t *testing.T) {
	cipher, _ := secret.NewStaticKeyCipher(make([]byte, 32))
	a := reconcileTestConfig(t, cipher, "11111111-1111-4111-8111-111111111111", 1)
	b := reconcileTestConfig(t, cipher, "22222222-2222-4222-8222-222222222222", 2)
	store := newReconcileFakeStore([]domain.ChannelConfig{a, b})
	svc, fp := newReconcileHarness(t, store, []string{a.BotUUID}) // b missing on poller
	defer fp.close()

	if err := svc.ReconcileBots(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	starts, stops := fp.recorded()
	if len(starts) != 1 || starts[0] != b.BotUUID {
		t.Fatalf("expected /start for %s, got starts=%v", b.BotUUID, starts)
	}
	if len(stops) != 0 {
		t.Fatalf("unexpected stops: %v", stops)
	}
}

func TestReconcileBotsStopsStaleBot(t *testing.T) {
	cipher, _ := secret.NewStaticKeyCipher(make([]byte, 32))
	a := reconcileTestConfig(t, cipher, "11111111-1111-4111-8111-111111111111", 1)
	store := newReconcileFakeStore([]domain.ChannelConfig{a})
	svc, fp := newReconcileHarness(t, store, []string{
		a.BotUUID,
		"99999999-9999-4999-8999-999999999999", // zombie: not in DB
	})
	defer fp.close()

	if err := svc.ReconcileBots(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	starts, stops := fp.recorded()
	if len(starts) != 0 {
		t.Fatalf("unexpected starts: %v", starts)
	}
	if len(stops) != 1 || stops[0] != "99999999-9999-4999-8999-999999999999" {
		t.Fatalf("expected /stop for zombie, got stops=%v", stops)
	}
}

func TestReconcileBotsNoopWhenConverged(t *testing.T) {
	cipher, _ := secret.NewStaticKeyCipher(make([]byte, 32))
	a := reconcileTestConfig(t, cipher, "11111111-1111-4111-8111-111111111111", 1)
	store := newReconcileFakeStore([]domain.ChannelConfig{a})
	svc, fp := newReconcileHarness(t, store, []string{a.BotUUID})
	defer fp.close()

	if err := svc.ReconcileBots(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	starts, stops := fp.recorded()
	if len(starts) != 0 || len(stops) != 0 {
		t.Fatalf("expected no poller calls, got starts=%v stops=%v", starts, stops)
	}
}

// A config deactivated between List and re-check must not be resurrected.
func TestReconcileBotsSkipsDeactivatedBetweenListAndStart(t *testing.T) {
	cipher, _ := secret.NewStaticKeyCipher(make([]byte, 32))
	a := reconcileTestConfig(t, cipher, "11111111-1111-4111-8111-111111111111", 1)
	store := newReconcileFakeStore([]domain.ChannelConfig{a})
	// Simulate concurrent unbind: list snapshot says active, re-check says disabled.
	store.recheckState[a.BotUUID] = domain.ChannelDisabled
	svc, fp := newReconcileHarness(t, store, nil)
	defer fp.close()

	if err := svc.ReconcileBots(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	starts, stops := fp.recorded()
	if len(starts) != 0 {
		t.Fatalf("must not restart deactivated bot, got starts=%v", starts)
	}
	if len(stops) != 0 {
		t.Fatalf("unexpected stops: %v", stops)
	}
}

// A config whose row is deleted between List and re-check is no longer
// desired: skip silently (no error, no start), not a reconciliation failure.
func TestReconcileBotsSkipsRowDeletedBetweenListAndStart(t *testing.T) {
	cipher, _ := secret.NewStaticKeyCipher(make([]byte, 32))
	a := reconcileTestConfig(t, cipher, "11111111-1111-4111-8111-111111111111", 1)
	store := newReconcileFakeStore([]domain.ChannelConfig{a})
	store.recheckState[a.BotUUID] = "__deleted__" // sentinel: recheck returns ErrNoRows
	svc, fp := newReconcileHarness(t, store, nil)
	defer fp.close()

	if err := svc.ReconcileBots(context.Background()); err != nil {
		t.Fatalf("reconcile must not fail on deleted row, got: %v", err)
	}
	starts, stops := fp.recorded()
	if len(starts) != 0 || len(stops) != 0 {
		t.Fatalf("deleted row must not trigger poller calls, got starts=%v stops=%v", starts, stops)
	}
}

// A poller health failure must not touch any bot (no false stops).
func TestReconcileBotsHealthFailureIsSafe(t *testing.T) {
	cipher, _ := secret.NewStaticKeyCipher(make([]byte, 32))
	a := reconcileTestConfig(t, cipher, "11111111-1111-4111-8111-111111111111", 1)
	store := newReconcileFakeStore([]domain.ChannelConfig{a})
	svc, fp := newReconcileHarness(t, store, []string{a.BotUUID})
	defer fp.close()
	fp.mu.Lock()
	fp.healthErr = true
	fp.mu.Unlock()

	if err := svc.ReconcileBots(context.Background()); err == nil {
		t.Fatal("expected error when poller health fails")
	}
	starts, stops := fp.recorded()
	if len(starts) != 0 || len(stops) != 0 {
		t.Fatalf("health failure must not mutate bots, got starts=%v stops=%v", starts, stops)
	}
}

func TestReconcileBotsListFailure(t *testing.T) {
	store := newReconcileFakeStore(nil)
	store.listErr = errors.New("db down")
	svc, fp := newReconcileHarness(t, store, []string{"99999999-9999-4999-8999-999999999999"})
	defer fp.close()

	if err := svc.ReconcileBots(context.Background()); err == nil {
		t.Fatal("expected error when store list fails")
	}
	starts, stops := fp.recorded()
	if len(starts) != 0 || len(stops) != 0 {
		t.Fatalf("list failure must not mutate bots, got starts=%v stops=%v", starts, stops)
	}
}

// Multiple discrepancies are handled in one pass: a missing bot is restarted,
// a row-less zombie is stopped (stop forwarded, no cursor to persist), and a
// deactivated-but-running bot is stopped with cursor persisted.
func TestReconcileBotsMultipleDiscrepancies(t *testing.T) {
	cipher, _ := secret.NewStaticKeyCipher(make([]byte, 32))
	a := reconcileTestConfig(t, cipher, "11111111-1111-4111-8111-111111111111", 1)
	b := reconcileTestConfig(t, cipher, "22222222-2222-4222-8222-222222222222", 2)
	c := reconcileTestConfig(t, cipher, "33333333-3333-4333-8333-333333333333", 3)
	c.State = domain.ChannelDisabled // row exists but no longer active
	store := newReconcileFakeStore([]domain.ChannelConfig{a, b, c})
	store.recheckState[c.BotUUID] = domain.ChannelDisabled
	svc, fp := newReconcileHarness(t, store, []string{
		a.BotUUID,
		c.BotUUID,                              // zombie: still running on poller
		"99999999-9999-4999-8999-999999999999", // zombie: row deleted
	})
	defer fp.close()

	if err := svc.ReconcileBots(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	starts, stops := fp.recorded()
	if len(starts) != 1 || starts[0] != b.BotUUID {
		t.Fatalf("expected /start for %s, got starts=%v", b.BotUUID, starts)
	}
	if len(stops) != 2 {
		t.Fatalf("expected two /stop calls, got stops=%v", stops)
	}
	// Only the bot whose row still exists persists a cursor.
	if len(store.upserted) != 1 {
		t.Fatalf("expected exactly one cursor persist (row exists), upserted=%v", store.upserted)
	}
}
