package application

import (
	"context"
	"errors"
	"fmt"
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
)

// RouterResult is the outcome of processing an incoming message.
type RouterResult struct {
	Action  RouterAction
	Reply   string
	UserID  int64
}

// IncomingMessage is a message received from a bot transport.
type IncomingMessage struct {
	BotUUID     string
	IlinkUserID string
	MessageID   string
	Text        string
	// MediaPaths are local file paths of inbound media downloaded by the
	// Bot Poller. Empty for text-only messages. The router surfaces them in
	// the task prompt so GA's file tools can read them.
	MediaPaths []string
}

// RouterStore is the persistence port for router identity resolution.
type RouterStore interface {
	GetBotByUUID(ctx context.Context, botUUID string) (domain.Bot, error)
	GetUserStatus(ctx context.Context, userID int64) (domain.UserStatus, error)
	GetUserToolPolicy(ctx context.Context, userID int64) (string, error)
	FindRunningTaskBySession(ctx context.Context, sessionKey string) (domain.Task, error)
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
	ToolPolicy     string
	SourceInstance string
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
	default:
		// Unknown handler key in DB → treat as passthrough (safe default).
		return r.handleNormalMessage(ctx, msg, bot, text)
	}
}

// resolveCommand checks if text matches an enabled command in the registry.
// Returns the command entry and whether it was found. Uses a TTL cache to
// avoid hitting the DB on every message; admin changes take effect within
// commandCacheTTL (60s). When CommandRegistry is nil, uses built-in defaults.
func (r *router) resolveCommand(ctx context.Context, text string) (domain.PlatformCommand, bool) {
	commands := r.loadCommands(ctx)
	cmd, ok := commands[text]
	return cmd, ok
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
// (e.g. unit tests). Matches the seed data in migration 0004.
func defaultCommands() map[string]domain.PlatformCommand {
	defaults := []domain.PlatformCommand{
		{Command: "/help", Action: domain.CommandIntercept, Handler: "help"},
		{Command: "/status", Action: domain.CommandIntercept, Handler: "status"},
		{Command: "/stop", Action: domain.CommandIntercept, Handler: "stop"},
		{Command: "/new", Action: domain.CommandIntercept, Handler: "new"},
		{Command: "/llm", Action: domain.CommandIntercept, Handler: "llm"},
	}
	m := make(map[string]domain.PlatformCommand, len(defaults))
	for _, c := range defaults {
		m[c.Command] = c
	}
	return m
}

func (r *router) handleStop(ctx context.Context, msg IncomingMessage, bot domain.Bot) (RouterResult, error) {
	sessionKey := personalSessionKey(bot.OwnerID)
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

// handleNew acknowledges a new-session request. In the platform model each
// task is independent; clearing Worker conversation history is a Worker-level
// concern handled by checkpoint/snapshot coordination, not by the router.
func (r *router) handleNew(ctx context.Context, msg IncomingMessage, bot domain.Bot) (RouterResult, error) {
	reply := "new session acknowledged"
	_ = r.transport.SendMessage(ctx, msg.BotUUID, msg.IlinkUserID, reply)
	return RouterResult{Action: ActionNewSession, Reply: reply, UserID: bot.OwnerID}, nil
}

// handleStatus reports whether there is a running task for the user's session.
func (r *router) handleStatus(ctx context.Context, msg IncomingMessage, bot domain.Bot) (RouterResult, error) {
	sessionKey := personalSessionKey(bot.OwnerID)
	_, err := r.store.FindRunningTaskBySession(ctx, sessionKey)
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
	sessionKey := personalSessionKey(bot.OwnerID)
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

// nowFunc is overridable for tests.
var nowFunc = func() time.Time { return time.Now().UTC() }
