package application

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
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

func TestDeliveryServiceFormatsStartedDeliveryAsSingleAckMessage(t *testing.T) {
	ctx, svc, deps := setupDeliveryService(t)
	deps.tasks.task = domain.Task{ID: "t1", RequesterID: 1}
	deps.bots.bot = boundBot(1)

	delivery := domain.Delivery{DeliveryID: "t1:task_started", TaskID: "t1", DeliveryType: domain.DeliveryTaskStarted}
	deps.store.pending = []domain.Delivery{delivery}

	if err := svc.tick(ctx); err != nil {
		t.Fatalf("tick: %v", err)
	}

	if len(deps.transport.sent) != 1 {
		t.Fatalf("expected 1 sent, got %d", len(deps.transport.sent))
	}
	if deps.transport.sent[0].Text != "✓ 收到，正在处理您的任务..." {
		t.Fatalf("unexpected text: %q", deps.transport.sent[0].Text)
	}
}

func TestDeliveryServiceStripsInternalTranscriptFromTaskComplete(t *testing.T) {
	ctx, svc, deps := setupDeliveryService(t)
	deps.tasks.task = domain.Task{ID: "t1", RequesterID: 1, TerminalAt: ptr(time.Now().UTC())}
	deps.bots.bot = boundBot(1)
	deps.results.payload = domain.ResultPayload{
		Ref:    "ref:1",
		Digest: "sha256:a",
		Body:   []byte("\n**LLM Running (Turn 1) ...**\n\n<summary>无活跃任务，检查工作记忆与上下文</summary>\n我正在等待你的指令！\n🛠️ update_working_checkpoint(无活跃任务...)\n\n**LLM Running (Turn 2) ...**\n\n我正闲着等你派活呢！😄\n\n"),
	}

	delivery := domain.Delivery{DeliveryID: "t1:task_complete", TaskID: "t1", DeliveryType: domain.DeliveryTaskComplete, PayloadRef: "ref:1", PayloadDigest: "sha256:a"}
	deps.store.pending = []domain.Delivery{delivery}

	if err := svc.tick(ctx); err != nil {
		t.Fatalf("tick: %v", err)
	}

	if len(deps.transport.sent) != 1 {
		t.Fatalf("expected 1 sent, got %d", len(deps.transport.sent))
	}
	if deps.transport.sent[0].Text != "任务完成：\n我正闲着等你派活呢！😄" {
		t.Fatalf("unexpected text: %q", deps.transport.sent[0].Text)
	}
}

func TestDeliveryServiceSendsFilesFromResultMarkers(t *testing.T) {
	ctx, svc, deps := setupDeliveryService(t)
	deps.tasks.task = domain.Task{ID: "t1", SessionKey: "personal:1", RequesterID: 1, TerminalAt: ptr(time.Now().UTC())}
	deps.bots.bot = boundBot(1)

	root := deps.sessionFiles.SandboxRoot("personal:1")
	path := filepath.Join(root, "outputs", "resume.docx")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte("docx"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	deps.results.payload = domain.ResultPayload{
		Ref:    "ref:1",
		Digest: "sha256:a",
		Body:   []byte("整理好了，请查收。\n[FILE:outputs/resume.docx]"),
	}

	delivery := domain.Delivery{DeliveryID: "t1:task_complete", TaskID: "t1", DeliveryType: domain.DeliveryTaskComplete, PayloadRef: "ref:1", PayloadDigest: "sha256:a"}
	deps.store.pending = []domain.Delivery{delivery}

	if err := svc.tick(ctx); err != nil {
		t.Fatalf("tick: %v", err)
	}
	if len(deps.transport.sent) != 1 || deps.transport.sent[0].Text != "任务完成：\n整理好了，请查收。" {
		t.Fatalf("unexpected text deliveries: %+v", deps.transport.sent)
	}
	if len(deps.transport.sentFiles) != 1 || !strings.HasSuffix(filepath.Base(deps.transport.sentFiles[0].FilePath), "resume.docx") {
		t.Fatalf("unexpected file deliveries: %+v", deps.transport.sentFiles)
	}
	if len(deps.messages.assets) != 1 || deps.messages.assets[0].Direction != domain.MessageOutbound {
		t.Fatalf("unexpected outbound assets: %+v", deps.messages.assets)
	}
}

func TestDeliveryServiceRetriesOnlyMissingFileAfterTextSucceeded(t *testing.T) {
	ctx, svc, deps := setupDeliveryService(t)
	deps.tasks.task = domain.Task{ID: "t1", SessionKey: "personal:1", RequesterID: 1, TerminalAt: ptr(time.Now().UTC())}
	deps.bots.bot = boundBot(1)

	root := deps.sessionFiles.SandboxRoot("personal:1")
	path := filepath.Join(root, "outputs", "resume.docx")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte("docx"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	deps.results.payload = domain.ResultPayload{
		Ref:    "ref:1",
		Digest: "sha256:a",
		Body:   []byte("整理好了，请查收。\n[FILE:outputs/resume.docx]"),
	}
	delivery := domain.Delivery{
		DeliveryID: "t1:task_complete", TaskID: "t1", DeliveryType: domain.DeliveryTaskComplete,
		PayloadRef: "ref:1", PayloadDigest: "sha256:a", AttemptCount: 1,
	}
	deps.transport.fileErr = errors.New("getuploadurl failed")
	deps.store.pending = []domain.Delivery{delivery}

	if err := svc.tick(ctx); err != nil {
		t.Fatalf("first tick: %v", err)
	}
	if len(deps.transport.sent) != 1 || len(deps.transport.sentFiles) != 0 {
		t.Fatalf("first attempt should send text once and fail the file: text=%+v files=%+v", deps.transport.sent, deps.transport.sentFiles)
	}
	if len(deps.messages.outbound) != 1 || deps.messages.outbound[0].MessageType != domain.MessageTypeText {
		t.Fatalf("successful text part must be journaled: %+v", deps.messages.outbound)
	}
	if len(deps.store.retries) != 1 {
		t.Fatalf("file failure should keep the delivery retryable: %+v", deps.store.retries)
	}

	deps.transport.fileErr = nil
	delivery.AttemptCount = 2
	deps.store.pending = []domain.Delivery{delivery}
	if err := svc.tick(ctx); err != nil {
		t.Fatalf("second tick: %v", err)
	}
	if len(deps.transport.sent) != 1 {
		t.Fatalf("second attempt must not repeat completed text: %+v", deps.transport.sent)
	}
	if len(deps.transport.sentFiles) != 1 || !strings.HasSuffix(filepath.Base(deps.transport.sentFiles[0].FilePath), "resume.docx") {
		t.Fatalf("second attempt should send only the missing file: %+v", deps.transport.sentFiles)
	}
	if len(deps.store.acked) != 1 || deps.store.acked[0] != delivery.DeliveryID {
		t.Fatalf("delivery should ack after missing file succeeds: %+v", deps.store.acked)
	}
}

func TestDeliveryServiceDeadLettersWhenSentTextCannotBeJournaled(t *testing.T) {
	ctx, svc, deps := setupDeliveryService(t)
	deps.tasks.task = domain.Task{ID: "t1", SessionKey: "personal:1", RequesterID: 1, TerminalAt: ptr(time.Now().UTC())}
	deps.bots.bot = boundBot(1)
	deps.results.payload = domain.ResultPayload{Ref: "ref:1", Digest: "sha256:a", Body: []byte("done")}
	deps.messages.insertErr = errors.New("database unavailable")
	deps.store.pending = []domain.Delivery{{
		DeliveryID: "t1:task_complete", TaskID: "t1", DeliveryType: domain.DeliveryTaskComplete,
		PayloadRef: "ref:1", PayloadDigest: "sha256:a", AttemptCount: 1,
	}}

	if err := svc.tick(ctx); err != nil {
		t.Fatalf("tick: %v", err)
	}
	if len(deps.transport.sent) != 1 {
		t.Fatalf("expected text to reach carrier once, got %+v", deps.transport.sent)
	}
	if len(deps.store.retries) != 0 || len(deps.store.acked) != 0 {
		t.Fatalf("untracked carrier send must not retry or ack: retries=%v acked=%v", deps.store.retries, deps.store.acked)
	}
	if len(deps.store.deadLetters) != 1 || deps.store.deadLetters[0].Code != "DELIVERY_PROGRESS_WRITE_FAILED" {
		t.Fatalf("expected explicit progress-write dead letter, got %+v", deps.store.deadLetters)
	}
}

func TestDeliveryServiceReconcilesCarrierSuccessAfterTransientDatabaseFailure(t *testing.T) {
	ctx, svc, deps := setupDeliveryService(t)
	deps.tasks.task = domain.Task{ID: "t1", SessionKey: "personal:1", RequesterID: 1, TerminalAt: ptr(time.Now().UTC())}
	deps.bots.bot = boundBot(1)
	deps.results.payload = domain.ResultPayload{Ref: "ref:1", Digest: "sha256:a", Body: []byte("done")}
	delivery := domain.Delivery{
		DeliveryID: "t1:task_complete", TaskID: "t1", DeliveryType: domain.DeliveryTaskComplete,
		PayloadRef: "ref:1", PayloadDigest: "sha256:a", AttemptCount: 1,
	}
	deps.messages.insertErr = errors.New("database unavailable")
	deps.store.deadLetterErr = errors.New("database unavailable")
	deps.store.pending = []domain.Delivery{delivery}

	if err := svc.tick(ctx); err != nil {
		t.Fatalf("first tick: %v", err)
	}
	if len(deps.transport.sent) != 1 {
		t.Fatalf("expected first carrier send, got %+v", deps.transport.sent)
	}

	deps.messages.insertErr = nil
	deps.store.deadLetterErr = nil
	delivery.AttemptCount = 2
	deps.store.pending = []domain.Delivery{delivery}
	if err := svc.tick(ctx); err != nil {
		t.Fatalf("second tick: %v", err)
	}
	if len(deps.transport.sent) != 1 {
		t.Fatalf("database recovery must journal prior carrier success without resending: %+v", deps.transport.sent)
	}
	if len(deps.messages.outbound) != 1 || deps.messages.outbound[0].Content != "任务完成：\ndone" {
		t.Fatalf("expected recovered progress journal: %+v", deps.messages.outbound)
	}
	if len(deps.store.acked) != 1 {
		t.Fatalf("expected delivery ack after progress reconciliation: %+v", deps.store.acked)
	}
}

func TestDeliveryServiceReconcilesCarrierFileSuccessAfterTransientDatabaseFailure(t *testing.T) {
	ctx, svc, deps := setupDeliveryService(t)
	deps.tasks.task = domain.Task{ID: "t1", SessionKey: "personal:1", RequesterID: 1, TerminalAt: ptr(time.Now().UTC())}
	deps.bots.bot = boundBot(1)
	root := deps.sessionFiles.SandboxRoot("personal:1")
	path := filepath.Join(root, "outputs", "resume.docx")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte("docx"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	deps.results.payload = domain.ResultPayload{
		Ref: "ref:1", Digest: "sha256:a", Body: []byte("done\n[FILE:outputs/resume.docx]"),
	}
	delivery := domain.Delivery{
		DeliveryID: "t1:task_complete", TaskID: "t1", DeliveryType: domain.DeliveryTaskComplete,
		PayloadRef: "ref:1", PayloadDigest: "sha256:a", AttemptCount: 1,
	}
	deps.messages.insertErrByType = map[string]error{domain.MessageTypeFile: errors.New("database unavailable")}
	deps.store.deadLetterErr = errors.New("database unavailable")
	deps.store.pending = []domain.Delivery{delivery}

	if err := svc.tick(ctx); err != nil {
		t.Fatalf("first tick: %v", err)
	}
	if len(deps.transport.sent) != 1 || len(deps.transport.sentFiles) != 1 {
		t.Fatalf("expected text and file to reach carrier once: text=%+v files=%+v", deps.transport.sent, deps.transport.sentFiles)
	}

	deps.messages.insertErrByType = nil
	deps.store.deadLetterErr = nil
	delivery.AttemptCount = 2
	deps.store.pending = []domain.Delivery{delivery}
	if err := svc.tick(ctx); err != nil {
		t.Fatalf("second tick: %v", err)
	}
	if len(deps.transport.sent) != 1 || len(deps.transport.sentFiles) != 1 {
		t.Fatalf("database recovery must not resend carrier-accepted parts: text=%+v files=%+v", deps.transport.sent, deps.transport.sentFiles)
	}
	if len(deps.messages.outbound) != 2 || deps.messages.outbound[1].MessageType != domain.MessageTypeFile {
		t.Fatalf("expected recovered file progress journal: %+v", deps.messages.outbound)
	}
	if len(deps.store.acked) != 1 {
		t.Fatalf("expected delivery ack after file progress reconciliation: %+v", deps.store.acked)
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
	store        *fakeDeliveryStore
	tasks        *fakeTaskReader
	bots         *fakeBotResolver
	transport    *fakeTransport
	results      *fakeResultReader
	messages     *fakeMessageStore
	sessionFiles SessionFiles
}

func setupDeliveryService(t *testing.T) (context.Context, *deliveryService, deliveryDeps) {
	t.Helper()
	root := t.TempDir()
	sessionFiles, err := NewSessionFiles(root)
	if err != nil {
		t.Fatalf("new session files: %v", err)
	}
	deps := deliveryDeps{
		store:        &fakeDeliveryStore{},
		tasks:        &fakeTaskReader{},
		bots:         &fakeBotResolver{},
		transport:    &fakeTransport{},
		results:      &fakeResultReader{},
		messages:     &fakeMessageStore{},
		sessionFiles: sessionFiles,
	}
	svc, err := NewDeliveryService(DeliveryServiceConfig{
		Store:        deps.store,
		Tasks:        deps.tasks,
		Bots:         deps.bots,
		Transport:    deps.transport,
		Results:      deps.results,
		Messages:     deps.messages,
		SessionFiles: deps.sessionFiles,
		Now:          func() time.Time { return time.Now().UTC() },
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
	pending       []domain.Delivery
	acked         []string
	retries       []string
	deadLetters   []deadLetterRecord
	deadLetterErr error
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

func (s *fakeDeliveryStore) ResetStaleSendingDeliveries(_ context.Context, _ time.Time) (int64, error) {
	return 0, nil
}
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
	if s.deadLetterErr != nil {
		return s.deadLetterErr
	}
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
	sent      []transportRecord
	sentFiles []fileTransportRecord
	err       error
	fileErr   error
}

type transportRecord struct {
	BotUUID, IlinkUserID, Text string
}

type fileTransportRecord struct {
	BotUUID, IlinkUserID, FilePath string
}

func (t *fakeTransport) SendMessage(_ context.Context, botUUID, ilinkUserID, text string) error {
	if t.err != nil {
		return t.err
	}
	t.sent = append(t.sent, transportRecord{BotUUID: botUUID, IlinkUserID: ilinkUserID, Text: text})
	return nil
}

func (t *fakeTransport) SendFile(_ context.Context, botUUID, ilinkUserID, filePath string) error {
	if t.fileErr != nil {
		return t.fileErr
	}
	if t.err != nil {
		return t.err
	}
	t.sentFiles = append(t.sentFiles, fileTransportRecord{BotUUID: botUUID, IlinkUserID: ilinkUserID, FilePath: filePath})
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
	inbound         []domain.Message
	outbound        []domain.Message
	assets          []domain.MediaAsset
	inErr           error
	lookupErr       error
	insertErr       error
	insertErrByType map[string]error
	assetErr        error
}

func (s *fakeMessageStore) InsertInboundMessage(_ context.Context, m domain.Message) (domain.Message, error) {
	if s.inErr != nil {
		return domain.Message{}, s.inErr
	}
	s.inbound = append(s.inbound, m)
	return m, nil
}

func (s *fakeMessageStore) InsertOutboundMessage(_ context.Context, m domain.Message) (domain.Message, error) {
	if err := s.insertErrByType[m.MessageType]; err != nil {
		return domain.Message{}, err
	}
	if s.insertErr != nil {
		return domain.Message{}, s.insertErr
	}
	m.ID = int64(len(s.outbound) + 1)
	s.outbound = append(s.outbound, m)
	return m, nil
}

func (s *fakeMessageStore) HasOutboundMessage(_ context.Context, taskID, messageType, content, mediaPath string) (bool, error) {
	if s.lookupErr != nil {
		return false, s.lookupErr
	}
	for _, m := range s.outbound {
		if m.TaskID == taskID && m.MessageType == messageType && m.Content == content && m.MediaPath == mediaPath {
			return true, nil
		}
	}
	return false, nil
}

func (s *fakeMessageStore) InsertMediaAsset(_ context.Context, m domain.MediaAsset) (domain.MediaAsset, error) {
	if s.assetErr != nil {
		return domain.MediaAsset{}, s.assetErr
	}
	s.assets = append(s.assets, m)
	return m, nil
}
