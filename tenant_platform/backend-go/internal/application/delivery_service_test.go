package application

import (
	"context"
	"errors"
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

func TestDeliveryServiceSendsWeComTaskCompleteToWeComBot(t *testing.T) {
	ctx, svc, deps := setupDeliveryService(t)
	deps.tasks.task = domain.Task{ID: "t1", RequesterID: 1, Source: domain.SourceWecom, ConversationKey: "chatid_1", TerminalAt: ptr(time.Now().UTC())}
	deps.bots.bot = domain.ChannelConfig{OwnerID: 1, BotUUID: "b1", ChannelType: domain.ChannelWecom, State: domain.ChannelActive}
	deps.results.payload = domain.ResultPayload{Ref: "ref:1", Digest: "sha256:a", Body: []byte("result body")}

	delivery := domain.Delivery{DeliveryID: "t1:task_complete", TaskID: "t1", DeliveryType: domain.DeliveryTaskComplete, PayloadRef: "ref:1", PayloadDigest: "sha256:a"}
	deps.store.pending = []domain.Delivery{delivery}

	if err := svc.tick(ctx); err != nil {
		t.Fatalf("tick: %v", err)
	}

	// 企微任务必须解析到企微渠道 bot(channelTypeForTaskSource 路由键)。
	if len(deps.transport.sent) != 1 {
		t.Fatalf("expected 1 sent, got %d (wecom task routed to wrong channel?)", len(deps.transport.sent))
	}
	if deps.transport.sent[0].BotUUID != "b1" {
		t.Fatalf("sent to %s, want b1", deps.transport.sent[0].BotUUID)
	}
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

	// 审查 R5-I3: 文件内容在成功事务时已快照入 task_delivery_files,
	// delivery 发送时直接使用快照, 不再解析 workspace 路径。
	deps.store.files = []domain.DeliveryFile{{
		Marker: "outputs/resume.docx", FileName: "resume.docx", RelPath: "outputs/resume.docx",
		Content: []byte("docx"), Digest: "sha256:6b25d6e95c3c4c6c7b0a2e6f7a6b5a4c3d2e1f0a9b8c7d6e5f4a3b2c1d0e9f8a7",
		SizeBytes: 4,
	}}
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
	// 审查 R5-I10: transport 收到的显示名必须是用户可见名, 而不是快照临时
	// 路径的 basename(含 marker hash 前缀)。
	if len(deps.transport.sentFiles) != 1 || deps.transport.sentFiles[0].FileName != "resume.docx" {
		t.Fatalf("file display name = %+v, want resume.docx", deps.transport.sentFiles)
	}
	if len(deps.messages.assets) != 1 || deps.messages.assets[0].Direction != domain.MessageOutbound {
		t.Fatalf("unexpected outbound assets: %+v", deps.messages.assets)
	}
}

func TestDeliveryServiceRetriesOnlyMissingFileAfterTextSucceeded(t *testing.T) {
	ctx, svc, deps := setupDeliveryService(t)
	deps.tasks.task = domain.Task{ID: "t1", SessionKey: "personal:1", RequesterID: 1, TerminalAt: ptr(time.Now().UTC())}
	deps.bots.bot = boundBot(1)

	// 审查 R5-I3: 文件内容在成功事务时已快照入 task_delivery_files,
	// delivery 发送时直接使用快照, 不再解析 workspace 路径。
	deps.store.files = []domain.DeliveryFile{{
		Marker: "outputs/resume.docx", FileName: "resume.docx", RelPath: "outputs/resume.docx",
		Content: []byte("docx"), Digest: "sha256:6b25d6e95c3c4c6c7b0a2e6f7a6b5a4c3d2e1f0a9b8c7d6e5f4a3b2c1d0e9f8a7",
		SizeBytes: 4,
	}}
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
	// 审查 R5-I3: 文件内容在成功事务时已快照入 task_delivery_files,
	// delivery 发送时直接使用快照, 不再解析 workspace 路径。
	deps.store.files = []domain.DeliveryFile{{
		Marker: "outputs/resume.docx", FileName: "resume.docx", RelPath: "outputs/resume.docx",
		Content: []byte("docx"), Digest: "sha256:6b25d6e95c3c4c6c7b0a2e6f7a6b5a4c3d2e1f0a9b8c7d6e5f4a3b2c1d0e9f8a7",
		SizeBytes: 4,
	}}
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
	deps.bots.bot = domain.ChannelConfig{OwnerID: 1, BotUUID: "b1", ChannelType: domain.ChannelWechat} // not bound

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
	membership   *fakeMembershipChecker
}

// fakeMembershipChecker 模拟团队成员资格检查(审查 R5-I4)。
type fakeMembershipChecker struct {
	approved map[string]bool // teamID -> approved
	// 审查 R5-I9: 模拟"检查通过后、发送前成员被移除"——从第 removeAfter 次
	// 调用起返回 false(0 表示不启用)。
	removeAfter int
	calls       int
}

func (f *fakeMembershipChecker) IsApprovedTeamMember(_ context.Context, teamID string, _ int64) (bool, error) {
	f.calls++
	if f.removeAfter > 0 && f.calls > f.removeAfter {
		return false, nil
	}
	return f.approved[teamID], nil
}

func setupDeliveryService(t *testing.T) (context.Context, *deliveryService, deliveryDeps) {
	t.Helper()
	root := t.TempDir()
	sessionFiles, err := NewSessionFiles(root, "")
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
		membership:   &fakeMembershipChecker{approved: map[string]bool{}},
	}
	svc, err := NewDeliveryService(DeliveryServiceConfig{
		Store:          deps.store,
		Tasks:          deps.tasks,
		Bots:           deps.bots,
		Transport:      deps.transport,
		Results:        deps.results,
		Messages:       deps.messages,
		TeamMembership: deps.membership,
		Now:            func() time.Time { return time.Now().UTC() },
	})
	if err != nil {
		t.Fatalf("new service: %v", err)
	}
	return context.Background(), svc.(*deliveryService), deps
}

func boundBot(ownerID int64) domain.ChannelConfig {
	return domain.ChannelConfig{
		OwnerID:     ownerID,
		BotUUID:     "b1",
		ChannelType: domain.ChannelWechat,
		IlinkUserID: "u1",
		State:       domain.ChannelActive,
	}
}

func ptr(t time.Time) *time.Time { return &t }

type fakeDeliveryStore struct {
	pending       []domain.Delivery
	acked         []string
	retries       []string
	deadLetters   []deadLetterRecord
	deadLetterErr error
	files         []domain.DeliveryFile // LoadDeliveryFiles 返回值(审查 R5-I3)
}

type deadLetterRecord struct {
	ID, Code, Message string
}

func (s *fakeDeliveryStore) LoadDeliveryFiles(_ context.Context, _ string) ([]domain.DeliveryFile, error) {
	return s.files, nil
}

func (s *fakeDeliveryStore) DeleteExpiredDeliveryFiles(_ context.Context, _ time.Time) (int64, error) {
	return 0, nil
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

func (s *fakeDeliveryStore) MarkDeliveryAcked(_ context.Context, deliveryID, _ string, _ time.Time) error {
	s.acked = append(s.acked, deliveryID)
	return nil
}

func (s *fakeDeliveryStore) MarkDeliveryRetry(_ context.Context, deliveryID, _ string, _ time.Time, _ time.Time) error {
	s.retries = append(s.retries, deliveryID)
	return nil
}

func (s *fakeDeliveryStore) MarkDeliveryDeadLetter(_ context.Context, deliveryID, _ string, errCode, errMessage string, _ time.Time) error {
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
	bot domain.ChannelConfig
	err error
}

func (r *fakeBotResolver) GetChannelConfigByOwnerAndType(_ context.Context, _ int64, channelType domain.ChannelType) (domain.ChannelConfig, error) {
	if r.err != nil {
		return domain.ChannelConfig{}, r.err
	}
	// 模拟真实 store 的 WHERE channel_type = ? 语义: 请求渠道与 bot 不匹配
	// 即视为未绑定(捕获 channelTypeForTaskSource 错投缺陷)。
	if r.bot.ChannelType != "" && r.bot.ChannelType != channelType {
		return domain.ChannelConfig{}, domain.ErrChannelBindingNotFound
	}
	return r.bot, nil
}

type fakeTransport struct {
	sent      []transportRecord
	sentFiles []fileTransportRecord
	err       error
	fileErr   error
}

type transportRecord struct {
	BotUUID, ChannelAccountID, Text, ClientID string
}

type fileTransportRecord struct {
	BotUUID, ChannelAccountID, FilePath, FileName, ClientID string
}

func (t *fakeTransport) SendMessage(_ context.Context, botUUID, ilinkUserID, text, clientID string) error {
	if t.err != nil {
		return t.err
	}
	t.sent = append(t.sent, transportRecord{BotUUID: botUUID, ChannelAccountID: ilinkUserID, Text: text, ClientID: clientID})
	return nil
}

func (t *fakeTransport) SendFile(_ context.Context, botUUID, ilinkUserID, filePath, fileName, clientID string) error {
	if t.fileErr != nil {
		return t.fileErr
	}
	if t.err != nil {
		return t.err
	}
	t.sentFiles = append(t.sentFiles, fileTransportRecord{BotUUID: botUUID, ChannelAccountID: ilinkUserID, FilePath: filePath, FileName: fileName, ClientID: clientID})
	return nil
}

func (t *fakeTransport) CheckMessageIdempotency(_ context.Context, _, _ string) (bool, error) {
	return true, nil
}

func (t *fakeTransport) MarkMessageIdempotency(_ context.Context, _, _ string) error {
	return nil
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

func (s *fakeMessageStore) DeleteInboundMessage(_ context.Context, _ int64, _ string) error { return nil }

func (s *fakeMessageStore) InsertInboundMessage(_ context.Context, m domain.Message) (domain.Message, error) {
	if s.inErr != nil {
		return domain.Message{}, s.inErr
	}
	s.inbound = append(s.inbound, m)
	return m, nil
}

func (s *fakeMessageStore) HasInboundMessage(_ context.Context, botID int64, messageID string) (bool, error) {
	if s.lookupErr != nil {
		return false, s.lookupErr
	}
	for _, m := range s.inbound {
		if m.BotID == botID && m.MessageID == messageID {
			return true, nil
		}
	}
	return false, nil
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

// TestDeliveryServiceRejectsRemovedTeamMember 验证审查 R5-I4: 团队任务的
// 发起人已不是 approved 成员时, 终端交付 dead-letter(MEMBER_REMOVED),
// 不得发送任务结果/文件。
func TestDeliveryServiceRejectsRemovedTeamMember(t *testing.T) {
	ctx, svc, deps := setupDeliveryService(t)
	deps.tasks.task = domain.Task{ID: "t1", SessionKey: "team:3b1f6a2e-9d4c-4f8e-9b2a-1c3d5e7f9a0b", RequesterID: 1, TerminalAt: ptr(time.Now().UTC())}
	deps.bots.bot = boundBot(1)
	deps.results.payload = domain.ResultPayload{Ref: "ref:1", Digest: "sha256:a", Body: []byte("done")}
	// 成员已被移除: 非 approved。
	deps.membership.approved["3b1f6a2e-9d4c-4f8e-9b2a-1c3d5e7f9a0b"] = false

	deps.store.pending = []domain.Delivery{{DeliveryID: "t1:task_complete", TaskID: "t1", DeliveryType: domain.DeliveryTaskComplete, PayloadRef: "ref:1", PayloadDigest: "sha256:a"}}
	if err := svc.tick(ctx); err != nil {
		t.Fatalf("tick: %v", err)
	}
	if len(deps.transport.sent) != 0 || len(deps.transport.sentFiles) != 0 {
		t.Fatalf("removed member must not receive task results: text=%+v files=%+v", deps.transport.sent, deps.transport.sentFiles)
	}
	if len(deps.store.deadLetters) != 1 || deps.store.deadLetters[0].Code != "MEMBER_REMOVED" {
		t.Fatalf("expected MEMBER_REMOVED dead-letter, got %+v", deps.store.deadLetters)
	}
	// 仍是 approved 成员时正常发送。
	deps.store.pending = []domain.Delivery{{DeliveryID: "t1:task_complete", TaskID: "t1", DeliveryType: domain.DeliveryTaskComplete, PayloadRef: "ref:1", PayloadDigest: "sha256:a"}}
	deps.membership.approved["3b1f6a2e-9d4c-4f8e-9b2a-1c3d5e7f9a0b"] = true
	if err := svc.tick(ctx); err != nil {
		t.Fatalf("tick: %v", err)
	}
	if len(deps.transport.sent) != 1 {
		t.Fatalf("approved member must receive task results, got %+v", deps.transport.sent)
	}
}

// 审查 R5-I9: 成员资格检查与外部发送之间仍有窗口——成员在 process 开头
// 检查通过后、发送前被移除时, 不得发出消息/文件(发送前再次检查)。
func TestDeliveryServiceRejectsMemberRemovedBetweenCheckAndSend(t *testing.T) {
	ctx, svc, deps := setupDeliveryService(t)
	deps.tasks.task = domain.Task{ID: "t1", SessionKey: "team:3b1f6a2e-9d4c-4f8e-9b2a-1c3d5e7f9a0b", RequesterID: 1, TerminalAt: ptr(time.Now().UTC())}
	deps.bots.bot = boundBot(1)
	deps.results.payload = domain.ResultPayload{Ref: "ref:1", Digest: "sha256:a", Body: []byte("done")}
	deps.membership.approved["3b1f6a2e-9d4c-4f8e-9b2a-1c3d5e7f9a0b"] = true
	// 第一次检查(process 开头)通过, 之后(发送前)视为已移除。
	deps.membership.removeAfter = 1

	deps.store.pending = []domain.Delivery{{DeliveryID: "t1:task_complete", TaskID: "t1", DeliveryType: domain.DeliveryTaskComplete, PayloadRef: "ref:1", PayloadDigest: "sha256:a"}}
	if err := svc.tick(ctx); err != nil {
		t.Fatalf("tick: %v", err)
	}
	if len(deps.transport.sent) != 0 {
		t.Fatalf("removed member must not receive text after send-time check, got %+v", deps.transport.sent)
	}
	if len(deps.store.deadLetters) != 1 || deps.store.deadLetters[0].Code != "MEMBER_REMOVED" {
		t.Fatalf("expected MEMBER_REMOVED dead-letter, got %+v", deps.store.deadLetters)
	}
}

// TestDeliveryServiceSnapshotFileNameCannotEscapeSnapshotDir 验证 DB 快照中
// 的 FileName 含路径穿越时(该值源于 Runner 可写的 manifest, 审查 C1),
// buildPayload 不得把快照写到 snapshotDir 之外(Platform 以自身权限
// WriteFile/Remove 的逃逸点), 且 displayName 必须被清洗为纯 basename。
func TestDeliveryServiceSnapshotFileNameCannotEscapeSnapshotDir(t *testing.T) {
	ctx, svc, deps := setupDeliveryService(t)
	deps.tasks.task = domain.Task{ID: "t1", RequesterID: 1, TerminalAt: ptr(time.Now().UTC())}
	deps.bots.bot = boundBot(1)
	deps.results.payload = domain.ResultPayload{Ref: "ref:1", Digest: "sha256:a", Body: []byte("see [FILE:outputs/evil.docx]")}
	deps.store.files = []domain.DeliveryFile{{
		Marker: "outputs/evil.docx", FileName: strings.Repeat("../", 12) + "pwned",
		RelPath: "outputs/evil.docx", Digest: "sha256:b", SizeBytes: 3, Content: []byte("abc"),
	}}
	deps.store.pending = []domain.Delivery{{DeliveryID: "t1:task_complete", TaskID: "t1", DeliveryType: domain.DeliveryTaskComplete, PayloadRef: "ref:1", PayloadDigest: "sha256:a"}}

	if err := svc.tick(ctx); err != nil {
		t.Fatalf("tick: %v", err)
	}
	if len(deps.transport.sentFiles) != 1 {
		t.Fatalf("expected 1 sent file, got %d", len(deps.transport.sentFiles))
	}
	sent := deps.transport.sentFiles[0]
	rel, err := filepath.Rel(svc.snapshotDir, sent.FilePath)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		t.Fatalf("snapshot path %q escaped snapshotDir %q (rel=%q)", sent.FilePath, svc.snapshotDir, rel)
	}
	if sent.FileName != "pwned" {
		t.Fatalf("displayName = %q, want sanitized basename pwned", sent.FileName)
	}
}

// TestDeliveryServiceSnapshotFileNameWindowsSlashCannotEscape 验证 FileName
// 含反斜杠路径分隔符(Windows 风格穿越)时同样被清洗, 不产生嵌套子路径。
func TestDeliveryServiceSnapshotFileNameWindowsSlashCannotEscape(t *testing.T) {
	ctx, svc, deps := setupDeliveryService(t)
	deps.tasks.task = domain.Task{ID: "t1", RequesterID: 1, TerminalAt: ptr(time.Now().UTC())}
	deps.bots.bot = boundBot(1)
	deps.results.payload = domain.ResultPayload{Ref: "ref:1", Digest: "sha256:a", Body: []byte("see [FILE:outputs/evil.docx]")}
	deps.store.files = []domain.DeliveryFile{{
		Marker: "outputs/evil.docx", FileName: strings.Repeat(`..\`, 12) + "pwned",
		RelPath: "outputs/evil.docx", Digest: "sha256:b", SizeBytes: 3, Content: []byte("abc"),
	}}
	deps.store.pending = []domain.Delivery{{DeliveryID: "t1:task_complete", TaskID: "t1", DeliveryType: domain.DeliveryTaskComplete, PayloadRef: "ref:1", PayloadDigest: "sha256:a"}}

	if err := svc.tick(ctx); err != nil {
		t.Fatalf("tick: %v", err)
	}
	if len(deps.transport.sentFiles) != 1 {
		t.Fatalf("expected 1 sent file, got %d", len(deps.transport.sentFiles))
	}
	sent := deps.transport.sentFiles[0]
	rel, err := filepath.Rel(svc.snapshotDir, sent.FilePath)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		t.Fatalf("snapshot path %q escaped snapshotDir %q (rel=%q)", sent.FilePath, svc.snapshotDir, rel)
	}
	if sent.FileName != "pwned" {
		t.Fatalf("displayName = %q, want sanitized basename pwned", sent.FileName)
	}
}

// TestSanitizeDeliverableDisplayNameStripsControlChars 验证显示名中的控制
// 字符(换行等)被剥离——该值流入消息 Content 与 MediaAsset.FileName。
func TestSanitizeDeliverableDisplayNameStripsControlChars(t *testing.T) {
	got := sanitizeDeliverableDisplayName("evil\r\nname.txt", "outputs/evil.txt")
	if got != "evilname.txt" {
		t.Fatalf("displayName = %q, want control chars stripped", got)
	}
	if got := deliverableSnapshotBase("outputs/.."); got != "deliverable.bin" {
		t.Fatalf("snapshot base of .. = %q, want deliverable.bin", got)
	}
}

// Round8 审查: 快照文件 key 的 8 字节哈希不得被 4 字节前缀碰撞击穿——
// outputs/d4561/report.docx 与 outputs/d36751/report.docx 在 4 字节前缀下
// 同为 73262a8d, 后写覆盖前写, 两个发送项读到同一份内容。
func TestDeliveryFileMarkerKeyRejectsShortCollision(t *testing.T) {
	a := deliveryFileMarkerKey("outputs/d4561/report.docx")
	b := deliveryFileMarkerKey("outputs/d36751/report.docx")
	if a == b {
		t.Fatalf("marker keys must not collide: both %q", a)
	}
	if len(a) != 16 {
		t.Fatalf("expected 16 hex chars (8 bytes), got %d (%q)", len(a), a)
	}
	if len(b) != 16 {
		t.Fatalf("expected 16 hex chars (8 bytes), got %d (%q)", len(b), b)
	}
}

// Round8 审查: 快照 basename 截断必须为 hash 前缀留出空间(16 hex + '_'),
// 超长 basename 回退 deliverable.bin 不得生成超限文件名。
func TestDeliverableSnapshotBaseLeavesRoomForPrefix(t *testing.T) {
	long := "x"
	for i := 0; i < 300; i++ {
		long += "x"
	}
	name := deliverableSnapshotBase("outputs/" + long + ".docx")
	if len(name) > 255 {
		t.Fatalf("snapshot base must fit 255-byte filename budget, got %d", len(name))
	}
	// 常规名不受影响。
	if got := deliverableSnapshotBase("outputs/report.docx"); got != "report.docx" {
		t.Fatalf("expected report.docx, got %q", got)
	}
}

func TestCleanIMMarkdownDowngradesWechatText(t *testing.T) {
	in := `任务完成：
Word 文档已生成成功！文件大小 **42 KB**，位于 ` + "`outputs/简历.docx`" + ` ✅

# 排版美化要点
- 📄 **封面标题** — 居中大标题
- 🔤 字体：正文等线、标题微软雅黑

| 等级 | 内容 |
|------|------|
| A | 技术基础 |

链接 [示例](https://example.com) 保留文本`
	want := `任务完成：
Word 文档已生成成功！文件大小 42 KB，位于 outputs/简历.docx ✅

排版美化要点
- 📄 封面标题 — 居中大标题
- 🔤 字体：正文等线、标题微软雅黑

等级 | 内容
A | 技术基础

链接 示例 保留文本`
	if got := cleanIMMarkdown(in); got != want {
		t.Errorf("cleanIMMarkdown mismatch\n got: %q\nwant: %q", got, want)
	}
}
