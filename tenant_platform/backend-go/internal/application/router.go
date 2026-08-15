package application

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/domain"
	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/infrastructure/transport"
)

// RouterAction classifies what the router did with an incoming message.
type RouterAction string

const (
	ActionStopped     RouterAction = "stopped"
	ActionNewSession  RouterAction = "new_session"
	ActionTaskCreated RouterAction = "task_created"
	ActionRejected    RouterAction = "rejected"
	ActionDuplicate   RouterAction = "duplicate"
	ActionNoRunning   RouterAction = "no_running_task"
	ActionHelp        RouterAction = "help"
	ActionStatus      RouterAction = "status"
	// ActionReplied covers team/context commands whose outcome is a user-facing
	// reply (identity, switch, invite submit, member approve/reject/remove).
	// Keeping a single action avoids action-type sprawl for one-shot commands.
	ActionReplied RouterAction = "replied"
)

// RouterResult is the outcome of processing an incoming message.
type RouterResult struct {
	Action RouterAction
	Reply  string
	UserID int64
}

// IncomingMessage is a message received from a channel transport
// (IM_CHANNEL_BINDING §5).
type IncomingMessage struct {
	BotUUID string
	// ChannelType 标识渠道(wechat|feishu|dingtalk|qq), 同时是任务 Source 与
	// 回复分发依据。
	ChannelType string
	// ChannelAccountID 是渠道侧账号标识(微信=ilink_user_id, 其他=发送者账号)。
	ChannelAccountID string
	// ConversationID 是对话单元 ID(群 ID / 对端 ID; 微信恒空)——分桶键。
	ConversationID string
	// ConversationType 是对话单元类型('private'|'group'; 空/非法回退
	// 'private')——IM 流式转发判定维度(群聊只发最终结果)。
	ConversationType string
	MessageID      string
	Text           string
	// MediaPaths are absolute local file paths of inbound media downloaded by
	// the Bot Poller. Surfaced in the task prompt so GA's file tools can read
	// them. Empty for text-only messages.
	MediaPaths []string
	// MediaItems is the metadata for each media file (file_name, relative
	// storage_path, content_type, size). Used to populate the media_assets
	// table for audit / Web UI history / cross-instance idempotency.
	MediaItems []IncomingMediaItem
}

// replyTarget 返回回复目标地址: 新渠道优先回对话单元(群/单聊), 微信回
// 发送者 ilink_user_id(conversation_id 恒空)。
func (m IncomingMessage) replyTarget() string {
	if m.ConversationID != "" {
		return m.ConversationID
	}
	return m.ChannelAccountID
}

// IncomingMediaItem is the per-file metadata forwarded by the Bot Poller.
// StoragePath is RELATIVE to media_dir so the same row works across mount points.
type IncomingMediaItem struct {
	FileName    string
	StoragePath string
	ContentType string
	Size        int64
}

// RouterStore is the persistence port for router identity resolution.
type RouterStore interface {
	GetChannelConfigByUUID(ctx context.Context, botUUID string) (domain.ChannelConfig, error)
	GetUserStatus(ctx context.Context, userID int64) (domain.UserStatus, error)
	FindRunningTaskBySession(ctx context.Context, sessionKey, conversationKey string) (domain.Task, error)
	// ResetWorkspaceForNewSession 设置 reset_at 并取消 queued 任务(/new)。
	ResetWorkspaceForNewSession(ctx context.Context, sessionKey, conversationKey string) (int, error)
	// GetConversationResetAt 返回本对话单元桶最近一次 /new 的 reset_at
	// (无记录 = 零值)。会话文件引用隔离的真值源(2026-08-15):
	// RecentSince 按它过滤跨会话文件。
	GetConversationResetAt(ctx context.Context, sessionKey, conversationKey string) (time.Time, error)
}

// MessageStore persists inbound and outbound WeChat messages for history,
// audit, and Web UI rendering. The router uses it as the durable idempotency
// backstop (replacing the in-memory seen map as the source of truth).
type MessageStore interface {
	InsertInboundMessage(ctx context.Context, m domain.Message) (domain.Message, error)
	// HasInboundMessage 只读检查 (bot_id, message_id) 是否已入库(round9 审查:
	// 路由成功后才写消息行, 因此消息行存在 = 该消息已成功处理过——重启/多
	// 实例后内存幂等缓存变冷时, 用它短路重复的 relay/命令副作用)。
	HasInboundMessage(ctx context.Context, botID int64, messageID string) (bool, error)
	// DeleteInboundMessage 删除 claim 后副作用失败的消息行(round10 审查 B7):
	// 允许 Poller 重试重新执行命令/relay。
	DeleteInboundMessage(ctx context.Context, botID int64, messageID string) error
	InsertOutboundMessage(ctx context.Context, m domain.Message) (domain.Message, error)
	HasOutboundMessage(ctx context.Context, taskID, messageType, content, mediaPath string) (bool, error)
	InsertMediaAsset(ctx context.Context, m domain.MediaAsset) (domain.MediaAsset, error)
	// DeleteExpiredMediaAssets 删除超过保留期的媒体审计行(2026-08-13 审查
	// I4/D7: 媒体字节=用户隐私数据, 审计行定保留期 90d, 与 delivery 文件
	// 快照 30d 清理同模式)。返回删除行数。
	DeleteExpiredMediaAssets(ctx context.Context, before time.Time) (int64, error)
}

// CommandRegistry supplies the admin-configurable command list.
// The router uses version-based cache invalidation: it checks a cheap
// fingerprint (MAX(updated_at)) every 5 seconds, and only reloads the full
// command list when the version changes. This avoids per-message DB load
// while keeping admin changes near-real-time (≤5s latency).
type CommandRegistry interface {
	ListEnabledCommands(ctx context.Context) ([]domain.PlatformCommand, error)
	CommandRegistryVersion(ctx context.Context) (string, error)
}

// ChannelBindingResolver 解析渠道账号(如 iLink user id)对应的 canonical user,
// 并提供幂等绑定写入(方案 §5.1: 首次消息自动建立绑定)。
type ChannelBindingResolver interface {
	ResolveCanonicalUserID(ctx context.Context, channelType, channelAccountID string) (int64, error)
	BindChannelAccount(ctx context.Context, channelType, channelAccountID string, canonicalUserID int64) (domain.ChannelBinding, error)
}

// RouterConfig wires the router's dependencies.
type RouterConfig struct {
	Store          RouterStore
	Tasks          TaskService
	Transport      transport.BotTransportAdapter
	Commands       CommandRegistry
	Messages       MessageStore
	SessionFiles   SessionFiles
	ToolPolicy     string
	SourceInstance string
	// Teams is the team lifecycle service. Optional in P0; when nil, team
	// commands reply with "功能未启用" and messages always route to personal.
	Teams TeamService
	// ChannelBindings 解析渠道账号 → canonical user(方案 §5.1)。nil 时回退
	// bot 属主(未启用渠道绑定)。
	ChannelBindings ChannelBindingResolver
	// Relay is the @username forwarding service. Optional; when nil,
	// @-mentions fall through to normal task routing.
	Relay RelayService
	// BotMediaRoot 是 Bot Poller 媒体文件的宿主根目录(compose: bot_media 卷)。
	// 非空时, 入站 media_paths 必须解析后仍位于该根内(Round8 审查: Poller
	// 可被攻破时防止读取 Platform 容器任意文件); 空 = 不校验(loopback/dev)。
	BotMediaRoot string
}

// Router processes incoming bot messages: identity resolution, status check,
// command parsing, and task routing (spec §6.1–§6.2).
type Router interface {
	HandleMessage(ctx context.Context, msg IncomingMessage) (RouterResult, error)
	// InvalidateCommandCache clears the in-memory command registry cache so
	// the next message reloads the latest admin configuration. Called by
	// admin update handlers after changing platform_commands.
	InvalidateCommandCache()
}

type router struct {
	store           RouterStore
	tasks           TaskService
	transport       transport.BotTransportAdapter
	commands        CommandRegistry
	messages        MessageStore
	sessionFiles    SessionFiles
	teams           TeamService
	relay           RelayService
	channelBindings ChannelBindingResolver
	toolPolicy      string
	sourceInstance  string
	botMediaRoot    string
	// Trigger-invalidated cache for command registry. Admin update handlers
	// call InvalidateCommandCache() after changing platform_commands.
	cacheMu        sync.Mutex
	cachedCommands map[string]domain.PlatformCommand // command text → entry
	cacheLoaded    bool
}

// NewRouter constructs the router.
func NewRouter(cfg RouterConfig) (Router, error) {
	if cfg.Store == nil || cfg.Tasks == nil || cfg.Transport == nil {
		return nil, fmt.Errorf("store, tasks, and transport are required")
	}
	if cfg.Messages == nil {
		return nil, fmt.Errorf("message store is required")
	}
	if cfg.ToolPolicy == "" {
		return nil, fmt.Errorf("tool policy version is required")
	}
	if cfg.SourceInstance == "" {
		cfg.SourceInstance = "router"
	}
	return &router{
		store:           cfg.Store,
		tasks:           cfg.Tasks,
		transport:       cfg.Transport,
		commands:        cfg.Commands,
		messages:        cfg.Messages,
		sessionFiles:    cfg.SessionFiles,
		teams:           cfg.Teams,
		relay:           cfg.Relay,
		channelBindings: cfg.ChannelBindings,
		toolPolicy:      cfg.ToolPolicy,
		sourceInstance:  cfg.SourceInstance,
		botMediaRoot:    cfg.BotMediaRoot,
	}, nil
}

// validateMediaPaths 校验入站媒体文件路径必须位于 BotMediaRoot 内(Round8 审查:
// webhook 的 media_paths 来自 Bot Poller, HMAC 只能证明请求来自 Poller——Poller
// 被攻破后可提交任意宿主路径读取 Platform 容器文件, 如 /proc/self/environ 的
// 数据库/Manager/JWT 密钥)。校验: 绝对路径 + EvalSymlinks 解析全部组件后仍
// 在根内(防符号链接逃逸) + 普通文件。root 为空时不校验(loopback/dev)。
func validateMediaPaths(root string, paths []string) error {
	if root == "" {
		return nil
	}
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return fmt.Errorf("resolve bot media root %q: %w", root, err)
	}
	for _, p := range paths {
		if strings.TrimSpace(p) == "" {
			continue
		}
		if !filepath.IsAbs(p) {
			return fmt.Errorf("media path must be absolute: %q", p)
		}
		resolved, err := filepath.EvalSymlinks(p)
		if err != nil {
			return fmt.Errorf("resolve media path %q: %w", p, err)
		}
		info, err := os.Stat(resolved)
		if err != nil {
			return fmt.Errorf("stat media path %q: %w", p, err)
		}
		if info.IsDir() {
			return fmt.Errorf("media path %q is a directory", p)
		}
		rel, err := filepath.Rel(absRoot, resolved)
		if err != nil {
			return fmt.Errorf("relate media path %q: %w", p, err)
		}
		if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return fmt.Errorf("media path %q escapes bot media root", p)
		}
	}
	return nil
}

// resolveOwnerID 解析消息发送者的 canonical user: 仅微信渠道走渠道绑定
// (方案 §5.1, 跨渠道统一身份); 新渠道(channel_configs 的属主即 canonical
// user, IM_CHANNEL_BINDING §6)与未绑定场景直接回退 config 属主。绑定查询
// 发生非"未找到"错误时返回错误(不静默路由到 bot owner 的工作区, 防止
// DB 故障下跨用户串区)。
func (r *router) resolveOwnerID(ctx context.Context, msg IncomingMessage, botOwnerID int64) (int64, error) {
	if r.channelBindings == nil || msg.ChannelType != string(domain.ChannelWechat) || msg.ChannelAccountID == "" {
		return botOwnerID, nil
	}
	canonical, err := r.channelBindings.ResolveCanonicalUserID(ctx, string(domain.ChannelWechat), msg.ChannelAccountID)
	if err != nil {
		if errors.Is(err, domain.ErrChannelBindingNotFound) {
			return botOwnerID, nil
		}
		return 0, err
	}
	return canonical, nil
}

// HandleMessage processes one incoming message per spec §6.1.
//
// Round8 审查(处理顺序重构): 幂等标记从"处理前"改为"成功处理后"——任何
// 中途失败(授权/路由/任务提交)返回 error 时都不标记, Poller 重试能真正
// 重新处理; 消息持久化从授权检查之前移到路由成功之后——身份不匹配/未绑定/
// 被阻止的发送者不得向目标租户写入 messages/media_assets 记录; 任务提交
// 先于消息入库, 重试撞 DB 唯一键时任务已存在, 不会丢任务。
func (r *router) HandleMessage(ctx context.Context, msg IncomingMessage) (RouterResult, error) {
	if msg.BotUUID == "" || msg.ChannelAccountID == "" || msg.MessageID == "" {
		return RouterResult{Action: ActionRejected, Reply: "missing required fields"}, nil
	}
	// 渠道类型必须是已支持渠道(未识别渠道拒绝而非透传, 防误路由)。
	if msg.ChannelType == "" {
		return RouterResult{Action: ActionRejected, Reply: "missing channel_type"}, nil
	}
	if !domain.IsValidChannelType(msg.ChannelType) {
		return RouterResult{Action: ActionRejected, Reply: "unsupported channel_type"}, nil
	}
	// Step 1: 幂等只读检查(成功处理后 Mark, 见函数末尾)。
	seen, err := r.transport.CheckMessageIdempotency(ctx, msg.BotUUID, msg.MessageID)
	if err != nil {
		return RouterResult{}, fmt.Errorf("idempotency: %w", err)
	}
	if seen {
		return RouterResult{Action: ActionDuplicate, Reply: "duplicate message ignored"}, nil
	}
	// Step 2: resolve bot identity。区分"bot 不存在"(永久拒绝)与"查询失败"
	// (瞬态, 如 DB 抖动): 旧实现把所有 GetChannelConfigByUUID 错误都当 unknown bot
	// 消费——DB 故障时用户收到误导性回复且 webhook 回 200, Poller ack 后
	// 消息永久丢失。Round8 原则: 中途失败必须返回 error → webhook 5xx →
	// Poller 按契约重试(任务/消息行唯一键保证重试幂等)。
	bot, err := r.store.GetChannelConfigByUUID(ctx, msg.BotUUID)
	if err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			return RouterResult{}, fmt.Errorf("resolve bot: %w", err)
		}
		reply := "机器人未注册或已失效，请联系管理员"
		slog.WarnContext(ctx, "router: rejected message from unknown bot",
			"bot_uuid", msg.BotUUID,
			"channel_account_id", msg.ChannelAccountID,
			"channel_type", msg.ChannelType,
			"message_id", msg.MessageID)
		if sendErr := r.transport.SendMessage(ctx, msg.BotUUID, msg.replyTarget(), reply, ""); sendErr != nil {
			slog.WarnContext(ctx, "router: send unknown-bot reply failed",
				"bot_uuid", msg.BotUUID,
				"error", sendErr)
		}
		return RouterResult{Action: ActionRejected, Reply: reply}, nil
	}
	// Step 2.5: DB 级幂等(round9 审查: 重启/多实例后内存 seen 缓存变冷,
	// 任务唯一键只保护任务不重复, relay 转发与团队命令没有等价兜底)。
	// 入站消息行在路由成功后写入, 因此消息行已存在 = 该消息已成功处理过,
	// 直接按 duplicate 短路, 不再执行路由(不重复 relay/命令副作用)。
	// 残余窗口: 第一次路由成功但消息行写入前崩溃——重试会重新路由,
	// 任务由唯一键去重, relay 仍可能重复(Round8 已声明限制)。
	if r.messages != nil {
		dup, hasErr := r.messages.HasInboundMessage(ctx, bot.ID, msg.MessageID)
		if hasErr != nil {
			return RouterResult{}, fmt.Errorf("inbound idempotency: %w", hasErr)
		}
		if dup {
			return RouterResult{Action: ActionDuplicate, Reply: "duplicate message ignored"}, nil
		}
	}
	if bot.ChannelType != domain.ChannelType(msg.ChannelType) {
		// 契约完整性(IM_CHANNEL_BINDING §5): bot_uuid 与 channel_type 必须
		// 一致(Source=ChannelType 直接进任务, 错配会污染桶归属)。fail-closed。
		slog.WarnContext(ctx, "router: rejected message with channel_type mismatch",
			"bot_uuid", msg.BotUUID,
			"bot_channel_type", bot.ChannelType,
			"msg_channel_type", msg.ChannelType,
			"message_id", msg.MessageID)
		reply := "channel type mismatch"
		_ = r.transport.SendMessage(ctx, msg.BotUUID, msg.replyTarget(), reply, "")
		return RouterResult{Action: ActionRejected, Reply: reply, UserID: bot.OwnerID}, nil
	}
	// canonical identity(方案 §5.1): 后续 status/任务提交全部以绑定用户为准。
	ownerID, err := r.resolveOwnerID(ctx, msg, bot.OwnerID)
	if err != nil {
		reply := "身份解析失败，请稍后重试"
		_ = r.transport.SendMessage(ctx, msg.BotUUID, msg.replyTarget(), reply, "")
		return RouterResult{Action: ActionRejected, Reply: reply, UserID: bot.OwnerID}, nil
	}
	bot.OwnerID = ownerID
	// Step 3: 授权检查(Round8: 全部先于任何持久化——未授权发送者不得写入
	// 目标租户的 messages/media_assets)。
	if !bot.IsBound() {
		reply := "bot not bound; contact admin to rebind via WeChat QR"
		_ = r.transport.SendMessage(ctx, msg.BotUUID, msg.replyTarget(), reply, "")
		return RouterResult{Action: ActionRejected, Reply: reply, UserID: bot.OwnerID}, nil
	}
	// 微信身份校验: 只允许绑定用户本人与 bot 对话。新渠道无此概念——
	// 配置属主即 canonical user, 群内任意成员 @ 触发都路由到属主工作区。
	if bot.ChannelType == domain.ChannelWechat && bot.IlinkUserID != msg.ChannelAccountID {
		reply := "identity mismatch"
		_ = r.transport.SendMessage(ctx, msg.BotUUID, msg.replyTarget(), reply, "")
		return RouterResult{Action: ActionRejected, Reply: reply}, nil
	}
	status, err := r.store.GetUserStatus(ctx, bot.OwnerID)
	if err != nil {
		return RouterResult{}, fmt.Errorf("user status: %w", err)
	}
	if status != domain.UserApproved {
		reply := fmt.Sprintf("user is %s, cannot process messages", status)
		_ = r.transport.SendMessage(ctx, msg.BotUUID, msg.replyTarget(), reply, "")
		return RouterResult{Action: ActionRejected, Reply: reply, UserID: bot.OwnerID}, nil
	}
	// canonical 绑定写入(方案 §5.1): 仅微信——身份校验通过后幂等建立渠道
	// 账号 → canonical user 绑定, 保证跨渠道同一用户落在同一工作区。新渠道
	// 不需要(channel_configs 的 owner 即 canonical user)。绑定失败不阻断
	// 消息(仅影响跨渠道身份合并)。
	if r.channelBindings != nil && bot.ChannelType == domain.ChannelWechat && msg.ChannelAccountID != "" {
		if _, bindErr := r.channelBindings.BindChannelAccount(ctx, string(domain.ChannelWechat), msg.ChannelAccountID, ownerID); bindErr != nil {
			slog.WarnContext(ctx, "router: auto-bind channel account failed",
				"channel_account_id", msg.ChannelAccountID, "canonical_user_id", ownerID, "error", bindErr)
		}
	}
	// Round8: media_paths 必须位于 BotMediaRoot 内(Poller 可被攻破时防止
	// 读取 Platform 容器任意文件); 非法路径 fail-closed 拒绝整条消息。
	if err := validateMediaPaths(r.botMediaRoot, msg.MediaPaths); err != nil {
		slog.ErrorContext(ctx, "router: rejected message with out-of-root media path",
			"bot_uuid", msg.BotUUID, "message_id", msg.MessageID, "error", err)
		reply := "invalid media path"
		_ = r.transport.SendMessage(ctx, msg.BotUUID, msg.replyTarget(), reply, "")
		return RouterResult{Action: ActionRejected, Reply: reply, UserID: bot.OwnerID}, nil
	}
	// Step 4: 路由(命令/中继/任务提交)。round10 审查(B7): 消息行不再在
	// 副作用之后单独写入——命令/relay 先原子 claim 消息行再执行副作用
	// (失败删除 claim 行, 允许 Poller 重试重新执行); 任务提交与消息行在
	// 同一 DB 事务内(SubmitTaskWithInboundMessage), 消除二段写入的崩溃/
	// 并发窗口。失败返回 error 且不标记幂等, Poller 按 webhook 契约重试。
	inboundSessionKey := domain.PersonalSessionKey(ownerID)
	personalContext := true
	if r.teams != nil {
		ac, err := r.teams.GetActiveContext(ctx, ownerID)
		if err != nil {
			// round12 审查(I7): 上下文解析错误不得静默降级个人——团队消息可能
			// 被写入个人工作区(审计/隐私分叉)。fail-closed 拒绝消息, Poller
			// 按 webhook 契约重试; "无 active_contexts 行"由 GetActiveContext
			// 返回个人上下文(TeamID nil), 不视为错误。
			slog.ErrorContext(ctx, "router: resolve active context failed; rejecting message",
				"bot_uuid", msg.BotUUID, "message_id", msg.MessageID, "error", err)
			return RouterResult{}, fmt.Errorf("resolve active context: %w", err)
		}
		inboundSessionKey = ac.SessionKey(ownerID)
		personalContext = ac.IsPersonal()
	}
	// round13 审查(D2): 路由决策全部基于这一次解析结果——relay 判定与任务/
	// 消息行落库共用同一上下文, 不再二次解析 active context(修复前 relay
	// 分支独立重查, 并发切换团队/个人时团队消息可能被误转发)。
	result, msgRow, err := r.routeBoundMessage(ctx, msg, bot, inboundSessionKey, personalContext)
	if err != nil {
		return RouterResult{}, err
	}
	// Step 5: 消息行已随路由持久化(任务同事务 / 命令-relay claim);此处只
	// 补 media_assets 审计(失败非致命, 记日志便于 DB 故障暴露)。
	if msgRow != nil && r.messages != nil {
		if perr := r.persistInboundMedia(ctx, msg, bot, *msgRow); perr != nil {
			slog.ErrorContext(ctx, "router: persist inbound media failed",
				"message_id", msg.MessageID,
				"bot_uuid", msg.BotUUID,
				"error", perr)
		}
	}
	// Step 6: 标记幂等——仅当处理成功(无 error)。命令/relay 的副作用已由
	// claim 行串行化(并发重复被唯一键短路, 崩溃窗口只可能丢命令、用户
	// 可重发), 任务路径零窗口(同事务); 此处内存标记仅做 TTL 内的快速短路。
	if err := r.transport.MarkMessageIdempotency(ctx, msg.BotUUID, msg.MessageID); err != nil {
		return RouterResult{}, fmt.Errorf("mark idempotency: %w", err)
	}
	return result, nil
}

// routeBoundMessage parses platform-level commands and routes everything else
// as a task to the Worker (spec §6.2).
//
// Design principle: slash commands are an explicit control-plane allowlist.
// Only enabled intercept commands reach handlers; restricted, passthrough, and
// unknown /xxx inputs are rejected before the Worker so GA's local-only slash
// commands cannot bypass tenant policy. Non-command text is forwarded as a task.
//
// The command set is admin-configurable via the platform_commands table
// (migration 0004). If CommandRegistry is nil (tests), falls back to defaults.
func (r *router) routeBoundMessage(ctx context.Context, msg IncomingMessage, bot domain.ChannelConfig, inboundSessionKey string, personalContext bool) (RouterResult, *domain.Message, error) {
	text := strings.TrimSpace(msg.Text)
	// Relay intercept: "@<username> <body>" is forwarded directly to the
	// named user's WeChat, bypassing the LLM/Worker. Checked before command
	// resolution so @-mentions never collide with slash commands. When the
	// RelayService is nil, handleRelay falls through to handleNormalMessage.
	// Only triggered in personal context; in team context @username is a
	// normal message to the team AI (spec §6 R4).
	// round10 审查(B7): relay 是副作用(转发消息), 先 claim 消息行再执行——
	// 并发重复被唯一键短路, 崩溃窗口内残留行只导致转发丢失(用户可重发),
	// 不再重复转发。
	// round13 审查(D2): personalContext 来自入口单次解析, 不再二次查询。
	if strings.HasPrefix(text, relayAtPrefix) && personalContext {
		if r.relay == nil {
			// 功能禁用: @mention 按普通任务处理(消息行与任务同事务), 避免
			// 外层 claim 与事务内消息行插入冲突。
			msgRow, result, err := r.handleNormalMessage(ctx, msg, bot, text, inboundSessionKey)
			return result, msgRow, err
		}
		return r.claimAndRun(ctx, msg, bot, inboundSessionKey, func() (RouterResult, error) {
			return r.handleRelay(ctx, msg, bot, text)
		})
	}
	cmd, found := r.resolveCommand(ctx, text)
	if strings.HasPrefix(text, "/") {
		if isRestrictedUserCommand(text) || !found || cmd.Action != domain.CommandIntercept {
			// 拒绝类命令同样 claim(处理已完整, 用户收到拒绝回复), 防并发重放。
			msgRow, err := r.claimInboundMessage(ctx, msg, bot, inboundSessionKey)
			if err != nil {
				if errors.Is(err, domain.ErrDuplicateInboundMessage) {
					return RouterResult{Action: ActionDuplicate, Reply: "duplicate message ignored"}, nil, nil
				}
				return RouterResult{}, nil, err
			}
			result, _ := r.rejectUnavailableCommand(ctx, msg, bot)
			return result, &msgRow, nil
		}
		return r.claimAndRun(ctx, msg, bot, inboundSessionKey, func() (RouterResult, error) {
			return r.dispatchHandler(ctx, msg, bot, cmd.Handler, text)
		})
	}
	// 任务路径: 与消息行同事务提交(round10 审查 B7), 零二段写入窗口。
	msgRow, result, err := r.handleNormalMessage(ctx, msg, bot, text, inboundSessionKey)
	return result, msgRow, err
}

// claimAndRun 先原子 claim 入站消息行(冲突=已处理→duplicate 短路), 再执行
// 命令/relay 副作用; 副作用失败时删除 claim 行并返回 error, 让 Poller 重试
// 能重新执行(round10 审查 B7: 把"副作用后写行"的重复窗口换成"先写行后
// 副作用"的丢失窗口——命令可重发, 重复执行(如重复转发/重复加人)危害更大)。
func (r *router) claimAndRun(ctx context.Context, msg IncomingMessage, bot domain.ChannelConfig, inboundSessionKey string, run func() (RouterResult, error)) (RouterResult, *domain.Message, error) {
	if r.messages == nil {
		result, err := run()
		return result, nil, err
	}
	msgRow, err := r.claimInboundMessage(ctx, msg, bot, inboundSessionKey)
	if err != nil {
		if errors.Is(err, domain.ErrDuplicateInboundMessage) {
			return RouterResult{Action: ActionDuplicate, Reply: "duplicate message ignored"}, nil, nil
		}
		return RouterResult{}, nil, fmt.Errorf("claim inbound message: %w", err)
	}
	result, err := run()
	if err != nil {
		if delErr := r.messages.DeleteInboundMessage(ctx, bot.ID, msg.MessageID); delErr != nil {
			slog.ErrorContext(ctx, "router: delete claimed message after side-effect failure failed",
				"message_id", msg.MessageID, "bot_uuid", msg.BotUUID, "error", delErr)
		}
		return RouterResult{}, nil, err
	}
	return result, &msgRow, nil
}

// claimInboundMessage 插入入站消息行作为"处理中"标记(round10 审查 B7)。
func (r *router) claimInboundMessage(ctx context.Context, msg IncomingMessage, bot domain.ChannelConfig, inboundSessionKey string) (domain.Message, error) {
	mediaPath := ""
	if len(msg.MediaPaths) > 0 {
		mediaPath = msg.MediaPaths[0]
	}
	return r.messages.InsertInboundMessage(ctx, domain.Message{
		UserID:      bot.OwnerID,
		BotID:       bot.ID,
		SessionKey:  inboundSessionKey,
		MessageID:   msg.MessageID,
		MessageType: inferMessageType(msg.MediaPaths, msg.MediaItems),
		Content:     msg.Text,
		MediaPath:   mediaPath,
	})
}

// dispatchHandler routes to the Go handler func by handler key.
// Handler keys are stable identifiers (e.g. "stop", "status"); the registry
// maps command text → handler key, and this function maps handler key → func.
func (r *router) dispatchHandler(ctx context.Context, msg IncomingMessage, bot domain.ChannelConfig, handler, text string) (RouterResult, error) {
	switch handler {
	case "stop":
		return r.handleStop(ctx, msg, bot)
	case "new":
		return r.handleNew(ctx, msg, bot)
	case "status":
		return r.handleStatus(ctx, msg, bot)
	case "help":
		return r.handleHelp(ctx, msg, bot)
	case "identity":
		return r.handleIdentity(ctx, msg, bot)
	case "personal":
		return r.handlePersonal(ctx, msg, bot)
	case "team":
		return r.handleTeam(ctx, msg, bot, text)
	case "invite_code":
		return r.handleInviteCode(ctx, msg, bot, text)
	case "accept":
		return r.handleAcceptInvite(ctx, msg, bot, text)
	case "approve":
		return r.handleApprove(ctx, msg, bot, text)
	case "reject":
		return r.handleReject(ctx, msg, bot, text)
	case "remove":
		return r.handleRemove(ctx, msg, bot, text)
	case "relay_off":
		return r.handleRelayOff(ctx, msg, bot)
	case "relay_on":
		return r.handleRelayOn(ctx, msg, bot)
	default:
		return r.rejectUnavailableCommand(ctx, msg, bot)
	}
}

func isRestrictedUserCommand(text string) bool {
	fields := strings.Fields(strings.TrimSpace(text))
	if len(fields) == 0 {
		return false
	}
	command := strings.ToLower(fields[0])
	if strings.HasPrefix(command, "/session.") {
		return true
	}
	switch command {
	case "/llm", "/activate", "/resume":
		return true
	default:
		return false
	}
}

func (r *router) rejectUnavailableCommand(ctx context.Context, msg IncomingMessage, bot domain.ChannelConfig) (RouterResult, error) {
	reply := "该命令不可用。发送 /help 查看可用命令。"
	_ = r.transport.SendMessage(ctx, msg.BotUUID, msg.replyTarget(), reply, "")
	return RouterResult{Action: ActionRejected, Reply: reply, UserID: bot.OwnerID}, nil
}
