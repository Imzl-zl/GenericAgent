package application

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/domain"
)

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

// handleStop implements /stop: cancels the running task for the user's
// session (personal or team) if one exists.
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

// handleNormalMessage forwards a non-command text/media payload as a task to
// the Worker via TaskService.SubmitTask.
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
