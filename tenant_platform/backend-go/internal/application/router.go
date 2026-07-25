package application

import (
	"context"
	"errors"
	"fmt"
	"strings"
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
}

// RouterStore is the persistence port for router identity resolution.
type RouterStore interface {
	GetBotByUUID(ctx context.Context, botUUID string) (domain.Bot, error)
	GetUserStatus(ctx context.Context, userID int64) (domain.UserStatus, error)
	FindRunningTaskBySession(ctx context.Context, sessionKey string) (domain.Task, error)
}

// RouterConfig wires the router's dependencies.
type RouterConfig struct {
	Store         RouterStore
	Binding       BindingService
	Tasks         TaskService
	Transport     transport.BotTransportAdapter
	ToolPolicy    string
	SourceInstance string
}

// Router processes incoming bot messages: identity resolution, status check,
// command parsing, and task routing (spec §6.1–§6.2).
type Router interface {
	HandleMessage(ctx context.Context, msg IncomingMessage) (RouterResult, error)
}

type router struct {
	store          RouterStore
	binding        BindingService
	tasks          TaskService
	transport      transport.BotTransportAdapter
	toolPolicy     string
	sourceInstance string
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

// routeBoundMessage parses commands and routes normal messages (spec §6.2).
func (r *router) routeBoundMessage(ctx context.Context, msg IncomingMessage, bot domain.Bot) (RouterResult, error) {
	text := strings.TrimSpace(msg.Text)
	switch {
	case text == "/stop":
		return r.handleStop(ctx, msg, bot)
	case text == "/new":
		reply := "new session acknowledged"
		_ = r.transport.SendMessage(ctx, msg.BotUUID, msg.IlinkUserID, reply)
		return RouterResult{Action: ActionNewSession, Reply: reply, UserID: bot.OwnerID}, nil
	default:
		return r.handleNormalMessage(ctx, msg, bot, text)
	}
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

func (r *router) handleNormalMessage(ctx context.Context, msg IncomingMessage, bot domain.Bot, text string) (RouterResult, error) {
	if text == "" {
		reply := "empty message ignored"
		_ = r.transport.SendMessage(ctx, msg.BotUUID, msg.IlinkUserID, reply)
		return RouterResult{Action: ActionRejected, Reply: reply, UserID: bot.OwnerID}, nil
	}
	sessionKey := personalSessionKey(bot.OwnerID)
	task, err := r.tasks.SubmitTask(ctx, domain.SubmitTaskCommand{
		SessionKey:        sessionKey,
		RequesterUserID:   bot.OwnerID,
		Source:            domain.SourceWechat,
		SourceInstanceID:  r.sourceInstance,
		MessageID:         msg.MessageID,
		Prompt:            text,
		PersonaSnapshot:   []string{},
		ToolPolicyVersion: r.toolPolicy,
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
