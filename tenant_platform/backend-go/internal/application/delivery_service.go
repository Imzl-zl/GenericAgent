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

	"github.com/google/uuid"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/domain"
	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/infrastructure/transport"
)

const (
	defaultDeliveryPollInterval = 2 * time.Second
	defaultDeliveryClaimLease   = 30 * time.Second
	defaultDeliveryRetryWindow  = 30 * time.Minute
	defaultDeliveryMaxBatch     = 8
	// deliveryFilesRetention 是 task_delivery_files 快照的审计保留期
	// (审查 R5-I3: 内容随 outbox 保留, 定期清理防无界增长)。
	deliveryFilesRetention = 30 * 24 * time.Hour
	// mediaAssetsRetention 是 media_assets 审计行保留期(2026-08-13 审查
	// I4/D7: 媒体字节=用户隐私数据; 90d 与 poller media_root 清扫对齐)。
	mediaAssetsRetention = 90 * 24 * time.Hour
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
	// deliveryMediaSendTimeout 是媒体(文件/图片/视频)单次发送预算(2026-08-14
	// 生产事故修复: 微信生图交付死信)。iLink CDN 上传(US→微信 CDN, AES 加密
	// 整文件)正常 1-5s, 但连接层瞬时故障时旧逻辑挂到 read timeout(120s×3
	// 次)才失败——远超文本 15s 预算, 8 次重试全部撞同一窗口 → 死信。现在
	// poller 侧连接超时 10s 快速失败(退避 1/3s, 最坏 ~33s), 此处给媒体
	// 90s 预算: 快速失败路径完全落在预算内, 慢传输也有足够余量。
	deliveryMediaSendTimeout = 90 * time.Second
	// maxDeliveryAttempts is a hard cap independent of the retry window. The
	// 30-minute window already bounds attempts under exponential backoff, but a
	// clock anomaly or a task with NULL terminal_at (zero deadline) would
	// otherwise retry forever. Pattern: SQS maxReceiveCount.
	maxDeliveryAttempts = 10
)

// DeliveryStore is the persistence port for the terminal delivery outbox.
type DeliveryStore interface {
	ClaimPendingDeliveries(ctx context.Context, limit int, lease time.Duration, retryWindow time.Duration, now time.Time) ([]domain.Delivery, error)
	ResetStaleSendingDeliveries(ctx context.Context, now time.Time) (int64, error)
	DeadLetterExpiredDeliveries(ctx context.Context, retryWindow time.Duration, now time.Time) (int64, error)
	MarkDeliveryAcked(ctx context.Context, deliveryID, attemptToken string, ackedAt time.Time) error
	MarkDeliveryRetry(ctx context.Context, deliveryID, attemptToken string, nextAttemptAt time.Time, now time.Time) error
	MarkDeliveryDeadLetter(ctx context.Context, deliveryID, attemptToken string, errCode, errMessage string, terminalAt time.Time) error
	// LoadDeliveryFiles 返回 task_complete delivery 绑定的输出文件快照
	// (审查 R5-I3: 成功事务时捕获, 发送时不再解析 workspace 路径)。
	LoadDeliveryFiles(ctx context.Context, deliveryID string) ([]domain.DeliveryFile, error)
	// DeleteExpiredDeliveryFiles 删除超过保留期的 delivery 文件快照。
	DeleteExpiredDeliveryFiles(ctx context.Context, before time.Time) (int64, error)
}

// ChannelResolverByOwner locates the channel config a task's reply should go
// through (IM_CHANNEL_BINDING §6: 回复按任务 Source 渠道路由; 非渠道来源
// 回退微信)。
type ChannelResolverByOwner interface {
	GetChannelConfigByOwnerAndType(ctx context.Context, ownerID int64, channelType domain.ChannelType) (domain.ChannelConfig, error)
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

// channelTypeForTaskSource 映射任务来源到回复渠道: IM 渠道任务回来源渠道,
// 其余(web/未知)回退微信(与既有行为一致)。
func channelTypeForTaskSource(source string) domain.ChannelType {
	switch source {
	case domain.SourceFeishu:
		return domain.ChannelFeishu
	case domain.SourceDingTalk:
		return domain.ChannelDingTalk
	case domain.SourceQQ:
		return domain.ChannelQQ
	case domain.SourceWecom:
		return domain.ChannelWecom
	default:
		return domain.ChannelWechat
	}
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
	Bots         ChannelResolverByOwner
	Transport    transport.BotTransportAdapter
	Results      ResultReader
	Messages     MessageStore
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
	// round9 审查: 交付快照目录从 Platform 私有 MkdirTemp 改为部署共享的
	// delivery spool(GA_DELIVERY_SPOOL_DIR, compose 中 Platform rw / Bot
	// Poller ro 挂载同一卷)——旧实现把快照写在 Platform /tmp(独立 tmpfs),
	// 而 Poller 在另一容器按绝对路径读文件, DOCX/PDF/XLSX 交付必然失败。
	// 未配置 env(单元测试/loopback)时保持私有临时目录。
	// 2026-08-13 审查 B4/T5: 同一 spool 目录承载捕获期文件引用
	// (capture/ 子目录, scheduler 侧写入)——delivery 与 capture 必须同卷。
	snapshotDir, err := ResolveDeliverySpoolDir()
	if err != nil {
		return nil, err
	}
	return &deliveryService{
		cfg:              cfg,
		unjournaledParts: make(map[string]struct{}),
		snapshotDir:      snapshotDir,
	}, nil
}

// resolveDeliverySpoolDir 解析 delivery spool 共享卷根(GA_DELIVERY_SPOOL_DIR)。
// 空 env(单元测试/loopback)回退 Platform 私有临时目录。scheduler 捕获侧与
// delivery 发送侧共用同一目录(compose 同卷挂载)。
func ResolveDeliverySpoolDir() (string, error) {
	snapshotDir := strings.TrimSpace(os.Getenv("GA_DELIVERY_SPOOL_DIR"))
	var err error
	if snapshotDir == "" {
		snapshotDir, err = os.MkdirTemp("", "ga-delivery-*")
		if err != nil {
			return "", fmt.Errorf("create deliverable snapshot dir: %w", err)
		}
	} else if err := os.MkdirAll(snapshotDir, 0o2770); err != nil {
		return "", fmt.Errorf("create delivery spool dir %s: %w", snapshotDir, err)
	}
	return snapshotDir, nil
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
		// 2026-08-13 审查 B4/T5: spool 引用文件(捕获期持久快照)与 DB 行
		// 同保留期, 按 mtime 清扫 capture/ 子目录(DB 行删除后文件独立过期)。
		if n := cleanupSpoolCaptureDir(s.snapshotDir, now.Add(-deliveryFilesRetention)); n > 0 {
			slog.InfoContext(ctx, "delivery: cleaned expired spool capture files", "count", n)
		}
		// 2026-08-13 审查 I4/D7: 媒体审计行(media_assets, 入站/出站)90d
		// 保留期——媒体字节=用户隐私数据, 审计明细不无限积累。
		if s.cfg.Messages != nil {
			if n, err := s.cfg.Messages.DeleteExpiredMediaAssets(ctx, now.Add(-mediaAssetsRetention)); err != nil {
				slog.ErrorContext(ctx, "delivery: delete expired media assets failed", "error", err)
			} else if n > 0 {
				slog.InfoContext(ctx, "delivery: deleted expired media assets", "count", n)
			}
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

// removePayloadFiles 删除 buildPayload 物化的全部 spool 快照与空子目录
// (round12 审查 I6): 幂等, 文件不存在视为成功。process 在 buildPayload
// 成功后立即 defer 调用——文本/前序文件发送失败时其余快照同样清理。
func removePayloadFiles(payload deliveryPayload) {
	for _, f := range payload.Files {
		// 2026-08-13 审查 B4/T5: 捕获期 spool 文件(spoolPath 非空)是任务
		// 成功事务绑定的持久快照, 必须保留(重试/审计), 由 30d mtime 清扫
		// 回收——这里只清理 buildPayload 本次物化的临时副本。
		if f.spoolPath != "" {
			continue
		}
		_ = os.Remove(f.absPath)
		_ = os.Remove(filepath.Dir(f.absPath))
	}
}

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
	// 回复渠道 = 任务来源渠道(IM_CHANNEL_BINDING §6); 非渠道来源(web 等)
	// 回退微信。
	channelType := channelTypeForTaskSource(task.Source)
	bot, err := s.cfg.Bots.GetChannelConfigByOwnerAndType(ctx, task.RequesterID, channelType)
	if err != nil {
		return s.deadLetter(ctx, d, "BOT_RESOLVE_FAILED", err.Error(), now)
	}
	if !bot.IsBound() {
		return s.deadLetter(ctx, d, "BOT_NOT_BOUND", "channel config is not bound", now)
	}
	// 回复目标: 微信=绑定 ilink_user_id; 新渠道=任务对话单元(conversation_id)。
	replyTarget := bot.IlinkUserID
	if bot.ChannelType != domain.ChannelWechat {
		replyTarget = task.ConversationKey
		if replyTarget == "" {
			return s.deadLetter(ctx, d, "NO_REPLY_TARGET", "task has no conversation key for channel reply", now)
		}
	}
	payload, err := s.buildPayload(ctx, d, task)
	if err != nil {
		return s.deadLetter(ctx, d, "PAYLOAD_BUILD_FAILED", err.Error(), now)
	}
	// round12 审查(I6): 清理所有权覆盖整个 process——buildPayload 成功后
	// 立即注册统一清理, 文本发送失败/前序文件发送失败时其余 spool 快照
	// 不会残留(旧实现只在逐文件循环内注册 defer)。
	defer removePayloadFiles(payload)
	sendCtx, cancel := context.WithTimeout(ctx, deliverySendTimeout)
	defer cancel()
	// IM 流式交付(IM_STREAMING_DELIVERY §4.2): 任务流式回复已 commit 成功
	// (stream_final_at 非空)时, 最终文本已在流式消息中交付, 跳过文本 part
	// 防重复; 文件 part 照发。失败/未流式路径无标记 → 文本照发兜底。
	if payload.Text != "" && task.StreamFinalAt == nil {
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
				return s.cfg.Transport.SendMessage(sendCtx, bot.BotUUID, replyTarget, payload.Text, deliveryClientID(partKey))
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
					mediaCtx, mcancel := context.WithTimeout(ctx, deliveryMediaSendTimeout)
					defer mcancel()
					return s.cfg.Transport.SendFile(mediaCtx, bot.BotUUID, replyTarget, file.absPath, file.displayName, deliveryClientID(partKey), mediaTypeForPath(file.relPath))
				}
				// 安全发送(方案 §6): 打开校验(O_NOFOLLOW + fstat + 大小上限)
				// 后复制到 Platform 私有快照, transport 发送不可变快照,
				// 消除校验与发送之间的 TOCTOU。
				snap, snapErr := snapshotDeliverable(file.absPath, file.root, file.relPath, s.snapshotDir, defaultMaxDeliverableBytes)
				if snapErr != nil {
					return snapErr
				}
				defer os.Remove(snap)
				mediaCtx, mcancel := context.WithTimeout(ctx, deliveryMediaSendTimeout)
				defer mcancel()
				return s.cfg.Transport.SendFile(mediaCtx, bot.BotUUID, replyTarget, snap, file.displayName, deliveryClientID(partKey), mediaTypeForPath(file.relPath))
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
	return s.cfg.Store.MarkDeliveryAcked(ctx, d.DeliveryID, d.AttemptToken, now)
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
	absPath         string // 可发送文件的完整路径(Platform 私有快照或捕获期 spool)
	root            string // absPath 的受限根(OpenBeneath 用)
	relPath         string // root 相对路径(OpenBeneath 用)
	displayName     string
	auditPath       string // workspace 内相对路径(消息媒体审计, 审查 R5-I3)
	snapshotContent bool   // true = 内容来自成功事务捕获, 直接发送
	spoolPath       string // 非空 = 捕获期 spool 引用(保留, 不随 payload 清理)
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
		cleaned := cleanIMMarkdown(stripFileMarkers(body))
		cleaned = domain.TruncateUTF8(cleaned, maxDeliveryTextBytes)
		out := deliveryPayload{}
		if cleaned != "" {
			out.Text = fmt.Sprintf("任务完成：\n%s", cleaned)
		} else if len(markers) > 0 {
			out.Text = "任务完成，请查收文件。"
		} else {
			// 空结果防护(2026-08-12 生产实证): 上游模型退化响应(仅
			// summary/thinking 或空白)被 GA 当正常完成时结果体为空——
			// 不再用"任务完成。"伪装成正常交付, 明确提示用户可重试。
			// (worker 侧 EMPTY_RESULT 失败路径已拦大部分; 此处兜底历史
			// 任务/流式失败等其余空体成功场景。)
			out.Text = "任务已完成，但模型没有返回可显示的内容(可能是上游模型异常)，请重试。"
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
			if f.SpoolPath != "" {
				// spool 引用(2026-08-13 审查 B4/T5): 文件内容在任务成功事务时
				// 已流式复制到共享卷(Platform rw / Poller ro), 发送前 Lstat
				// 校验普通文件 + 类型上限(防卷被篡改, 纵深防御)。
				// 2026-08-14 审查 S2: SpoolPath 虽由构造保证安全(deliveryFileKey
				// 清洗 + marker hash), 仍对 DB 读出的值做 Clean + 逃逸前缀校验
				// (防污染行/误写把读取引到 spool 根外)。
				clean := filepath.Clean(filepath.FromSlash(f.SpoolPath))
				if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) || filepath.IsAbs(clean) {
					removePayloadFiles(out)
					return deliveryPayload{}, fmt.Errorf("spool file %q escapes spool root", f.SpoolPath)
				}
				spoolAbs := filepath.Join(s.snapshotDir, clean)
				info, err := os.Lstat(spoolAbs)
				if err != nil {
					removePayloadFiles(out)
					return deliveryPayload{}, fmt.Errorf("stat spool file %q: %w", f.SpoolPath, err)
				}
				if !info.Mode().IsRegular() {
					removePayloadFiles(out)
					return deliveryPayload{}, fmt.Errorf("spool file %q is not a regular file", f.SpoolPath)
				}
				if info.Size() > deliverableMaxBytes(f.RelPath) {
					removePayloadFiles(out)
					return deliveryPayload{}, fmt.Errorf("spool file %q exceeds size limit", f.SpoolPath)
				}
				out.Files = append(out.Files, deliveryFile{
					absPath:         spoolAbs,
					displayName:     sanitizeDeliverableDisplayName(f.FileName, f.RelPath),
					auditPath:       f.RelPath,
					snapshotContent: true,
					spoolPath:       f.SpoolPath,
				})
				continue
			}
			// 存量行(content 快照, 30d 保留期内): 写入 Platform 私有临时文件
			// (发送后删除)。文件名用服务端可信的 relPath basename(审查 C1:
			// FileName 源于 Runner 可写的 manifest, 不得进入路径拼接), 子目录
			// 按 delivery 隔离避免并发同名覆盖; marker 哈希前缀区分同 basename
			// 的不同输出文件(如 outputs/a.docx 与 outputs/sub/a.docx)。用户
			// 可见名单独经 sanitizeDeliverableDisplayName 清洗。
			dir := filepath.Join(s.snapshotDir, deliveryFileKey(d.DeliveryID))
			if err := os.MkdirAll(dir, 0o2770); err != nil {
				removePayloadFiles(out)
				return deliveryPayload{}, fmt.Errorf("create delivery file dir: %w", err)
			}
			tmpPath := filepath.Join(dir, fmt.Sprintf("%s_%s", deliveryFileMarkerKey(f.Marker), deliverableSnapshotBase(f.RelPath)))
			if err := os.WriteFile(tmpPath, f.Content, 0o640); err != nil {
				// round12 审查(I6): 中途失败必须清理已写入的前序快照。
				// round13 审查(X2): 本次 WriteFile 的残片也要清理——tmpPath 尚
				// 未加入 out.Files, removePayloadFiles 覆盖不到。
				_ = os.Remove(tmpPath)
				removePayloadFiles(out)
				return deliveryPayload{}, fmt.Errorf("write delivery file snapshot %q: %w", f.Marker, err)
			}
			out.Files = append(out.Files, deliveryFile{
				absPath: tmpPath, root: dir, relPath: filepath.Base(tmpPath),
				displayName: sanitizeDeliverableDisplayName(f.FileName, f.RelPath),
				auditPath:   f.RelPath, snapshotContent: true,
			})
		}
		return out, nil
	case domain.DeliveryTaskFailed:
		return deliveryPayload{Text: fmt.Sprintf("任务失败：%s\n%s", d.ErrorCode, domain.TruncateUTF8(d.ErrorMessage, maxDeliveryTextBytes))}, nil
	case domain.DeliveryTaskCancelled:
		return deliveryPayload{Text: fmt.Sprintf("任务已取消：%s", domain.TruncateUTF8(d.ErrorMessage, maxDeliveryTextBytes))}, nil
	case domain.DeliveryTaskInterrupted:
		return deliveryPayload{Text: fmt.Sprintf("任务中断：%s", domain.TruncateUTF8(d.ErrorMessage, maxDeliveryTextBytes))}, nil
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
	return s.cfg.Store.MarkDeliveryRetry(ctx, d.DeliveryID, d.AttemptToken, next, now)
}

func (s *deliveryService) deadLetter(ctx context.Context, d domain.Delivery, code, message string, now time.Time) error {
	return s.cfg.Store.MarkDeliveryDeadLetter(ctx, d.DeliveryID, d.AttemptToken, code, message, now)
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

// deliveryClientID 返回发送幂等键(round9 审查): 同一 delivery part 重试时
// 保持同 client_id, 供 iLink 服务端去重。partKey 可能含路径等长内容,
// 截断+哈希保证稳定且在平台 id 长度约束内。
func deliveryClientID(partKey string) string {
	sum := sha256.Sum256([]byte(partKey))
	return "ga-" + hex.EncodeToString(sum[:8])
}

// teamSessionKey 解析 team:<uuid> 形式的 session key; 非团队 key 或
// 非 uuid 的 team id 返回 ok=false(Round16-P2: 与 validateSessionAccess
// 一致, team 表 id 为 UUID 列, 旧整数/任意字符串在 $1::uuid cast 处
// SQL 报错而非干净拒绝)。
func teamSessionKey(sessionKey string) (string, bool) {
	if !strings.HasPrefix(sessionKey, "team:") {
		return "", false
	}
	id := strings.TrimPrefix(sessionKey, "team:")
	if _, err := uuid.Parse(id); err != nil {
		return "", false
	}
	return id, true
}


// cleanupSpoolCaptureDir 删除 delivery spool capture/ 下早于 before 的
// 文件与空目录(2026-08-13 审查 B4/T5): 捕获期 spool 文件是持久快照, 与
// task_delivery_files 行同 30d 保留期——DB 行由 DeleteExpiredDeliveryFiles
// 删除, 文件按 mtime 独立过期。返回删除文件数。错误仅记日志(不阻塞 tick)。
func cleanupSpoolCaptureDir(spoolDir string, before time.Time) int {
	captureRoot := filepath.Join(spoolDir, "capture")
	entries, err := os.ReadDir(captureRoot)
	if err != nil {
		if !os.IsNotExist(err) {
			slog.ErrorContext(context.Background(), "delivery: spool capture dir read failed", "error", err)
		}
		return 0
	}
	removed := 0
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		taskDir := filepath.Join(captureRoot, entry.Name())
		files, err := os.ReadDir(taskDir)
		if err != nil {
			continue
		}
		for _, f := range files {
			if f.IsDir() {
				continue
			}
			info, err := f.Info()
			if err != nil {
				continue
			}
			if info.ModTime().Before(before) {
				if err := os.Remove(filepath.Join(taskDir, f.Name())); err == nil {
					removed++
				}
			}
		}
		// 空任务目录一并回收。
		if remaining, err := os.ReadDir(taskDir); err == nil && len(remaining) == 0 {
			_ = os.Remove(taskDir)
		}
	}
	return removed
}

// deliveryFileKey 把 delivery_id(含 ':' 等非文件名字符)转为安全文件名前缀。
func deliveryFileKey(deliveryID string) string {
	replacer := strings.NewReplacer(":", "_", "/", "_", "\\", "_", "..", "_")
	return replacer.Replace(deliveryID)
}

// deliveryFileMarkerKey 返回 marker 的 16 位哈希前缀(8 字节, 区分同 basename
// 文件)。Round8 审查: 原 4 字节前缀(2^32 空间)在数百个 marker 内即可能碰撞
// (实证: outputs/d4561/report.docx 与 outputs/d36751/report.docx 同为
// 73262a8d), 碰撞导致同一 delivery 的两个快照互相覆盖、发送重复内容。
// 8 字节(2^64 空间)在同一 delivery 的文件规模下不可碰撞。
func deliveryFileMarkerKey(marker string) string {
	sum := sha256.Sum256([]byte(marker))
	return hex.EncodeToString(sum[:8])
}

// stripControlChars 移除 ASCII 控制字符(换行/回车等)——显示名会流入消息
// Content 与 MediaAsset.FileName(审查 C1), 控制字符会造成日志/消息注入。
func stripControlChars(s string) string {
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, s)
}

// mediaTypeForPath 按扩展名推断出站媒体类型(IM_MEDIA_ARCHITECTURE §5.1 A2:
// delivery 按 MIME 分发 image/video/file, poller 按 msg_type 走渠道上传)。
// 未知扩展名回退 "file"(保守, 与 poller /send 默认一致)。
func mediaTypeForPath(path string) string {
	ext := strings.ToLower(filepath.Ext(path))
	switch ext {
	case ".jpg", ".jpeg", ".png", ".gif", ".webp", ".bmp":
		return "image"
	case ".mp4", ".mov", ".avi", ".mkv", ".webm":
		return "video"
	default:
		return "file"
	}
}

// deliverableSnapshotBase 返回写入 Platform 私有快照目录的服务端可信
// 文件名(审查 C1): 只从 DB 快照的 RelPath 派生 basename(该值由服务端
// resolveUnderRoot 解析生成), 不信任 manifest 提供的 FileName。
// Round8 审查: 文件名还要拼接 16 位 hash 前缀 + '_', basename 必须为
// 前缀留出空间(255 - 17), 否则整体文件名超过 NAME_MAX 写入失败。
const maxSnapshotBaseLen = 255 - 17

func deliverableSnapshotBase(relPath string) string {
	name := stripControlChars(filepath.Base(filepath.FromSlash(relPath)))
	if name == "" || name == "." || name == ".." || strings.ContainsAny(name, `\/`) || len(name) > maxSnapshotBaseLen {
		return "deliverable.bin"
	}
	return name
}

// sanitizeDeliverableDisplayName 清洗用户可见显示名(审查 C1): FileName
// 源于 Runner 可写的 manifest(如 path traversal / 反斜杠分隔符), 必须
// 降级为纯 basename; 非法时回退到服务端可信的 relPath basename。
func sanitizeDeliverableDisplayName(fileName, relPath string) string {
	// 反斜杠视作分隔符(Windows 风格穿越), 统一转斜杠后取最后一段;
	// 控制字符一并剥离(流入消息 Content / MediaAsset.FileName)。
	name := stripControlChars(filepath.Base(filepath.FromSlash(strings.ReplaceAll(fileName, `\`, "/"))))
	if name == "" || name == "." || name == ".." || strings.ContainsAny(name, `\/`) || len(name) > 255 {
		name = deliverableSnapshotBase(relPath)
	}
	return name
}

var (
	turnMarkerLineRE           = regexp.MustCompile(`(?m)^\s*\*{0,2}LLM Running \(Turn \d+\) \.\.\.\*{0,2}\s*$`)
	hiddenTranscriptTagRE      = regexp.MustCompile(`(?is)<(?:thinking|summary|tool_use|file_content)>.*?</(?:thinking|summary|tool_use|file_content)>`)
	compactToolLineRE          = regexp.MustCompile(`^\s*🛠️\s+[A-Za-z_][A-Za-z0-9_]*\(.*$`)
	internalReasoningEnglishRE = regexp.MustCompile(`(?i)(the user is asking|let me\b|i should\b|actually\b|since there(?:'s| is)\b|i'?m just waiting for instructions)`)

	// IM 渠道 md 降级(微信等不渲染 Markdown): 见 cleanIMMarkdown。
	imBoldCodeRE    = regexp.MustCompile(`\*\*(.+?)\*\*|` + "`([^`]+)`" + `|~~(.+?)~~`)
	imLinkRE        = regexp.MustCompile(`\[([^\]]+)\]\([^)]+\)`)
	imHeadingPrefixRE = regexp.MustCompile(`^\s*#{1,6}\s*`)
)

// cleanIMMarkdown 将 Markdown 文本降级为纯文本(微信等 IM 渠道不渲染 md)。
// 覆盖加粗/行内代码/删除线/链接/标题/表格分隔线; 列表与引用保留可读符号。
// 平台当前交付统一走 Bot(微信)渠道, 全量应用无副作用。
func cleanIMMarkdown(s string) string {
	lines := strings.Split(s, "\n")
	out := make([]string, 0, len(lines))
	for _, ln := range lines {
		t := imBoldCodeRE.ReplaceAllStringFunc(ln, func(m string) string {
			switch {
			case strings.HasPrefix(m, "**"):
				return strings.TrimSuffix(strings.TrimPrefix(m, "**"), "**")
			case strings.HasPrefix(m, "`"):
				return strings.Trim(m, "`")
			default: // ~~x~~
				return strings.TrimSuffix(strings.TrimPrefix(m, "~~"), "~~")
			}
		})
		t = imLinkRE.ReplaceAllString(t, "$1")
		if strings.HasPrefix(t, "|") && strings.HasSuffix(t, "|") {
			cells := strings.Split(strings.Trim(t, "|"), "|")
			sep := true
			for _, c := range cells {
				if !strings.HasPrefix(strings.TrimSpace(c), "-") {
					sep = false
					break
				}
			}
			if sep {
				continue // 表格分隔行
			}
			for i := range cells {
				cells[i] = strings.TrimSpace(cells[i])
			}
			out = append(out, strings.Join(cells, " | "))
			continue
		}
		out = append(out, imHeadingPrefixRE.ReplaceAllString(t, ""))
	}
	return strings.Join(out, "\n")
}

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
