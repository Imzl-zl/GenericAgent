package application

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"

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

// IncomingMessage is a message received from a bot transport.
type IncomingMessage struct {
	BotUUID     string
	IlinkUserID string
	MessageID   string
	Text        string
	// MediaPaths are absolute local file paths of inbound media downloaded by
	// the Bot Poller. Surfaced in the task prompt so GA's file tools can read
	// them. Empty for text-only messages.
	MediaPaths []string
	// MediaItems is the metadata for each media file (file_name, relative
	// storage_path, content_type, size). Used to populate the media_assets
	// table for audit / Web UI history / cross-instance idempotency.
	MediaItems []IncomingMediaItem
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
	GetBotByUUID(ctx context.Context, botUUID string) (domain.Bot, error)
	GetUserStatus(ctx context.Context, userID int64) (domain.UserStatus, error)
	GetUserToolPolicy(ctx context.Context, userID int64) (string, error)
	FindRunningTaskBySession(ctx context.Context, sessionKey string) (domain.Task, error)
	// ResetWorkspaceForNewSession 设置 reset_at 并取消 queued 任务(/new)。
	ResetWorkspaceForNewSession(ctx context.Context, sessionKey string) (int, error)
}

// MessageStore persists inbound and outbound WeChat messages for history,
// audit, and Web UI rendering. The router uses it as the durable idempotency
// backstop (replacing the in-memory seen map as the source of truth).
type MessageStore interface {
	InsertInboundMessage(ctx context.Context, m domain.Message) (domain.Message, error)
	InsertOutboundMessage(ctx context.Context, m domain.Message) (domain.Message, error)
	HasOutboundMessage(ctx context.Context, taskID, messageType, content, mediaPath string) (bool, error)
	InsertMediaAsset(ctx context.Context, m domain.MediaAsset) (domain.MediaAsset, error)
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
	store          RouterStore
	tasks          TaskService
	transport      transport.BotTransportAdapter
	commands       CommandRegistry
	messages       MessageStore
	sessionFiles   SessionFiles
	teams          TeamService
	relay          RelayService
	channelBindings ChannelBindingResolver
	toolPolicy     string
	sourceInstance string
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
		store:          cfg.Store,
		tasks:          cfg.Tasks,
		transport:      cfg.Transport,
		commands:       cfg.Commands,
		messages:       cfg.Messages,
		sessionFiles:   cfg.SessionFiles,
		teams:          cfg.Teams,
		relay:          cfg.Relay,
		channelBindings: cfg.ChannelBindings,
		toolPolicy:     cfg.ToolPolicy,
		sourceInstance: cfg.SourceInstance,
	}, nil
}

// resolveOwnerID 解析消息发送者的 canonical user: 渠道绑定存在时用绑定用户
// (跨渠道统一身份, 方案 §5.1), 未绑定时回退 bot 属主; 绑定查询发生非
// "未找到"错误时返回错误(不静默路由到 bot owner 的工作区, 防止 DB 故障
// 下跨用户串区)。
func (r *router) resolveOwnerID(ctx context.Context, msg IncomingMessage, botOwnerID int64) (int64, error) {
	if r.channelBindings == nil || msg.IlinkUserID == "" {
		return botOwnerID, nil
	}
	canonical, err := r.channelBindings.ResolveCanonicalUserID(ctx, "ilink", msg.IlinkUserID)
	if err != nil {
		if errors.Is(err, domain.ErrChannelBindingNotFound) {
			return botOwnerID, nil
		}
		return 0, err
	}
	return canonical, nil
}

// HandleMessage processes one incoming message per spec §6.1.
func (r *router) HandleMessage(ctx context.Context, msg IncomingMessage) (RouterResult, error) {
	if msg.BotUUID == "" || msg.IlinkUserID == "" || msg.MessageID == "" {
		return RouterResult{Action: ActionRejected, Reply: "missing required fields"}, nil
	}
	// Step 1: idempotency check.
	first, err := r.transport.RecordMessageIdempotency(ctx, msg.BotUUID, msg.MessageID)
	if err != nil {
		return RouterResult{}, fmt.Errorf("idempotency: %w", err)
	}
	if !first {
		return RouterResult{Action: ActionDuplicate, Reply: "duplicate message ignored"}, nil
	}
	// Step 2: resolve bot identity.
	bot, err := r.store.GetBotByUUID(ctx, msg.BotUUID)
	if err != nil {
		reply := "unknown bot"
		_ = r.transport.SendMessage(ctx, msg.BotUUID, msg.IlinkUserID, reply)
		return RouterResult{Action: ActionRejected, Reply: reply}, nil
	}
	// Resolve active context early so persistInbound records the correct
	// session_key (personal:{user_id} vs team:{team_id}). Falls back to
	// personal session when teams are disabled or context lookup fails.
	ownerID, err := r.resolveOwnerID(ctx, msg, bot.OwnerID)
	if err != nil {
		reply := "身份解析失败，请稍后重试"
		_ = r.transport.SendMessage(ctx, msg.BotUUID, msg.IlinkUserID, reply)
		return RouterResult{Action: ActionRejected, Reply: reply, UserID: bot.OwnerID}, nil
	}
	// canonical identity(方案 §5.1): 后续 status/任务提交全部以绑定用户为准。
	bot.OwnerID = ownerID
	inboundSessionKey := personalSessionKey(ownerID)
	if r.teams != nil {
		if ac, err := r.teams.GetActiveContext(ctx, ownerID); err == nil {
			inboundSessionKey = ac.SessionKey(ownerID)
		}
	}
	// Persist inbound message for history/audit and as the cross-instance
	// idempotency backstop. The partial UNIQUE(bot_id, message_id) index
	// rejects duplicates when the in-memory seen map is cold (restart) or
	// split across instances. Persistence failure is non-fatal: the message
	// is still routed so the user is not blocked; the missing audit row is
	// acceptable, but we log loudly so DB issues surface fast.
	if _, perr := r.persistInbound(ctx, msg, bot, inboundSessionKey); perr != nil {
		if errors.Is(perr, domain.ErrDuplicateInboundMessage) {
			return RouterResult{Action: ActionDuplicate, Reply: "duplicate message ignored"}, nil
		}
		slog.ErrorContext(ctx, "router: persist inbound message failed",
			"message_id", msg.MessageID,
			"bot_uuid", msg.BotUUID,
			"error", perr)
	}
	// Unbound bot: with the official iLink QR binding flow, bots are always
	// created with ilink_user_id set. An unbound bot means it was created
	// outside the QR flow (legacy path) or the binding was revoked. Either
	// way, the bot cannot process messages; reject and surface the state.
	if !bot.IsBound() {
		reply := "bot not bound; contact admin to rebind via WeChat QR"
		_ = r.transport.SendMessage(ctx, msg.BotUUID, msg.IlinkUserID, reply)
		return RouterResult{Action: ActionRejected, Reply: reply, UserID: bot.OwnerID}, nil
	}
	// Bound bot: verify from_user_id matches (spec §6.1 step 2).
	if bot.IlinkUserID != msg.IlinkUserID {
		reply := "identity mismatch"
		_ = r.transport.SendMessage(ctx, msg.BotUUID, msg.IlinkUserID, reply)
		return RouterResult{Action: ActionRejected, Reply: reply}, nil
	}
	// canonical 绑定写入(方案 §5.1): 身份校验通过后幂等建立渠道账号 →
	// canonical user 绑定, 保证跨渠道同一用户落在同一工作区。绑定失败
	// 不阻断消息(仅影响跨渠道身份合并)。
	if r.channelBindings != nil && msg.IlinkUserID != "" {
		if _, bindErr := r.channelBindings.BindChannelAccount(ctx, "ilink", msg.IlinkUserID, ownerID); bindErr != nil {
			slog.WarnContext(ctx, "router: auto-bind channel account failed",
				"ilink_user_id", msg.IlinkUserID, "canonical_user_id", ownerID, "error", bindErr)
		}
	}
	// Step 3: check user status.
	status, err := r.store.GetUserStatus(ctx, bot.OwnerID)
	if err != nil {
		return RouterResult{}, fmt.Errorf("user status: %w", err)
	}
	if status != domain.UserApproved {
		reply := fmt.Sprintf("user is %s, cannot process messages", status)
		_ = r.transport.SendMessage(ctx, msg.BotUUID, msg.IlinkUserID, reply)
		return RouterResult{Action: ActionRejected, Reply: reply, UserID: bot.OwnerID}, nil
	}
	// Steps 4-7: parse command and route.
	return r.routeBoundMessage(ctx, msg, bot)
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
func (r *router) routeBoundMessage(ctx context.Context, msg IncomingMessage, bot domain.Bot) (RouterResult, error) {
	text := strings.TrimSpace(msg.Text)
	// Relay intercept: "@<username> <body>" is forwarded directly to the
	// named user's WeChat, bypassing the LLM/Worker. Checked before command
	// resolution so @-mentions never collide with slash commands. When the
	// RelayService is nil, handleRelay falls through to handleNormalMessage.
	// Only triggered in personal context; in team context @username is a
	// normal message to the team AI (spec §6 R4).
	if strings.HasPrefix(text, relayAtPrefix) && r.isPersonalContext(ctx, bot.OwnerID) {
		return r.handleRelay(ctx, msg, bot, text)
	}
	cmd, found := r.resolveCommand(ctx, text)
	if strings.HasPrefix(text, "/") {
		if isRestrictedUserCommand(text) || !found || cmd.Action != domain.CommandIntercept {
			return r.rejectUnavailableCommand(ctx, msg, bot)
		}
		return r.dispatchHandler(ctx, msg, bot, cmd.Handler, text)
	}
	return r.handleNormalMessage(ctx, msg, bot, text)
}

// dispatchHandler routes to the Go handler func by handler key.
// Handler keys are stable identifiers (e.g. "stop", "status"); the registry
// maps command text → handler key, and this function maps handler key → func.
func (r *router) dispatchHandler(ctx context.Context, msg IncomingMessage, bot domain.Bot, handler, text string) (RouterResult, error) {
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

func (r *router) rejectUnavailableCommand(ctx context.Context, msg IncomingMessage, bot domain.Bot) (RouterResult, error) {
	reply := "该命令不可用。发送 /help 查看可用命令。"
	_ = r.transport.SendMessage(ctx, msg.BotUUID, msg.IlinkUserID, reply)
	return RouterResult{Action: ActionRejected, Reply: reply, UserID: bot.OwnerID}, nil
}
