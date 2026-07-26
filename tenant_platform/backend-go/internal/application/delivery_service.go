package application

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"time"

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
	return &deliveryService{cfg: cfg}, nil
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
	text, err := s.buildText(ctx, d, task)
	if err != nil {
		return s.deadLetter(ctx, d, "PAYLOAD_BUILD_FAILED", err.Error(), now)
	}
	sendCtx, cancel := context.WithTimeout(ctx, deliverySendTimeout)
	defer cancel()
	if err := s.cfg.Transport.SendMessage(sendCtx, bot.BotUUID, bot.IlinkUserID, text); err != nil {
		return s.retryOrDeadLetter(ctx, d, task, "SEND_FAILED", err.Error(), now)
	}
	// Audit the outbound reply. Failure is non-fatal: the message has been
	// sent, so we still ack the delivery. The alternative (not acking) would
	// cause a duplicate send on the next tick, which is worse than a missing
	// audit row. The log surfaces DB issues without blocking the user.
	if _, err := s.cfg.Messages.InsertOutboundMessage(ctx, domain.Message{
		UserID:      bot.OwnerID,
		BotID:       bot.ID,
		SessionKey:  personalSessionKey(bot.OwnerID),
		MessageType: domain.MessageTypeText,
		Content:     text,
		TaskID:      task.ID,
	}); err != nil {
		slog.ErrorContext(ctx, "delivery: audit outbound failed",
			"delivery_id", d.DeliveryID,
			"task_id", task.ID,
			"user_id", bot.OwnerID,
			"bot_id", bot.ID,
			"error", err)
	}
	return s.cfg.Store.MarkDeliveryAcked(ctx, d.DeliveryID, now)
}

func (s *deliveryService) buildText(ctx context.Context, d domain.Delivery, task domain.Task) (string, error) {
	switch d.DeliveryType {
	case domain.DeliveryTaskStarted:
		// Initial notification: let user know processing has begun
		return "🤖 正在处理您的任务...", nil
	case domain.DeliveryTaskComplete:
		if d.PayloadRef == "" {
			return "", errors.New("task_complete missing payload_ref")
		}
		payload, err := s.cfg.Results.ReadResult(ctx, d.PayloadRef, d.PayloadDigest)
		if err != nil {
			return "", err
		}
		body := truncateBytes(string(payload.Body), maxDeliveryTextBytes)
		return fmt.Sprintf("任务完成：\n%s", body), nil
	case domain.DeliveryTaskFailed:
		return fmt.Sprintf("任务失败：%s\n%s", d.ErrorCode, truncateBytes(d.ErrorMessage, maxDeliveryTextBytes)), nil
	case domain.DeliveryTaskCancelled:
		return fmt.Sprintf("任务已取消：%s", truncateBytes(d.ErrorMessage, maxDeliveryTextBytes)), nil
	case domain.DeliveryTaskInterrupted:
		return fmt.Sprintf("任务中断：%s", truncateBytes(d.ErrorMessage, maxDeliveryTextBytes)), nil
	default:
		return "", fmt.Errorf("unknown delivery type %s", d.DeliveryType)
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
