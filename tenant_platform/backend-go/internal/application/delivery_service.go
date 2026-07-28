package application

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode"

	"golang.org/x/sync/errgroup"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/domain"
	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/infrastructure/transport"
)

const (
	defaultDeliveryPollInterval = 2 * time.Second
	defaultDeliveryClaimLease   = 30 * time.Second
	defaultDeliveryRetryWindow  = 5 * time.Minute
	defaultDeliveryMaxBatch     = 8
	// maxDeliveryConcurrency caps in-flight process() calls within one tick.
	// Each call may block up to deliverySendTimeout on iLink send, so a batch
	// of 8 with concurrency 4 finishes in ~2 send-windows worst case instead
	// of 8. Per-user ordering is already enforced at task claim time
	// (session_sequence + workspace FOR UPDATE), so concurrent delivery here
	// is safe.
	maxDeliveryConcurrency = 4
	maxDeliveryTextBytes   = 4096
	minDeliveryBackoff     = time.Second
	maxDeliveryBackoff     = 5 * time.Minute
	deliverySendTimeout    = 15 * time.Second
	// maxDeliveryAttempts is a hard cap independent of the retry window. The
	// 5-minute window already bounds attempts to ~8-10 under exponential
	// backoff, but a clock anomaly or a task with NULL terminal_at (zero
	// deadline) would otherwise retry forever. Pattern: SQS maxReceiveCount.
	maxDeliveryAttempts = 10
)

// DeliveryStore is the persistence port for the terminal delivery outbox.
type DeliveryStore interface {
	ClaimPendingDeliveries(ctx context.Context, limit int, lease time.Duration, retryWindow time.Duration, now time.Time) ([]domain.Delivery, error)
	ResetStaleSendingDeliveries(ctx context.Context, now time.Time) (int64, error)
	DeadLetterExpiredDeliveries(ctx context.Context, retryWindow time.Duration, now time.Time) (int64, error)
	MarkDeliveryAcked(ctx context.Context, deliveryID string, ackedAt time.Time) error
	MarkDeliveryRetry(ctx context.Context, deliveryID string, nextAttemptAt time.Time, now time.Time) error
	MarkDeliveryDeadLetter(ctx context.Context, deliveryID string, errCode, errMessage string, terminalAt time.Time) error
}

// BotResolverByOwner locates the bot registered to a platform user.
type BotResolverByOwner interface {
	GetBotByOwner(ctx context.Context, ownerID int64) (domain.Bot, error)
}

// TaskReader loads task metadata for delivery addressing.
type TaskReader interface {
	GetTask(ctx context.Context, taskID string) (domain.Task, error)
}

// ResultReader reads the bounded committed result payload for task_complete deliveries.
type ResultReader interface {
	ReadResult(ctx context.Context, ref, digest string) (domain.ResultPayload, error)
}

// DeliveryService polls the outbox and sends terminal notifications via the
// configured BotTransportAdapter.
type DeliveryService interface {
	Run(ctx context.Context) error
	Recover(ctx context.Context) error
}

// DeliveryServiceConfig wires the delivery loop dependencies.
type DeliveryServiceConfig struct {
	Store        DeliveryStore
	Tasks        TaskReader
	Bots         BotResolverByOwner
	Transport    transport.BotTransportAdapter
	Results      ResultReader
	Messages     MessageStore
	SessionFiles SessionFiles
	PollInterval time.Duration
	ClaimLease   time.Duration
	RetryWindow  time.Duration
	MaxBatch     int
	Now          func() time.Time
}

func (cfg DeliveryServiceConfig) withDefaults() DeliveryServiceConfig {
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = defaultDeliveryPollInterval
	}
	if cfg.ClaimLease <= 0 {
		cfg.ClaimLease = defaultDeliveryClaimLease
	}
	if cfg.RetryWindow <= 0 {
		cfg.RetryWindow = defaultDeliveryRetryWindow
	}
	if cfg.MaxBatch <= 0 {
		cfg.MaxBatch = defaultDeliveryMaxBatch
	}
	if cfg.Now == nil {
		cfg.Now = func() time.Time { return time.Now().UTC() }
	}
	return cfg
}

type deliveryService struct {
	cfg DeliveryServiceConfig

	unjournaledMu    sync.Mutex
	unjournaledParts map[string]struct{}
}

// NewDeliveryService validates config and returns a runnable delivery service.
func NewDeliveryService(cfg DeliveryServiceConfig) (DeliveryService, error) {
	cfg = cfg.withDefaults()
	if cfg.Store == nil {
		return nil, errors.New("DeliveryStore is required")
	}
	if cfg.Tasks == nil {
		return nil, errors.New("TaskReader is required")
	}
	if cfg.Bots == nil {
		return nil, errors.New("BotResolverByOwner is required")
	}
	if cfg.Transport == nil {
		return nil, errors.New("Transport is required")
	}
	if cfg.Results == nil {
		return nil, errors.New("ResultReader is required")
	}
	if cfg.Messages == nil {
		return nil, errors.New("MessageStore is required")
	}
	return &deliveryService{
		cfg:              cfg,
		unjournaledParts: make(map[string]struct{}),
	}, nil
}

// Recover returns stuck sending rows to pending and dead-letters expired rows.
func (s *deliveryService) Recover(ctx context.Context) error {
	now := s.cfg.Now()
	if _, err := s.cfg.Store.ResetStaleSendingDeliveries(ctx, now); err != nil {
		return fmt.Errorf("reset stale sending: %w", err)
	}
	if _, err := s.cfg.Store.DeadLetterExpiredDeliveries(ctx, s.cfg.RetryWindow, now); err != nil {
		return fmt.Errorf("dead-letter expired: %w", err)
	}
	return nil
}

// Run polls the outbox until ctx is done.
func (s *deliveryService) Run(ctx context.Context) error {
	ticker := time.NewTicker(s.cfg.PollInterval)
	defer ticker.Stop()
	for {
		if err := s.tick(ctx); err != nil && !errors.Is(err, context.Canceled) {
			slog.ErrorContext(ctx, "delivery: tick error", "error", err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func (s *deliveryService) tick(ctx context.Context) error {
	now := s.cfg.Now()
	if err := s.Recover(ctx); err != nil {
		return err
	}
	deliveries, err := s.cfg.Store.ClaimPendingDeliveries(ctx, s.cfg.MaxBatch, s.cfg.ClaimLease, s.cfg.RetryWindow, now)
	if err != nil {
		return err
	}
	if len(deliveries) == 0 {
		return nil
	}
	// Process the batch concurrently. Each delivery targets a different user
	// (ClaimPendingDeliveries already SKIP LOCKED'd them across instances),
	// so cross-user parallelism is safe. Within a user, task-level ordering
	// is enforced at SubmitTask/ClaimNextTask via session_sequence + workspace
	// FOR UPDATE, so concurrent delivery here doesn't break per-user order.
	// Errors are logged per-delivery and swallowed so one failure doesn't
	// cancel the rest of the batch.
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(maxDeliveryConcurrency)
	for _, d := range deliveries {
		d := d
		g.Go(func() error {
			if err := s.process(gctx, d, now); err != nil && !errors.Is(err, context.Canceled) {
				slog.ErrorContext(gctx, "delivery: process failed",
					"delivery_id", d.DeliveryID,
					"task_id", d.TaskID,
					"error", err)
			}
			return nil
		})
	}
	return g.Wait()
}

func (s *deliveryService) process(ctx context.Context, d domain.Delivery, now time.Time) error {
	task, err := s.cfg.Tasks.GetTask(ctx, d.TaskID)
	if err != nil {
		return s.deadLetter(ctx, d, "TASK_LOOKUP_FAILED", err.Error(), now)
	}
	bot, err := s.cfg.Bots.GetBotByOwner(ctx, task.RequesterID)
	if err != nil {
		return s.deadLetter(ctx, d, "BOT_RESOLVE_FAILED", err.Error(), now)
	}
	if !bot.IsBound() {
		return s.deadLetter(ctx, d, "BOT_NOT_BOUND", "bot has no bound ilink user", now)
	}
	payload, err := s.buildPayload(ctx, d, task)
	if err != nil {
		return s.deadLetter(ctx, d, "PAYLOAD_BUILD_FAILED", err.Error(), now)
	}
	sendCtx, cancel := context.WithTimeout(ctx, deliverySendTimeout)
	defer cancel()
	if payload.Text != "" {
		partKey := d.DeliveryID + ":text"
		_, _, partErr := s.sendAndJournalPart(ctx, deliveryPart{
			key: partKey,
			message: domain.Message{
				UserID:      bot.OwnerID,
				BotID:       bot.ID,
				SessionKey:  personalSessionKey(bot.OwnerID),
				MessageType: domain.MessageTypeText,
				Content:     payload.Text,
				TaskID:      task.ID,
			},
			send: func() error {
				return s.cfg.Transport.SendMessage(sendCtx, bot.BotUUID, bot.IlinkUserID, payload.Text)
			},
			sendErrorCode: "SEND_FAILED",
		})
		if partErr != nil {
			return s.handleDeliveryPartError(ctx, d, task, partKey, partErr, now)
		}
	}
	for _, file := range payload.Files {
		partKey := d.DeliveryID + ":file:" + file.relPath
		msgRow, alreadySent, partErr := s.sendAndJournalPart(ctx, deliveryPart{
			key: partKey,
			message: domain.Message{
				UserID:      bot.OwnerID,
				BotID:       bot.ID,
				SessionKey:  personalSessionKey(bot.OwnerID),
				MessageType: domain.MessageTypeFile,
				Content:     file.displayName,
				MediaPath:   file.relPath,
				TaskID:      task.ID,
			},
			send: func() error {
				return s.cfg.Transport.SendFile(sendCtx, bot.BotUUID, bot.IlinkUserID, file.absPath)
			},
			sendErrorCode: "SEND_FILE_FAILED",
		})
		if partErr != nil {
			return s.handleDeliveryPartError(ctx, d, task, partKey, partErr, now)
		}
		if alreadySent {
			continue
		}
		if _, err := s.cfg.Messages.InsertMediaAsset(ctx, domain.MediaAsset{
			UserID:      bot.OwnerID,
			BotID:       bot.ID,
			MessageID:   msgRow.ID,
			FileName:    file.displayName,
			StoragePath: file.relPath,
			ContentType: "application/octet-stream",
			Direction:   domain.MessageOutbound,
		}); err != nil && !errors.Is(err, domain.ErrDuplicateMediaAsset) {
			slog.ErrorContext(ctx, "delivery: audit outbound media asset failed",
				"delivery_id", d.DeliveryID,
				"task_id", task.ID,
				"user_id", bot.OwnerID,
				"bot_id", bot.ID,
				"file", file.relPath,
				"error", err)
		}
	}
	return s.cfg.Store.MarkDeliveryAcked(ctx, d.DeliveryID, now)
}

type deliveryPart struct {
	key           string
	message       domain.Message
	send          func() error
	sendErrorCode string
}

type deliveryPartError struct {
	code string
	err  error
}

func (s *deliveryService) sendAndJournalPart(ctx context.Context, part deliveryPart) (domain.Message, bool, *deliveryPartError) {
	alreadyJournaled, err := s.cfg.Messages.HasOutboundMessage(
		ctx,
		part.message.TaskID,
		part.message.MessageType,
		part.message.Content,
		part.message.MediaPath,
	)
	if err != nil {
		return domain.Message{}, false, &deliveryPartError{code: "DELIVERY_PROGRESS_LOOKUP_FAILED", err: err}
	}
	if alreadyJournaled {
		s.clearUnjournaledPart(part.key)
		return domain.Message{}, true, nil
	}

	if !s.hasUnjournaledPart(part.key) {
		if err := part.send(); err != nil {
			return domain.Message{}, false, &deliveryPartError{code: part.sendErrorCode, err: err}
		}
		s.markUnjournaledPart(part.key)
	}

	msgRow, err := s.cfg.Messages.InsertOutboundMessage(ctx, part.message)
	if err != nil {
		return domain.Message{}, false, &deliveryPartError{code: "DELIVERY_PROGRESS_WRITE_FAILED", err: err}
	}
	s.clearUnjournaledPart(part.key)
	return msgRow, false, nil
}

func (s *deliveryService) handleDeliveryPartError(
	ctx context.Context,
	d domain.Delivery,
	task domain.Task,
	partKey string,
	partErr *deliveryPartError,
	now time.Time,
) error {
	if partErr.code == "DELIVERY_PROGRESS_WRITE_FAILED" {
		err := s.deadLetter(ctx, d, partErr.code, partErr.err.Error(), now)
		if err == nil {
			s.clearUnjournaledPart(partKey)
		}
		return err
	}
	return s.retryOrDeadLetter(ctx, d, task, partErr.code, partErr.err.Error(), now)
}

func (s *deliveryService) hasUnjournaledPart(key string) bool {
	s.unjournaledMu.Lock()
	defer s.unjournaledMu.Unlock()
	_, ok := s.unjournaledParts[key]
	return ok
}

func (s *deliveryService) markUnjournaledPart(key string) {
	s.unjournaledMu.Lock()
	defer s.unjournaledMu.Unlock()
	s.unjournaledParts[key] = struct{}{}
}

func (s *deliveryService) clearUnjournaledPart(key string) {
	s.unjournaledMu.Lock()
	defer s.unjournaledMu.Unlock()
	delete(s.unjournaledParts, key)
}

type deliveryFile struct {
	absPath     string
	relPath     string
	displayName string
}

type deliveryPayload struct {
	Text  string
	Files []deliveryFile
}

func (s *deliveryService) buildPayload(ctx context.Context, d domain.Delivery, task domain.Task) (deliveryPayload, error) {
	switch d.DeliveryType {
	case domain.DeliveryTaskStarted:
		return deliveryPayload{Text: "✓ 收到，正在处理您的任务..."}, nil
	case domain.DeliveryTaskComplete:
		if d.PayloadRef == "" {
			return deliveryPayload{}, errors.New("task_complete missing payload_ref")
		}
		payload, err := s.cfg.Results.ReadResult(ctx, d.PayloadRef, d.PayloadDigest)
		if err != nil {
			return deliveryPayload{}, err
		}
		body := userVisibleTaskResult(string(payload.Body))
		markers := extractFileMarkers(body)
		cleaned := stripFileMarkers(body)
		cleaned = truncateBytes(cleaned, maxDeliveryTextBytes)
		out := deliveryPayload{}
		if cleaned != "" {
			out.Text = fmt.Sprintf("任务完成：\n%s", cleaned)
		} else if len(markers) > 0 {
			out.Text = "任务完成，请查收文件。"
		} else {
			out.Text = "任务完成。"
		}
		if len(markers) == 0 {
			return out, nil
		}
		if s.cfg.SessionFiles == nil {
			return deliveryPayload{}, errors.New("session files manager is required for [FILE:] markers")
		}
		out.Files = make([]deliveryFile, 0, len(markers))
		for _, marker := range markers {
			absPath, relPath, err := s.cfg.SessionFiles.ResolveMarker(task.SessionKey, marker)
			if err != nil {
				return deliveryPayload{}, err
			}
			ref, err := s.cfg.SessionFiles.RecordOutbound(task.SessionKey, marker)
			if err != nil {
				return deliveryPayload{}, err
			}
			out.Files = append(out.Files, deliveryFile{absPath: absPath, relPath: relPath, displayName: ref.OriginalName})
		}
		return out, nil
	case domain.DeliveryTaskFailed:
		return deliveryPayload{Text: fmt.Sprintf("任务失败：%s\n%s", d.ErrorCode, truncateBytes(d.ErrorMessage, maxDeliveryTextBytes))}, nil
	case domain.DeliveryTaskCancelled:
		return deliveryPayload{Text: fmt.Sprintf("任务已取消：%s", truncateBytes(d.ErrorMessage, maxDeliveryTextBytes))}, nil
	case domain.DeliveryTaskInterrupted:
		return deliveryPayload{Text: fmt.Sprintf("任务中断：%s", truncateBytes(d.ErrorMessage, maxDeliveryTextBytes))}, nil
	default:
		return deliveryPayload{}, fmt.Errorf("unknown delivery type %s", d.DeliveryType)
	}
}

func (s *deliveryService) retryOrDeadLetter(ctx context.Context, d domain.Delivery, task domain.Task, code, message string, now time.Time) error {
	if d.AttemptCount >= maxDeliveryAttempts {
		return s.deadLetter(ctx, d, "MAX_ATTEMPTS_EXCEEDED",
			fmt.Sprintf("%s (after %d attempts)", message, d.AttemptCount), now)
	}
	deadline := retryDeadline(task, s.cfg.RetryWindow)
	next := nextRetryAt(d, now)
	if !deadline.IsZero() && next.After(deadline) {
		return s.deadLetter(ctx, d, code, message, now)
	}
	return s.cfg.Store.MarkDeliveryRetry(ctx, d.DeliveryID, next, now)
}

func (s *deliveryService) deadLetter(ctx context.Context, d domain.Delivery, code, message string, now time.Time) error {
	return s.cfg.Store.MarkDeliveryDeadLetter(ctx, d.DeliveryID, code, message, now)
}

func retryDeadline(task domain.Task, window time.Duration) time.Time {
	if task.TerminalAt == nil {
		return time.Time{}
	}
	return task.TerminalAt.Add(window)
}

func nextRetryAt(d domain.Delivery, now time.Time) time.Time {
	attempt := d.AttemptCount
	if attempt < 1 {
		attempt = 1
	}
	delay := time.Duration(math.Min(
		float64(minDeliveryBackoff)*math.Pow(2, float64(attempt-1)),
		float64(maxDeliveryBackoff),
	))
	return now.Add(delay)
}

func truncateBytes(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	return s[:limit]
}

var (
	turnMarkerLineRE           = regexp.MustCompile(`(?m)^\s*\*{0,2}LLM Running \(Turn \d+\) \.\.\.\*{0,2}\s*$`)
	hiddenTranscriptTagRE      = regexp.MustCompile(`(?is)<(?:thinking|summary|tool_use|file_content)>.*?</(?:thinking|summary|tool_use|file_content)>`)
	compactToolLineRE          = regexp.MustCompile(`^\s*🛠️\s+[A-Za-z_][A-Za-z0-9_]*\(.*$`)
	internalReasoningEnglishRE = regexp.MustCompile(`(?i)(the user is asking|let me\b|i should\b|actually\b|since there(?:'s| is)\b|i'?m just waiting for instructions)`)
)

func userVisibleTaskResult(raw string) string {
	turns := splitTranscriptTurns(raw)
	if len(turns) == 0 {
		turns = []string{raw}
	}
	for i := len(turns) - 1; i >= 0; i-- {
		cleaned := cleanTranscriptTurn(turns[i])
		if cleaned != "" {
			return cleaned
		}
	}
	fallback := strings.TrimSpace(raw)
	if fallback == "" {
		return "任务已完成"
	}
	return fallback
}

func splitTranscriptTurns(raw string) []string {
	normalized := strings.ReplaceAll(raw, "\r\n", "\n")
	if !turnMarkerLineRE.MatchString(normalized) {
		return nil
	}
	lines := strings.Split(normalized, "\n")
	turns := make([]string, 0, 4)
	buf := make([]string, 0, len(lines))
	flush := func() {
		joined := strings.TrimSpace(strings.Join(buf, "\n"))
		if joined != "" {
			turns = append(turns, joined)
		}
		buf = buf[:0]
	}
	for _, line := range lines {
		if turnMarkerLineRE.MatchString(line) {
			flush()
			continue
		}
		buf = append(buf, line)
	}
	flush()
	return turns
}

func cleanTranscriptTurn(turn string) string {
	normalized := hiddenTranscriptTagRE.ReplaceAllString(strings.ReplaceAll(turn, "\r\n", "\n"), "")
	lines := strings.Split(normalized, "\n")
	out := make([]string, 0, len(lines))
	skipVerboseTool := false
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		switch {
		case trimmed == "":
			if !skipVerboseTool {
				out = append(out, "")
			}
			skipVerboseTool = false
		case strings.HasPrefix(trimmed, "🛠️ Tool:"):
			skipVerboseTool = true
		case skipVerboseTool:
			continue
		case compactToolLineRE.MatchString(trimmed):
			continue
		case strings.HasPrefix(trimmed, "[Info]") || strings.HasPrefix(trimmed, "[Warn]") || strings.HasPrefix(trimmed, "[Error]"):
			continue
		default:
			out = append(out, line)
		}
	}
	cleaned := collapseBlankLines(strings.Join(out, "\n"))
	cleaned = trimLikelyInternalReasoningPrefix(cleaned)
	return strings.TrimSpace(cleaned)
}

func collapseBlankLines(s string) string {
	parts := strings.Split(s, "\n")
	out := make([]string, 0, len(parts))
	lastBlank := false
	for _, part := range parts {
		blank := strings.TrimSpace(part) == ""
		if blank {
			if lastBlank {
				continue
			}
			lastBlank = true
			out = append(out, "")
			continue
		}
		lastBlank = false
		out = append(out, part)
	}
	return strings.TrimSpace(strings.Join(out, "\n"))
}

func trimLikelyInternalReasoningPrefix(s string) string {
	firstCJK := -1
	for i, r := range s {
		if unicode.Is(unicode.Han, r) {
			firstCJK = i
			break
		}
	}
	if firstCJK <= 0 {
		return s
	}
	prefix := strings.TrimSpace(s[:firstCJK])
	if len(prefix) < 20 || !internalReasoningEnglishRE.MatchString(prefix) {
		return s
	}
	return strings.TrimSpace(s[firstCJK:])
}
