package application

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/domain"
	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/transport"
)

// RouterAction classifies what the router did with an incoming message.
type RouterAction string

const (
	ActionActivated   RouterAction = "activated"
	ActionStopped     RouterAction = "stopped"
	ActionNewSession  RouterAction = "new_session"
	ActionTaskCreated RouterAction = "task_created"
	ActionRejected    RouterAction = "rejected"
	ActionDuplicate   RouterAction = "duplicate"
	ActionNoRunning   RouterAction = "no_running_task"
	ActionHelp        RouterAction = "help"
	ActionStatus      RouterAction = "status"
	ActionModelInfo   RouterAction = "model_info"
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
	ResetWorkspace(ctx context.Context, sessionKey string) error
}

// MessageStore persists inbound and outbound WeChat messages for history,
// audit, and Web UI rendering. The router uses it as the durable idempotency
// backstop (replacing the in-memory seen map as the source of truth).
type MessageStore interface {
	InsertInboundMessage(ctx context.Context, m domain.Message) (domain.Message, error)
	InsertOutboundMessage(ctx context.Context, m domain.Message) (domain.Message, error)
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

// RouterConfig wires the router's dependencies.
type RouterConfig struct {
	Store          RouterStore
	Binding        BindingService
	Tasks          TaskService
	Transport      transport.BotTransportAdapter
	Commands       CommandRegistry
	Messages       MessageStore
	ToolPolicy     string
	SourceInstance string
	// Teams is the team lifecycle service. Optional in P0; when nil, team
	// commands reply with "功能未启用" and messages always route to personal.
	Teams TeamService
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
	binding        BindingService
	tasks          TaskService
	transport      transport.BotTransportAdapter
	commands       CommandRegistry
	messages       MessageStore
	teams          TeamService
	relay          RelayService
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
	if cfg.Store == nil || cfg.Binding == nil || cfg.Tasks == nil || cfg.Transport == nil {
		return nil, fmt.Errorf("store, binding, tasks, and transport are required")
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
		binding:        cfg.Binding,
		tasks:          cfg.Tasks,
		transport:      cfg.Transport,
		commands:       cfg.Commands,
		messages:       cfg.Messages,
		teams:          cfg.Teams,
		relay:          cfg.Relay,
		toolPolicy:     cfg.ToolPolicy,
		sourceInstance: cfg.SourceInstance,
	}, nil
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
	// Persist inbound message for history/audit and as the cross-instance
	// idempotency backstop. The partial UNIQUE(bot_id, message_id) index
	// rejects duplicates when the in-memory seen map is cold (restart) or
	// split across instances. Persistence failure is non-fatal: the message
	// is still routed so the user is not blocked; the missing audit row is
	// acceptable, but we log loudly so DB issues surface fast.
	if _, perr := r.persistInbound(ctx, msg, bot); perr != nil {
		if errors.Is(perr, domain.ErrDuplicateInboundMessage) {
			return RouterResult{Action: ActionDuplicate, Reply: "duplicate message ignored"}, nil
		}
		slog.ErrorContext(ctx, "router: persist inbound message failed",
			"message_id", msg.MessageID,
			"bot_uuid", msg.BotUUID,
			"error", perr)
	}
	// Unbound bot: only /activate is allowed (spec §6.1 step 2).
	if !bot.IsBound() {
		return r.handleUnboundMessage(ctx, msg, bot)
	}
	// Bound bot: verify from_user_id matches (spec §6.1 step 2).
	if bot.IlinkUserID != msg.IlinkUserID {
		reply := "identity mismatch"
		_ = r.transport.SendMessage(ctx, msg.BotUUID, msg.IlinkUserID, reply)
		return RouterResult{Action: ActionRejected, Reply: reply}, nil
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

// handleUnboundMessage processes a message from an unbound bot (only /activate).
func (r *router) handleUnboundMessage(ctx context.Context, msg IncomingMessage, bot domain.Bot) (RouterResult, error) {
	code, ok := parseActivateCommand(msg.Text)
	if !ok {
		reply := "bot not bound; send /activate <code> to pair"
		_ = r.transport.SendMessage(ctx, msg.BotUUID, msg.IlinkUserID, reply)
		return RouterResult{Action: ActionRejected, Reply: reply, UserID: bot.OwnerID}, nil
	}
	_, err := r.binding.Activate(ctx, code, msg.BotUUID, msg.IlinkUserID)
	if err != nil {
		reply := fmt.Sprintf("activation failed: %s", err.Error())
		_ = r.transport.SendMessage(ctx, msg.BotUUID, msg.IlinkUserID, reply)
		return RouterResult{Action: ActionRejected, Reply: reply, UserID: bot.OwnerID}, nil
	}
	reply := "binding successful; you can now send messages"
	_ = r.transport.SendMessage(ctx, msg.BotUUID, msg.IlinkUserID, reply)
	return RouterResult{Action: ActionActivated, Reply: reply, UserID: bot.OwnerID}, nil
}

// routeBoundMessage parses platform-level commands and routes everything else
// as a task to the Worker (spec §6.2).
//
// Design principle: the control plane intercepts ONLY commands that affect
// platform-owned state (tasks, bindings, model policy). Unknown /xxx commands
// and normal text are forwarded as tasks — the Worker/GA decides whether they
// are valid agent commands. This keeps the platform decoupled from GA's
// command surface.
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
	if !found || cmd.Action == domain.CommandPassthrough {
		return r.handleNormalMessage(ctx, msg, bot, text)
	}
	return r.dispatchHandler(ctx, msg, bot, cmd.Handler, text)
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
	case "llm":
		return r.handleLLM(ctx, msg, bot)
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
		// Unknown handler key in DB → treat as passthrough (safe default).
		return r.handleNormalMessage(ctx, msg, bot, text)
	}
}

// resolveCommand checks if text matches an enabled command in the registry.
// Exact match is tried first; if that fails, a prefix match on the first
// token is attempted so that "/团队 项目组" resolves to the "/团队" command.
// Returns the command entry and whether it was found.
func (r *router) resolveCommand(ctx context.Context, text string) (domain.PlatformCommand, bool) {
	commands := r.loadCommands(ctx)
	if cmd, ok := commands[text]; ok {
		return cmd, true
	}
	if idx := strings.IndexByte(text, ' '); idx > 0 {
		if cmd, ok := commands[text[:idx]]; ok {
			return cmd, true
		}
	}
	return domain.PlatformCommand{}, false
}

// InvalidateCommandCache clears the command registry cache. Called by admin
// update handlers after changing platform_commands so the next message
// reloads the latest configuration. Single-instance deployment only; for
// multi-instance, use PostgreSQL LISTEN/NOTIFY or a message bus.
func (r *router) InvalidateCommandCache() {
	r.cacheMu.Lock()
	defer r.cacheMu.Unlock()
	r.cachedCommands = nil
	r.cacheLoaded = false
}

// loadCommands returns the cached command map, loading once and reloading
// only after InvalidateCommandCache() is called. When CommandRegistry is nil
// (unit tests), uses built-in defaults.
func (r *router) loadCommands(ctx context.Context) map[string]domain.PlatformCommand {
	if r.commands == nil {
		return defaultCommands()
	}
	r.cacheMu.Lock()
	defer r.cacheMu.Unlock()
	if r.cacheLoaded {
		return r.cachedCommands
	}
	list, err := r.commands.ListEnabledCommands(ctx)
	if err != nil || len(list) == 0 {
		// DB error or empty registry: use defaults (fail-safe).
		r.cachedCommands = defaultCommands()
		r.cacheLoaded = true
		return r.cachedCommands
	}
	m := make(map[string]domain.PlatformCommand, len(list))
	for _, c := range list {
		m[c.Command] = c
	}
	r.cachedCommands = m
	r.cacheLoaded = true
	return m
}

// defaultCommands is the built-in fallback when no CommandRegistry is wired
// (e.g. unit tests). Matches the seed data in migrations 0004 + 0016.
func defaultCommands() map[string]domain.PlatformCommand {
	defaults := []domain.PlatformCommand{
		{Command: "/help", Action: domain.CommandIntercept, Handler: "help"},
		{Command: "/status", Action: domain.CommandIntercept, Handler: "status"},
		{Command: "/stop", Action: domain.CommandIntercept, Handler: "stop"},
		{Command: "/new", Action: domain.CommandIntercept, Handler: "new"},
		{Command: "/llm", Action: domain.CommandIntercept, Handler: "llm"},
		{Command: "/我的身份", Action: domain.CommandIntercept, Handler: "identity"},
		{Command: "/个人", Action: domain.CommandIntercept, Handler: "personal"},
		{Command: "/团队", Action: domain.CommandIntercept, Handler: "team"},
		{Command: "/邀请码", Action: domain.CommandIntercept, Handler: "invite_code"},
		{Command: "/同意", Action: domain.CommandIntercept, Handler: "accept"},
		{Command: "/批准", Action: domain.CommandIntercept, Handler: "approve"},
		{Command: "/拒绝", Action: domain.CommandIntercept, Handler: "reject"},
		{Command: "/移除", Action: domain.CommandIntercept, Handler: "remove"},
		{Command: "/relay_off", Action: domain.CommandIntercept, Handler: "relay_off"},
		{Command: "/relay_on", Action: domain.CommandIntercept, Handler: "relay_on"},
	}
	m := make(map[string]domain.PlatformCommand, len(defaults))
	for _, c := range defaults {
		m[c.Command] = c
	}
	return m
}

func (r *router) handleStop(ctx context.Context, msg IncomingMessage, bot domain.Bot) (RouterResult, error) {
	sessionKey, err := r.resolveSessionKey(ctx, bot.OwnerID)
	if err != nil {
		return RouterResult{}, fmt.Errorf("resolve session: %w", err)
	}
	task, err := r.store.FindRunningTaskBySession(ctx, sessionKey)
	if errors.Is(err, pgx.ErrNoRows) {
		reply := "no running task to stop"
		_ = r.transport.SendMessage(ctx, msg.BotUUID, msg.IlinkUserID, reply)
		return RouterResult{Action: ActionNoRunning, Reply: reply, UserID: bot.OwnerID}, nil
	}
	if err != nil {
		return RouterResult{}, fmt.Errorf("find running task: %w", err)
	}
	if _, err := r.tasks.CancelTask(ctx, task.ID, bot.OwnerID); err != nil {
		reply := fmt.Sprintf("cancel failed: %s", err.Error())
		_ = r.transport.SendMessage(ctx, msg.BotUUID, msg.IlinkUserID, reply)
		return RouterResult{Action: ActionRejected, Reply: reply, UserID: bot.OwnerID}, nil
	}
	reply := "task cancelled"
	_ = r.transport.SendMessage(ctx, msg.BotUUID, msg.IlinkUserID, reply)
	return RouterResult{Action: ActionStopped, Reply: reply, UserID: bot.OwnerID}, nil
}

// handleNew implements /new: cancels any running task for the session and
// marks the workspace for fresh start. The next submitted task carries
// fresh_session=true, causing the scheduler to restart the Worker without
// loading the prior snapshot (spec §7 /new).
func (r *router) handleNew(ctx context.Context, msg IncomingMessage, bot domain.Bot) (RouterResult, error) {
	sessionKey, err := r.resolveSessionKey(ctx, bot.OwnerID)
	if err != nil {
		return RouterResult{}, fmt.Errorf("resolve session: %w", err)
	}
	if task, err := r.store.FindRunningTaskBySession(ctx, sessionKey); err == nil {
		if _, err := r.tasks.CancelTask(ctx, task.ID, bot.OwnerID); err != nil {
			slog.ErrorContext(ctx, "router: /new cancel running task failed",
				"task_id", task.ID, "error", err)
		}
	}
	if err := r.store.ResetWorkspace(ctx, sessionKey); err != nil {
		return RouterResult{}, fmt.Errorf("reset workspace: %w", err)
	}
	reply := "已开启新会话，history 和 working 已清空"
	_ = r.transport.SendMessage(ctx, msg.BotUUID, msg.IlinkUserID, reply)
	return RouterResult{Action: ActionNewSession, Reply: reply, UserID: bot.OwnerID}, nil
}

// handleStatus reports whether there is a running task for the user's session.
func (r *router) handleStatus(ctx context.Context, msg IncomingMessage, bot domain.Bot) (RouterResult, error) {
	sessionKey, err := r.resolveSessionKey(ctx, bot.OwnerID)
	if err != nil {
		return RouterResult{}, fmt.Errorf("resolve session: %w", err)
	}
	_, err = r.store.FindRunningTaskBySession(ctx, sessionKey)
	var reply string
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		reply = "🟢 idle — no running task"
	case err != nil:
		return RouterResult{}, fmt.Errorf("status check: %w", err)
	default:
		reply = "🔴 running — task in progress"
	}
	_ = r.transport.SendMessage(ctx, msg.BotUUID, msg.IlinkUserID, reply)
	return RouterResult{Action: ActionStatus, Reply: reply, UserID: bot.OwnerID}, nil
}

// handleHelp returns the list of platform-level commands, generated from the
// admin-configurable command registry (not hardcoded).
func (r *router) handleHelp(ctx context.Context, msg IncomingMessage, bot domain.Bot) (RouterResult, error) {
	commands := r.loadCommands(ctx)
	reply := buildHelpText(commands)
	_ = r.transport.SendMessage(ctx, msg.BotUUID, msg.IlinkUserID, reply)
	return RouterResult{Action: ActionHelp, Reply: reply, UserID: bot.OwnerID}, nil
}

// buildHelpText formats the command list from the registry into a help message.
func buildHelpText(commands map[string]domain.PlatformCommand) string {
	var sb strings.Builder
	sb.WriteString("📖 平台命令:\n")
	for _, cmd := range commands {
		if cmd.Action == domain.CommandIntercept {
			help := cmd.HelpText
			if help == "" {
				help = cmd.Handler
			}
			sb.WriteString(fmt.Sprintf("%s - %s\n", cmd.Command, help))
		}
	}
	sb.WriteString("\n其他消息或 /xxx 命令将作为任务发送给 Agent")
	return sb.String()
}

// handleLLM reports model policy info. Model selection is controlled by the
// platform's LLM Proxy (spec §44: real Key only in control plane); the Worker
// cannot self-select models. The actual model list comes from LLM Proxy policy.
func (r *router) handleLLM(ctx context.Context, msg IncomingMessage, bot domain.Bot) (RouterResult, error) {
	reply := fmt.Sprintf("模型由平台 LLM Proxy 管控（当前策略版本: %s）\nWorker 不可自选模型", r.toolPolicy)
	_ = r.transport.SendMessage(ctx, msg.BotUUID, msg.IlinkUserID, reply)
	return RouterResult{Action: ActionModelInfo, Reply: reply, UserID: bot.OwnerID}, nil
}

func (r *router) handleNormalMessage(ctx context.Context, msg IncomingMessage, bot domain.Bot, text string) (RouterResult, error) {
	if text == "" && len(msg.MediaPaths) == 0 {
		reply := "empty message ignored"
		_ = r.transport.SendMessage(ctx, msg.BotUUID, msg.IlinkUserID, reply)
		return RouterResult{Action: ActionRejected, Reply: reply, UserID: bot.OwnerID}, nil
	}
	// Surface inbound media paths in the prompt so GA's file tools can read them.
	// This keeps the SubmitTaskCommand proto stable while making media reachable
	// to the worker. If text is empty (pure media message), use a placeholder.
	prompt := text
	if len(msg.MediaPaths) > 0 {
		if prompt == "" {
			prompt = "[media message]"
		}
		prompt = prompt + "\n\n[Attached files: " + strings.Join(msg.MediaPaths, ", ") + "]"
	}
	// Per-user tool policy (migration 0005): resolve the user's assigned policy
	// version, not a global default. Admins can grant different capabilities
	// to different users at runtime.
	userPolicy, err := r.store.GetUserToolPolicy(ctx, bot.OwnerID)
	if err != nil {
		return RouterResult{}, fmt.Errorf("resolve user tool policy: %w", err)
	}
	if userPolicy == "" {
		userPolicy = r.toolPolicy // fallback to global default
	}
	sessionKey, err := r.resolveSessionKey(ctx, bot.OwnerID)
	if err != nil {
		return RouterResult{}, fmt.Errorf("resolve session: %w", err)
	}
	task, err := r.tasks.SubmitTask(ctx, domain.SubmitTaskCommand{
		SessionKey:        sessionKey,
		RequesterUserID:   bot.OwnerID,
		Source:            domain.SourceWechat,
		SourceInstanceID:  r.sourceInstance,
		MessageID:         msg.MessageID,
		Prompt:            prompt,
		PersonaSnapshot:   []string{},
		ToolPolicyVersion: userPolicy,
	})
	if err != nil {
		reply := fmt.Sprintf("task submission failed: %s", err.Error())
		_ = r.transport.SendMessage(ctx, msg.BotUUID, msg.IlinkUserID, reply)
		return RouterResult{Action: ActionRejected, Reply: reply, UserID: bot.OwnerID}, nil
	}
	reply := fmt.Sprintf("task %s queued", task.ID)
	_ = r.transport.SendMessage(ctx, msg.BotUUID, msg.IlinkUserID, reply)
	return RouterResult{Action: ActionTaskCreated, Reply: reply, UserID: bot.OwnerID}, nil
}

// parseActivateCommand extracts the code from "/activate <code>".
// Returns the code and true if the message matches; empty and false otherwise.
func parseActivateCommand(text string) (string, bool) {
	text = strings.TrimSpace(text)
	if !strings.HasPrefix(text, "/activate") {
		return "", false
	}
	rest := strings.TrimSpace(strings.TrimPrefix(text, "/activate"))
	if rest == "" {
		return "", false
	}
	return rest, true
}

// personalSessionKey returns the session key for a user's personal workspace.
func personalSessionKey(userID int64) string {
	return fmt.Sprintf("personal:%d", userID)
}

// persistInbound writes the received message to the message store and inserts
// a media_assets row for each media item. The first media path (if any) is
// persisted alongside the text body for quick access. Media asset insertions
// are best-effort: failures are logged but do not block message routing, and
// duplicates (ErrDuplicateMediaAsset) are silent successes (the cross-instance
// UNIQUE constraint already recorded the file).
func (r *router) persistInbound(ctx context.Context, msg IncomingMessage, bot domain.Bot) (domain.Message, error) {
	mediaPath := ""
	if len(msg.MediaPaths) > 0 {
		mediaPath = msg.MediaPaths[0]
	}
	msgRow, err := r.messages.InsertInboundMessage(ctx, domain.Message{
		UserID:      bot.OwnerID,
		BotID:       bot.ID,
		SessionKey:  personalSessionKey(bot.OwnerID),
		MessageID:   msg.MessageID,
		MessageType: inferMessageType(msg.MediaPaths),
		Content:     msg.Text,
		MediaPath:   mediaPath,
	})
	if err != nil {
		return msgRow, err
	}
	// Insert media_assets metadata. Idempotent (UNIQUE on message_id +
	// storage_path). Failure is non-fatal: the message is already persisted
	// and routed; a missing media audit row is acceptable.
	for _, item := range msg.MediaItems {
		_, merr := r.messages.InsertMediaAsset(ctx, domain.MediaAsset{
			UserID:      bot.OwnerID,
			BotID:       bot.ID,
			MessageID:   msgRow.ID,
			FileName:    item.FileName,
			StoragePath: item.StoragePath,
			ContentType: item.ContentType,
			SizeBytes:   item.Size,
			Direction:   domain.MessageInbound,
		})
		if merr != nil && !errors.Is(merr, domain.ErrDuplicateMediaAsset) {
			slog.ErrorContext(ctx, "router: persist media asset failed",
				"storage_path", item.StoragePath,
				"file_name", item.FileName,
				"error", merr)
		}
	}
	return msgRow, nil
}

// inferMessageType maps media presence to a coarse type. The Poller does not
// currently forward the iLink item type, so all media is labelled "file".
// Upgrading the webhook body to carry item type will enable image/voice/video.
func inferMessageType(mediaPaths []string) string {
	if len(mediaPaths) == 0 {
		return domain.MessageTypeText
	}
	return domain.MessageTypeFile
}

// nowFunc is overridable for tests.
var nowFunc = func() time.Time { return time.Now().UTC() }
