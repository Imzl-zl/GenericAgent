package application

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/domain"
)

func TestNewDeliveryServiceRequiresAllPorts(t *testing.T) {
	store := &fakeDeliveryStore{}
	tasks := &fakeTaskReader{}
	bots := &fakeBotResolver{}
	transport := &fakeTransport{}
	results := &fakeResultReader{}
	messages := &fakeMessageStore{}

	mustFail := func(name string, cfg DeliveryServiceConfig) {
		t.Helper()
		_, err := NewDeliveryService(cfg)
		if err == nil {
			t.Fatalf("%s: expected error", name)
		}
	}

	mustFail("missing store", DeliveryServiceConfig{Tasks: tasks, Bots: bots, Transport: transport, Results: results, Messages: messages})
	mustFail("missing tasks", DeliveryServiceConfig{Store: store, Bots: bots, Transport: transport, Results: results, Messages: messages})
	mustFail("missing bots", DeliveryServiceConfig{Store: store, Tasks: tasks, Transport: transport, Results: results, Messages: messages})
	mustFail("missing transport", DeliveryServiceConfig{Store: store, Tasks: tasks, Bots: bots, Results: results, Messages: messages})
	mustFail("missing results", DeliveryServiceConfig{Store: store, Tasks: tasks, Bots: bots, Transport: transport, Messages: messages})
	mustFail("missing messages", DeliveryServiceConfig{Store: store, Tasks: tasks, Bots: bots, Transport: transport, Results: results})
}

func TestDeliveryServiceAcksTaskComplete(t *testing.T) {
	ctx, svc, deps := setupDeliveryService(t)
	deps.tasks.task = domain.Task{ID: "t1", RequesterID: 1, TerminalAt: ptr(time.Now().UTC())}
	deps.bots.bot = boundBot(1)
	deps.results.payload = domain.ResultPayload{Ref: "ref:1", Digest: "sha256:a", Body: []byte("result body")}

	delivery := domain.Delivery{DeliveryID: "t1:task_complete", TaskID: "t1", DeliveryType: domain.DeliveryTaskComplete, PayloadRef: "ref:1", PayloadDigest: "sha256:a"}
	deps.store.pending = []domain.Delivery{delivery}

	if err := svc.tick(ctx); err != nil {
		t.Fatalf("tick: %v", err)
	}

	if len(deps.store.acked) != 1 || deps.store.acked[0] != delivery.DeliveryID {
		t.Fatalf("expected ack, got acked=%v", deps.store.acked)
	}
	if len(deps.transport.sent) != 1 {
		t.Fatalf("expected 1 sent, got %d", len(deps.transport.sent))
	}
	if deps.transport.sent[0].Text != "任务完成：\nresult body" {
		t.Fatalf("unexpected text: %q", deps.transport.sent[0].Text)
	}
}

func TestDeliveryServiceDeadLettersUnboundBot(t *testing.T) {
	ctx, svc, deps := setupDeliveryService(t)
	deps.tasks.task = domain.Task{ID: "t1", RequesterID: 1}
	deps.bots.bot = domain.Bot{OwnerID: 1, BotUUID: "b1"} // not bound

	deps.store.pending = []domain.Delivery{{DeliveryID: "t1:task_failed", TaskID: "t1", DeliveryType: domain.DeliveryTaskFailed, ErrorCode: "E", ErrorMessage: "fail"}}

	if err := svc.tick(ctx); err != nil {
		t.Fatalf("tick: %v", err)
	}

	if len(deps.store.deadLetters) != 1 {
		t.Fatalf("expected dead-letter, got %v", deps.store.deadLetters)
	}
	if deps.store.deadLetters[0].Code != "BOT_NOT_BOUND" {
		t.Fatalf("expected BOT_NOT_BOUND, got %s", deps.store.deadLetters[0].Code)
	}
}

func TestDeliveryServiceRetriesThenDeadLetters(t *testing.T) {
	ctx, svc, deps := setupDeliveryService(t)
	terminalAt := time.Now().UTC().Add(-time.Minute)
	deps.tasks.task = domain.Task{ID: "t1", RequesterID: 1, TerminalAt: &terminalAt}
	deps.bots.bot = boundBot(1)
	deps.transport.err = errors.New("ilink down")

	deps.store.pending = []domain.Delivery{{DeliveryID: "t1:task_complete", TaskID: "t1", DeliveryType: domain.DeliveryTaskComplete, PayloadRef: "ref:1", PayloadDigest: "sha256:a", AttemptCount: 10}}

	if err := svc.tick(ctx); err != nil {
		t.Fatalf("tick: %v", err)
	}

	if len(deps.store.retries) != 0 {
		t.Fatalf("expected no retry after deadline, got %v", deps.store.retries)
	}
	if len(deps.store.deadLetters) != 1 {
		t.Fatalf("expected dead-letter, got %v", deps.store.deadLetters)
	}
}

func TestDeliveryServiceRetriesWithinWindow(t *testing.T) {
	ctx, svc, deps := setupDeliveryService(t)
	terminalAt := time.Now().UTC().Add(10 * time.Minute)
	deps.tasks.task = domain.Task{ID: "t1", RequesterID: 1, TerminalAt: &terminalAt}
	deps.bots.bot = boundBot(1)
	deps.transport.err = errors.New("ilink down")

	deps.store.pending = []domain.Delivery{{DeliveryID: "t1:task_complete", TaskID: "t1", DeliveryType: domain.DeliveryTaskComplete, PayloadRef: "ref:1", PayloadDigest: "sha256:a", AttemptCount: 1}}

	if err := svc.tick(ctx); err != nil {
		t.Fatalf("tick: %v", err)
	}

	if len(deps.store.retries) != 1 {
		t.Fatalf("expected retry, got %v", deps.store.retries)
	}
	if len(deps.store.deadLetters) != 0 {
		t.Fatalf("unexpected dead-letter: %v", deps.store.deadLetters)
	}
}

// --- fakes ---

type deliveryDeps struct {
	store     *fakeDeliveryStore
	tasks     *fakeTaskReader
	bots      *fakeBotResolver
	transport *fakeTransport
	results   *fakeResultReader
	messages  *fakeMessageStore
}

func setupDeliveryService(t *testing.T) (context.Context, *deliveryService, deliveryDeps) {
	t.Helper()
	deps := deliveryDeps{
		store:     &fakeDeliveryStore{},
		tasks:     &fakeTaskReader{},
		bots:      &fakeBotResolver{},
		transport: &fakeTransport{},
		results:   &fakeResultReader{},
		messages:  &fakeMessageStore{},
	}
	svc, err := NewDeliveryService(DeliveryServiceConfig{
		Store:     deps.store,
		Tasks:     deps.tasks,
		Bots:      deps.bots,
		Transport: deps.transport,
		Results:   deps.results,
		Messages:  deps.messages,
		Now:       func() time.Time { return time.Now().UTC() },
	})
	if err != nil {
		t.Fatalf("new service: %v", err)
	}
	return context.Background(), svc.(*deliveryService), deps
}

func boundBot(ownerID int64) domain.Bot {
	return domain.Bot{OwnerID: ownerID, BotUUID: "b1", IlinkUserID: "u1"}
}

func ptr(t time.Time) *time.Time { return &t }

type fakeDeliveryStore struct {
	pending     []domain.Delivery
	acked       []string
	retries     []string
	deadLetters []deadLetterRecord
}

type deadLetterRecord struct {
	ID, Code, Message string
}

func (s *fakeDeliveryStore) ClaimPendingDeliveries(_ context.Context, limit int, _ time.Duration, _ time.Duration, _ time.Time) ([]domain.Delivery, error) {
	out := s.pending
	if len(out) > limit {
		out = out[:limit]
	}
	s.pending = nil
	return out, nil
}

func (s *fakeDeliveryStore) ResetStaleSendingDeliveries(_ context.Context, _ time.Time) (int64, error) { return 0, nil }
func (s *fakeDeliveryStore) DeadLetterExpiredDeliveries(_ context.Context, _ time.Duration, _ time.Time) (int64, error) {
	return 0, nil
}

func (s *fakeDeliveryStore) MarkDeliveryAcked(_ context.Context, deliveryID string, _ time.Time) error {
	s.acked = append(s.acked, deliveryID)
	return nil
}

func (s *fakeDeliveryStore) MarkDeliveryRetry(_ context.Context, deliveryID string, _ time.Time, _ time.Time) error {
	s.retries = append(s.retries, deliveryID)
	return nil
}

func (s *fakeDeliveryStore) MarkDeliveryDeadLetter(_ context.Context, deliveryID string, errCode, errMessage string, _ time.Time) error {
	s.deadLetters = append(s.deadLetters, deadLetterRecord{ID: deliveryID, Code: errCode, Message: errMessage})
	return nil
}

type fakeTaskReader struct {
	task domain.Task
	err  error
}

func (r *fakeTaskReader) GetTask(_ context.Context, _ string) (domain.Task, error) {
	return r.task, r.err
}

type fakeBotResolver struct {
	bot domain.Bot
	err error
}

func (r *fakeBotResolver) GetBotByOwner(_ context.Context, _ int64) (domain.Bot, error) {
	return r.bot, r.err
}

type fakeTransport struct {
	sent []transportRecord
	err  error
}

type transportRecord struct {
	BotUUID, IlinkUserID, Text string
}

func (t *fakeTransport) SendMessage(_ context.Context, botUUID, ilinkUserID, text string) error {
	if t.err != nil {
		return t.err
	}
	t.sent = append(t.sent, transportRecord{BotUUID: botUUID, IlinkUserID: ilinkUserID, Text: text})
	return nil
}

func (t *fakeTransport) RecordMessageIdempotency(_ context.Context, _, _ string) (bool, error) {
	return true, nil
}

type fakeResultReader struct {
	payload domain.ResultPayload
	err     error
}

func (r *fakeResultReader) ReadResult(_ context.Context, _, _ string) (domain.ResultPayload, error) {
	return r.payload, r.err
}

type fakeMessageStore struct {
	inbound  []domain.Message
	outbound []domain.Message
	assets   []domain.MediaAsset
	inErr    error
	outErr   error
	assetErr error
}

func (s *fakeMessageStore) InsertInboundMessage(_ context.Context, m domain.Message) (domain.Message, error) {
	if s.inErr != nil {
		return domain.Message{}, s.inErr
	}
	s.inbound = append(s.inbound, m)
	return m, nil
}

func (s *fakeMessageStore) InsertOutboundMessage(_ context.Context, m domain.Message) (domain.Message, error) {
	if s.outErr != nil {
		return domain.Message{}, s.outErr
	}
	s.outbound = append(s.outbound, m)
	return m, nil
}

func (s *fakeMessageStore) InsertMediaAsset(_ context.Context, m domain.MediaAsset) (domain.MediaAsset, error) {
	if s.assetErr != nil {
		return domain.MediaAsset{}, s.assetErr
	}
	s.assets = append(s.assets, m)
	return m, nil
}
