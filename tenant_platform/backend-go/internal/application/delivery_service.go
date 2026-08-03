package application

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"os"
	"path/filepath"
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
	// deliveryFilesRetention 是 task_delivery_files 快照的审计保留期
	// (审查 R5-I3: 内容随 outbox 保留, 定期清理防无界增长)。
	deliveryFilesRetention = 30 * 24 * time.Hour
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
	// LoadDeliveryFiles 返回 task_complete delivery 绑定的输出文件快照
	// (审查 R5-I3: 成功事务时捕获, 发送时不再解析 workspace 路径)。
	LoadDeliveryFiles(ctx context.Context, deliveryID string) ([]domain.DeliveryFile, error)
	// DeleteExpiredDeliveryFiles 删除超过保留期的 delivery 文件快照。
	DeleteExpiredDeliveryFiles(ctx context.Context, before time.Time) (int64, error)
}

// BotResolverByOwner locates the bot registered to a platform user.
type BotResolverByOwner interface {
	GetBotByOwner(ctx context.Context, ownerID int64) (domain.Bot, error)
}

// TaskReader loads task metadata for delivery addressing.
type TaskReader interface {
	GetTask(ctx context.Context, taskID string) (domain.Task, error)
}

// TeamMembershipChecker 校验任务发起人是否仍是被授权的团队成员
// (审查 R5-I4: 成员移除后, 既有任务的成功文件/结果不得发送给已失权成员)。
type TeamMembershipChecker interface {
	IsApprovedTeamMember(ctx context.Context, teamID string, userID int64) (bool, error)
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
	// TeamMembership 非 nil 时, 团队任务的终端交付前校验发起人成员资格
	// (审查 R5-I4: 已移除成员不得再收到团队任务结果)。
	TeamMembership TeamMembershipChecker
	PollInterval   time.Duration
	ClaimLease     time.Duration
	RetryWindow    time.Duration
	MaxBatch       int
	Now            func() time.Time
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
	// snapshotDir 是 Platform 私有文件快照目录(发送前不可变副本)。
	snapshotDir string
	// lastFileCleanup 记录上次 delivery 文件快照清理时间(节流, 审查 R5-I3)。
	lastFileCleanup time.Time
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
	snapshotDir, err := os.MkdirTemp("", "ga-delivery-*")
	if err != nil {
		return nil, fmt.Errorf("create deliverable snapshot dir: %w", err)
	}
	return &deliveryService{
		cfg:              cfg,
		unjournaledParts: make(map[string]struct{}),
		snapshotDir:      snapshotDir,
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
	// 审查 R5-I3: 定期清理超过保留期的 delivery 文件快照(审计保留,
	// 防无界增长)。节流到每 24 小时一次。
	if time.Since(s.lastFileCleanup) > 24*time.Hour {
		s.lastFileCleanup = now
		if n, err := s.cfg.Store.DeleteExpiredDeliveryFiles(ctx, now.Add(-deliveryFilesRetention)); err != nil {
			slog.ErrorContext(ctx, "delivery: delete expired delivery files failed", "error", err)
		} else if n > 0 {
			slog.InfoContext(ctx, "delivery: deleted expired delivery file snapshots", "count", n)
		}
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

// errDeliveryMemberRemoved 是发送前成员资格复查失败的哨兵错误(审查 R5-I9):
// 直接死信 MEMBER_REMOVED, 不重试(成员移除是永久状态)。
var errDeliveryMemberRemoved = errors.New("requester is no longer an approved team member")

func (s *deliveryService) process(ctx context.Context, d domain.Delivery, now time.Time) error {
	task, err := s.cfg.Tasks.GetTask(ctx, d.TaskID)
	if err != nil {
		return s.deadLetter(ctx, d, "TASK_LOOKUP_FAILED", err.Error(), now)
	}
	// 审查 R5-I4: 团队任务的发起人在交付前必须仍是 approved 成员——成员被
	// 移除后, 其既有任务(可能已被移除时取消请求, 但终端交付仍可能因竞态
	// 存在)的成功文件/结果不得发送给已失权成员。
	var teamID string
	if s.cfg.TeamMembership != nil {
		if tid, ok := teamSessionKey(task.SessionKey); ok {
			teamID = tid
			approved, err := s.cfg.TeamMembership.IsApprovedTeamMember(ctx, tid, task.RequesterID)
			if err != nil {
				return s.deadLetter(ctx, d, "TEAM_MEMBERSHIP_CHECK_FAILED", err.Error(), now)
			}
			if !approved {
				return s.deadLetter(ctx, d, "MEMBER_REMOVED", "requester is no longer an approved team member", now)
			}
		}
	}
	// 审查 R5-I9: 开头检查与外部发送之间仍有窗口——成员在窗口内被移除时
	// 不得发出消息/文件。发送前再次校验; 失败走 MEMBER_REMOVED 死信。
	assertMemberAtSend := func() error {
		if s.cfg.TeamMembership == nil || teamID == "" {
			return nil
		}
		approved, err := s.cfg.TeamMembership.IsApprovedTeamMember(ctx, teamID, task.RequesterID)
		if err != nil {
			return err
		}
		if !approved {
			return errDeliveryMemberRemoved
		}
		return nil
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
				UserID: bot.OwnerID,
				BotID:  bot.ID,
				// 审查: 出站消息记录到任务所属 session(个人或团队), 而不是
				// 硬编码个人 session——团队任务的出站消息此前被错误记入
				// 个人会话。
				SessionKey:  task.SessionKey,
				MessageType: domain.MessageTypeText,
				Content:     payload.Text,
				TaskID:      task.ID,
			},
			send: func() error {
				if err := assertMemberAtSend(); err != nil {
					return err
				}
				return s.cfg.Transport.SendMessage(sendCtx, bot.BotUUID, bot.IlinkUserID, payload.Text)
			},
			sendErrorCode: "SEND_FAILED",
		})
		if partErr != nil {
			if errors.Is(partErr.err, errDeliveryMemberRemoved) {
				return s.deadLetterMemberRemoved(ctx, d, partKey, partErr.err, now)
			}
			return s.handleDeliveryPartError(ctx, d, task, partKey, partErr, now)
		}
	}
	for _, file := range payload.Files {
		// 审查 R5-I3: 发送结束后删除 buildPayload 写入的私有临时文件
		// (顺带清理空子目录)。
		defer func() {
			_ = os.Remove(file.absPath)
			_ = os.Remove(filepath.Dir(file.absPath))
		}()
		partKey := d.DeliveryID + ":file:" + file.auditPath
		msgRow, alreadySent, partErr := s.sendAndJournalPart(ctx, deliveryPart{
			key: partKey,
			message: domain.Message{
				UserID:      bot.OwnerID,
				BotID:       bot.ID,
				SessionKey:  task.SessionKey,
				MessageType: domain.MessageTypeFile,
				Content:     file.displayName,
				MediaPath:   file.auditPath,
				TaskID:      task.ID,
			},
			send: func() error {
				if err := assertMemberAtSend(); err != nil {
					return err
				}
				// 审查 R5-I3: DB 快照文件的内容来自成功事务的安全捕获
				// (safefs 限长读取 + 普通文件校验), tmp 位于 Platform 私有
				// 目录(0700), 直接发送, 无需二次快照。
				if file.snapshotContent {
					return s.cfg.Transport.SendFile(sendCtx, bot.BotUUID, bot.IlinkUserID, file.absPath, file.displayName)
				}
				// 安全发送(方案 §6): 打开校验(O_NOFOLLOW + fstat + 大小上限)
				// 后复制到 Platform 私有快照, transport 发送不可变快照,
				// 消除校验与发送之间的 TOCTOU。
				snap, snapErr := snapshotDeliverable(file.absPath, file.root, file.relPath, s.snapshotDir, defaultMaxDeliverableBytes)
				if snapErr != nil {
					return snapErr
				}
				defer os.Remove(snap)
				return s.cfg.Transport.SendFile(sendCtx, bot.BotUUID, bot.IlinkUserID, snap, file.displayName)
			},
			sendErrorCode: "SEND_FILE_FAILED",
		})
		if partErr != nil {
			if errors.Is(partErr.err, errDeliveryMemberRemoved) {
				return s.deadLetterMemberRemoved(ctx, d, partKey, partErr.err, now)
			}
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
			StoragePath: file.auditPath,
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

func (s *deliveryService) deadLetterMemberRemoved(ctx context.Context, d domain.Delivery, partKey string, sendErr error, now time.Time) error {
	err := s.deadLetter(ctx, d, "MEMBER_REMOVED", sendErr.Error(), now)
	if err == nil {
		s.clearUnjournaledPart(partKey)
	}
	return err
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
	absPath         string // 可发送文件的完整路径(Platform 私有快照)
	root            string // absPath 的受限根(OpenBeneath 用)
	relPath         string // root 相对路径(OpenBeneath 用)
	displayName     string
	auditPath       string // workspace 内相对路径(消息媒体审计, 审查 R5-I3)
	snapshotContent bool   // true = 内容来自成功事务捕获的 DB 快照, 直接发送
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
		// 审查 R5-I3: 文件内容在任务成功事务时已快照入 task_delivery_files
		// (与成功状态原子提交); 发送时直接使用快照, 不再重新解析 workspace
		// 路径——同 Runner 下一条串行任务可能已覆盖/删除同名输出。
		files, err := s.cfg.Store.LoadDeliveryFiles(ctx, d.DeliveryID)
		if err != nil {
			return deliveryPayload{}, fmt.Errorf("load delivery files: %w", err)
		}
		if len(files) == 0 {
			return out, nil
		}
		if len(markers) > 0 && cleaned == "" {
			out.Text = "任务完成，请查收文件。"
		}
		out.Files = make([]deliveryFile, 0, len(files))
		for _, f := range files {
			// 快照内容写入 Platform 私有临时文件(发送后删除)。文件名保留
			// 用户可见名(transport 以路径 basename 作为显示名), 子目录按
			// delivery 隔离避免并发同名覆盖; marker 哈希前缀区分同 basename
			// 的不同输出文件(如 outputs/a.docx 与 outputs/sub/a.docx)。
			dir := filepath.Join(s.snapshotDir, deliveryFileKey(d.DeliveryID))
			if err := os.MkdirAll(dir, 0o700); err != nil {
				return deliveryPayload{}, fmt.Errorf("create delivery file dir: %w", err)
			}
			tmpPath := filepath.Join(dir, fmt.Sprintf("%s_%s", deliveryFileMarkerKey(f.Marker), f.FileName))
			if err := os.WriteFile(tmpPath, f.Content, 0o600); err != nil {
				return deliveryPayload{}, fmt.Errorf("write delivery file snapshot %q: %w", f.Marker, err)
			}
			out.Files = append(out.Files, deliveryFile{
				absPath: tmpPath, root: dir, relPath: filepath.Base(tmpPath),
				displayName: f.FileName, auditPath: f.RelPath, snapshotContent: true,
			})
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

// teamSessionKey 解析 team:<uuid> 形式的 session key; 非团队 key 返回 ok=false。
func teamSessionKey(sessionKey string) (string, bool) {
	if !strings.HasPrefix(sessionKey, "team:") {
		return "", false
	}
	id := strings.TrimPrefix(sessionKey, "team:")
	if id == "" || strings.ContainsAny(id, ":/\\\x00") {
		return "", false
	}
	return id, true
}

func truncateBytes(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	return s[:limit]
}

// deliveryFileKey 把 delivery_id(含 ':' 等非文件名字符)转为安全文件名前缀。
func deliveryFileKey(deliveryID string) string {
	replacer := strings.NewReplacer(":", "_", "/", "_", "\\", "_", "..", "_")
	return replacer.Replace(deliveryID)
}

// deliveryFileMarkerKey 返回 marker 的 8 位哈希前缀(区分同 basename 文件)。
func deliveryFileMarkerKey(marker string) string {
	sum := sha256.Sum256([]byte(marker))
	return hex.EncodeToString(sum[:4])
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
