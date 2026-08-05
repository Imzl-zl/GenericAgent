package application

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
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
// (e.g. unit tests). It contains only user-safe platform commands.
func defaultCommands() map[string]domain.PlatformCommand {
	defaults := []domain.PlatformCommand{
		{Command: "/help", Action: domain.CommandIntercept, Handler: "help", HelpText: "显示帮助", SortOrder: 1},
		{Command: "/status", Action: domain.CommandIntercept, Handler: "status", HelpText: "查看任务状态", SortOrder: 2},
		{Command: "/stop", Action: domain.CommandIntercept, Handler: "stop", HelpText: "停止当前任务", SortOrder: 3},
		{Command: "/abort", Action: domain.CommandIntercept, Handler: "stop", HelpText: "停止当前任务（/stop 别名）", SortOrder: 4},
		{Command: "/new", Action: domain.CommandIntercept, Handler: "new", HelpText: "开启新对话", SortOrder: 5},
		{Command: "/我的身份", Action: domain.CommandIntercept, Handler: "identity", HelpText: "查看当前身份和上下文", SortOrder: 10},
		{Command: "/个人", Action: domain.CommandIntercept, Handler: "personal", HelpText: "切换到个人助手上下文", SortOrder: 11},
		{Command: "/团队", Action: domain.CommandIntercept, Handler: "team", HelpText: "进入团队上下文或列出团队", SortOrder: 12},
		{Command: "/邀请码", Action: domain.CommandIntercept, Handler: "invite_code", HelpText: "提交团队邀请码", SortOrder: 13},
		{Command: "/同意", Action: domain.CommandIntercept, Handler: "accept", HelpText: "同意团队邀请", SortOrder: 14},
		{Command: "/批准", Action: domain.CommandIntercept, Handler: "approve", HelpText: "批准成员加入", SortOrder: 15},
		{Command: "/拒绝", Action: domain.CommandIntercept, Handler: "reject", HelpText: "拒绝团队邀请", SortOrder: 16},
		{Command: "/移除", Action: domain.CommandIntercept, Handler: "remove", HelpText: "移除团队成员", SortOrder: 17},
		{Command: "/relay_off", Action: domain.CommandIntercept, Handler: "relay_off", HelpText: "关闭消息转发", SortOrder: 20},
		{Command: "/relay_on", Action: domain.CommandIntercept, Handler: "relay_on", HelpText: "开启消息转发", SortOrder: 21},
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
		_ = r.transport.SendMessage(ctx, msg.BotUUID, msg.IlinkUserID, reply, "")
		return RouterResult{Action: ActionNoRunning, Reply: reply, UserID: bot.OwnerID}, nil
	}
	if err != nil {
		return RouterResult{}, fmt.Errorf("find running task: %w", err)
	}
	if _, err := r.tasks.CancelTask(ctx, task.ID, bot.OwnerID); err != nil {
		reply := fmt.Sprintf("cancel failed: %s", err.Error())
		_ = r.transport.SendMessage(ctx, msg.BotUUID, msg.IlinkUserID, reply, "")
		return RouterResult{Action: ActionRejected, Reply: reply, UserID: bot.OwnerID}, nil
	}
	reply := "task cancelled"
	_ = r.transport.SendMessage(ctx, msg.BotUUID, msg.IlinkUserID, reply, "")
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
	if _, err := r.store.ResetWorkspaceForNewSession(ctx, sessionKey); err != nil {
		return RouterResult{}, fmt.Errorf("reset workspace: %w", err)
	}
	reply := "已开启新会话，history 和 working 已清空"
	_ = r.transport.SendMessage(ctx, msg.BotUUID, msg.IlinkUserID, reply, "")
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
	_ = r.transport.SendMessage(ctx, msg.BotUUID, msg.IlinkUserID, reply, "")
	return RouterResult{Action: ActionStatus, Reply: reply, UserID: bot.OwnerID}, nil
}

// handleHelp returns the list of platform-level commands, generated from the
// admin-configurable command registry (not hardcoded).
func (r *router) handleHelp(ctx context.Context, msg IncomingMessage, bot domain.Bot) (RouterResult, error) {
	commands := r.loadCommands(ctx)
	reply := buildHelpText(commands)
	_ = r.transport.SendMessage(ctx, msg.BotUUID, msg.IlinkUserID, reply, "")
	return RouterResult{Action: ActionHelp, Reply: reply, UserID: bot.OwnerID}, nil
}

// buildHelpText formats the command list from the registry into a help message.
func buildHelpText(commands map[string]domain.PlatformCommand) string {
	visible := make([]domain.PlatformCommand, 0, len(commands))
	for _, cmd := range commands {
		if cmd.Action == domain.CommandIntercept && !isRestrictedUserCommand(cmd.Command) {
			visible = append(visible, cmd)
		}
	}
	sort.Slice(visible, func(i, j int) bool {
		if visible[i].SortOrder == visible[j].SortOrder {
			return visible[i].Command < visible[j].Command
		}
		return visible[i].SortOrder < visible[j].SortOrder
	})

	var sb strings.Builder
	sb.WriteString("📖 可用命令:\n")
	for _, cmd := range visible {
		help := cmd.HelpText
		if help == "" {
			help = cmd.Handler
		}
		sb.WriteString(fmt.Sprintf("%s - %s\n", cmd.Command, help))
	}
	sb.WriteString("\n未列出的 /xxx 命令不可用")
	return sb.String()
}

// handleNormalMessage forwards a non-command text/media payload as a task to
// the Worker via TaskService.SubmitTask.
func (r *router) handleNormalMessage(ctx context.Context, msg IncomingMessage, bot domain.Bot, text string, inboundSessionKey string) (*domain.Message, RouterResult, error) {
	if text == "" && len(msg.MediaPaths) == 0 {
		reply := "empty message ignored"
		_ = r.transport.SendMessage(ctx, msg.BotUUID, msg.IlinkUserID, reply, "")
		return nil, RouterResult{Action: ActionRejected, Reply: reply, UserID: bot.OwnerID}, nil
	}
	prompt := text
	if prompt == "" && len(msg.MediaPaths) > 0 {
		prompt = "[media message]"
	}
	// Per-user tool policy (migration 0005): resolve the user's assigned policy
	// version, not a global default. Admins can grant different capabilities
	// to different users at runtime.
	userPolicy, err := r.store.GetUserToolPolicy(ctx, bot.OwnerID)
	if err != nil {
		return nil, RouterResult{}, fmt.Errorf("resolve user tool policy: %w", err)
	}
	if userPolicy == "" {
		userPolicy = r.toolPolicy // fallback to global default
	}
	sessionKey, err := r.resolveSessionKey(ctx, bot.OwnerID)
	if err != nil {
		return nil, RouterResult{}, fmt.Errorf("resolve session: %w", err)
	}
	// round11 审查(I1): importedRefs 记录本次导入的附件, 任务提交失败时
	// 回滚(附件写入先于授权/幂等事务, 失败必须清理防止未授权残留)。
	var importedRefs []SessionFileRef
	if r.sessionFiles != nil {
		currentRefs, err := r.sessionFiles.ImportInbound(sessionKey, msg.MediaPaths)
		if err != nil {
			return nil, RouterResult{}, fmt.Errorf("stage session files: %w", err)
		}
		importedRefs = currentRefs
		recentRefs, err := r.sessionFiles.Recent(sessionKey, 8)
		if err != nil {
			return nil, RouterResult{}, fmt.Errorf("list session files: %w", err)
		}
		if hint := sessionFilesPrompt(currentRefs, recentRefs); hint != "" {
			if prompt != "" {
				prompt += "\n\n"
			}
			prompt += hint
		}
		if userPolicy == "foundation.no-host-tools.v1" {
			userPolicy = "foundation.session-files.v1"
		}
	} else if len(msg.MediaPaths) > 0 {
		if prompt != "" {
			prompt += "\n\n"
		}
		prompt += "[Attached files: " + strings.Join(msg.MediaPaths, ", ") + "]"
	}
	// round10 审查(B7): 消息行与任务在同一 DB 事务内提交——消息行冲突(23505)
	// 时返回 ErrDuplicateInboundMessage 由调用方短路, 任务冲突时返回已有任务;
	// 崩溃/并发下任务既不重复也不丢失, 消息审计与任务状态原子一致。
	var msgRow *domain.Message
	if r.messages != nil {
		_, row, err := r.tasks.SubmitTaskWithInboundMessage(ctx, domain.SubmitTaskCommand{
			SessionKey:        sessionKey,
			RequesterUserID:   bot.OwnerID,
			Source:            domain.SourceWechat,
			SourceInstanceID:  r.sourceInstance,
			MessageID:         msg.MessageID,
			Prompt:            prompt,
			PersonaSnapshot:   []string{},
			ToolPolicyVersion: userPolicy,
		}, domain.Message{
			UserID:      bot.OwnerID,
			BotID:       bot.ID,
			SessionKey:  inboundSessionKey,
			MessageID:   msg.MessageID,
			MessageType: inferMessageType(msg.MediaPaths),
			Content:     msg.Text,
		})
		if err != nil {
			// Round8 审查: 提交失败必须返回 error(而非 Rejected 200)——Poller
			// 收到 5xx 后重试; 同事务内消息行随任务一起回滚, 重试可完整重放,
			// 任务由唯一键兜底不重复。
			// round11 审查(I1): 附件写入先于本事务(授权/幂等检查在其中),
			// 提交失败必须回滚本次导入的附件, 防止未授权或重复消息残留文件。
			r.rollbackImportedAttachments(ctx, sessionKey, importedRefs)
			return nil, RouterResult{}, fmt.Errorf("submit task: %w", err)
		}
		msgRow = &row
	} else {
		// 测试环境未接线 messages store: 退化为纯任务提交。
		if _, err := r.tasks.SubmitTask(ctx, domain.SubmitTaskCommand{
			SessionKey:        sessionKey,
			RequesterUserID:   bot.OwnerID,
			Source:            domain.SourceWechat,
			SourceInstanceID:  r.sourceInstance,
			MessageID:         msg.MessageID,
			Prompt:            prompt,
			PersonaSnapshot:   []string{},
			ToolPolicyVersion: userPolicy,
		}); err != nil {
			r.rollbackImportedAttachments(ctx, sessionKey, importedRefs)
			return nil, RouterResult{}, fmt.Errorf("submit task: %w", err)
		}
	}
	// Normal message acceptance is delivered through the durable task_started
	// outbox path so users get exactly one acknowledgment instead of a sync
	// "收到" plus an async "正在处理" duplicate.
	reply := "✓ 收到，正在处理您的任务..."
	return msgRow, RouterResult{Action: ActionTaskCreated, Reply: reply, UserID: bot.OwnerID}, nil
}

// rollbackImportedAttachments 回滚本次消息导入的附件(best-effort,
// round11 审查 I1): 附件写入先于任务授权/幂等事务, 提交失败(成员被移除/
// 重复消息/DB 错误)时删除本次导入的文件, 防止团队 workspace 残留未授权
// 附件。失败只记日志, 不掩盖原始提交错误。
func (r *router) rollbackImportedAttachments(ctx context.Context, sessionKey string, refs []SessionFileRef) {
	if len(refs) == 0 || r.sessionFiles == nil {
		return
	}
	if err := r.sessionFiles.RemoveInbound(sessionKey, refs); err != nil {
		slog.ErrorContext(ctx, "router: rollback imported attachments failed",
			"session_key", sessionKey, "error", err)
	}
}
