package application

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/domain"
)

// Relay relay-at-mention format: "@<username> <message body>".
// The username runs from the char after '@' up to the first whitespace. This
// matches the common @-mention convention (Slack, Discord, WeChat groups)
// and keeps parsing side-effect-free (no DB lookup needed to extract the
// name). Messages without a body (bare "@alice") are rejected with usage.
const relayAtPrefix = "@"

// parseRelayMention splits "@<username> <body>" into its parts. Returns
// ok=false when text does not start with '@' or has no body after the
// username. The username is trimmed of surrounding whitespace; the body is
// trimmed of trailing whitespace.
func parseRelayMention(text string) (username, body string, ok bool) {
	if !strings.HasPrefix(text, relayAtPrefix) {
		return "", "", false
	}
	rest := strings.TrimPrefix(text, relayAtPrefix)
	// Username ends at the first whitespace (space, tab, newline).
	spaceIdx := strings.IndexAny(rest, " \t\n\r")
	if spaceIdx <= 0 {
		return "", "", false
	}
	username = strings.TrimSpace(rest[:spaceIdx])
	body = strings.TrimSpace(rest[spaceIdx:])
	if username == "" || body == "" {
		return "", "", false
	}
	return username, body, true
}

// handleRelay intercepts "@<username> <text>" and forwards the text to the
// named user's WeChat via their bound bot. No LLM is involved. When the
// RelayService is nil (feature disabled), falls back to normal task routing
// so the agent can still handle the message.
func (r *router) handleRelay(ctx context.Context, msg IncomingMessage, bot domain.Bot, text string) (RouterResult, error) {
	if r.relay == nil {
		// Feature disabled: treat as a normal message so the agent can
		// respond. This keeps @-text usable in loopback/dev mode.
		return r.handleNormalMessage(ctx, msg, bot, text)
	}
	username, body, ok := parseRelayMention(text)
	if !ok {
		reply := "用法：@<用户名> <消息内容>\n例：@alice 你好"
		_ = r.transport.SendMessage(ctx, msg.BotUUID, msg.IlinkUserID, reply, "")
		return RouterResult{Action: ActionRejected, Reply: reply, UserID: bot.OwnerID}, nil
	}
	if err := r.relay.Relay(ctx, bot.OwnerID, username, body); err != nil {
		reply := formatRelayError(err, username)
		_ = r.transport.SendMessage(ctx, msg.BotUUID, msg.IlinkUserID, reply, "")
		return RouterResult{Action: ActionRejected, Reply: reply, UserID: bot.OwnerID}, nil
	}
	reply := fmt.Sprintf("已转发给 %s", username)
	_ = r.transport.SendMessage(ctx, msg.BotUUID, msg.IlinkUserID, reply, "")
	return RouterResult{Action: ActionReplied, Reply: reply, UserID: bot.OwnerID}, nil
}

// handleRelayOff disables relay reception for the sender.
func (r *router) handleRelayOff(ctx context.Context, msg IncomingMessage, bot domain.Bot) (RouterResult, error) {
	if r.relay == nil {
		reply := "转发功能未启用"
		_ = r.transport.SendMessage(ctx, msg.BotUUID, msg.IlinkUserID, reply, "")
		return RouterResult{Action: ActionReplied, Reply: reply, UserID: bot.OwnerID}, nil
	}
	if err := r.relay.SetOptOut(ctx, bot.OwnerID, true); err != nil {
		reply := fmt.Sprintf("操作失败: %s", err.Error())
		_ = r.transport.SendMessage(ctx, msg.BotUUID, msg.IlinkUserID, reply, "")
		return RouterResult{Action: ActionRejected, Reply: reply, UserID: bot.OwnerID}, nil
	}
	reply := "已关闭 @用户名 转发接收\n发送 /relay_on 重新开启"
	_ = r.transport.SendMessage(ctx, msg.BotUUID, msg.IlinkUserID, reply, "")
	return RouterResult{Action: ActionReplied, Reply: reply, UserID: bot.OwnerID}, nil
}

// handleRelayOn re-enables relay reception for the sender.
func (r *router) handleRelayOn(ctx context.Context, msg IncomingMessage, bot domain.Bot) (RouterResult, error) {
	if r.relay == nil {
		reply := "转发功能未启用"
		_ = r.transport.SendMessage(ctx, msg.BotUUID, msg.IlinkUserID, reply, "")
		return RouterResult{Action: ActionReplied, Reply: reply, UserID: bot.OwnerID}, nil
	}
	if err := r.relay.SetOptOut(ctx, bot.OwnerID, false); err != nil {
		reply := fmt.Sprintf("操作失败: %s", err.Error())
		_ = r.transport.SendMessage(ctx, msg.BotUUID, msg.IlinkUserID, reply, "")
		return RouterResult{Action: ActionRejected, Reply: reply, UserID: bot.OwnerID}, nil
	}
	reply := "已开启 @用户名 转发接收"
	_ = r.transport.SendMessage(ctx, msg.BotUUID, msg.IlinkUserID, reply, "")
	return RouterResult{Action: ActionReplied, Reply: reply, UserID: bot.OwnerID}, nil
}

// formatRelayError maps relay service errors to user-facing replies. The
// username is included so the sender knows which target failed.
func formatRelayError(err error, username string) string {
	switch {
	case errors.Is(err, domain.ErrRelayUserNotFound):
		return fmt.Sprintf("用户 %s 不存在", username)
	case errors.Is(err, domain.ErrRelaySelfTarget):
		return "不能给自己转发消息"
	case errors.Is(err, domain.ErrRelayUserNotApproved):
		return fmt.Sprintf("用户 %s 未激活", username)
	case errors.Is(err, domain.ErrRelayOptedOut):
		return fmt.Sprintf("用户 %s 已关闭转发接收", username)
	case errors.Is(err, domain.ErrRelayUserNotBound):
		return fmt.Sprintf("用户 %s 未绑定微信", username)
	case errors.Is(err, domain.ErrRelayEmptyMessage):
		return "消息内容不能为空"
	case errors.Is(err, domain.ErrRelaySenderUnknown):
		return "无法解析发送者身份"
	default:
		return fmt.Sprintf("转发失败: %s", err.Error())
	}
}
