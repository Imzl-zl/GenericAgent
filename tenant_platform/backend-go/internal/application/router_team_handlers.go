package application

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/domain"
)

// resolveSessionKey returns the session_key for the user's current active
// context. When TeamService is nil (P0 mode) or the user is in personal
// context, returns personal:{userID}. Otherwise returns team:{teamID}.
func (r *router) resolveSessionKey(ctx context.Context, userID int64) (string, error) {
	if r.teams == nil {
		return personalSessionKey(userID), nil
	}
	ac, err := r.teams.GetActiveContext(ctx, userID)
	if err != nil {
		return "", fmt.Errorf("get active context: %w", err)
	}
	return ac.SessionKey(userID), nil
}

// isPersonalContext returns true when the user is in personal context (or
// when team service is unavailable). Relay is only allowed in personal
// context; in team context @username is a normal message to the team AI.
func (r *router) isPersonalContext(ctx context.Context, userID int64) bool {
	if r.teams == nil {
		return true
	}
	ac, err := r.teams.GetActiveContext(ctx, userID)
	if err != nil {
		return false
	}
	return ac.IsPersonal()
}

// handleIdentity replies with the user's current context (personal or team).
func (r *router) handleIdentity(ctx context.Context, msg IncomingMessage, bot domain.Bot) (RouterResult, error) {
	reply := "当前上下文：个人助手"
	if r.teams != nil {
		ac, err := r.teams.GetActiveContext(ctx, bot.OwnerID)
		if err != nil {
			return RouterResult{}, fmt.Errorf("get active context: %w", err)
		}
		if !ac.IsPersonal() {
			teamName := r.teamNameByID(ctx, bot.OwnerID, *ac.TeamID)
			reply = fmt.Sprintf("当前上下文：团队【%s】\n团队ID：%s\n发送 /个人 切回个人助手", teamName, *ac.TeamID)
		}
	}
	_ = r.transport.SendMessage(ctx, msg.BotUUID, msg.IlinkUserID, reply)
	return RouterResult{Action: ActionReplied, Reply: reply, UserID: bot.OwnerID}, nil
}

// handlePersonal switches the user to personal:{userID} context.
func (r *router) handlePersonal(ctx context.Context, msg IncomingMessage, bot domain.Bot) (RouterResult, error) {
	if r.teams == nil {
		reply := "已切换到个人助手"
		_ = r.transport.SendMessage(ctx, msg.BotUUID, msg.IlinkUserID, reply)
		return RouterResult{Action: ActionReplied, Reply: reply, UserID: bot.OwnerID}, nil
	}
	if _, err := r.teams.SwitchToPersonal(ctx, bot.OwnerID); err != nil {
		return RouterResult{}, fmt.Errorf("switch to personal: %w", err)
	}
	reply := "已切换到个人助手"
	_ = r.transport.SendMessage(ctx, msg.BotUUID, msg.IlinkUserID, reply)
	return RouterResult{Action: ActionReplied, Reply: reply, UserID: bot.OwnerID}, nil
}

// handleTeam lists the user's teams when called without argument, or switches
// to the named team when called with /团队 <名称>.
func (r *router) handleTeam(ctx context.Context, msg IncomingMessage, bot domain.Bot, text string) (RouterResult, error) {
	if r.teams == nil {
		reply := "团队功能未启用"
		_ = r.transport.SendMessage(ctx, msg.BotUUID, msg.IlinkUserID, reply)
		return RouterResult{Action: ActionReplied, Reply: reply, UserID: bot.OwnerID}, nil
	}
	arg := parseCommandArg(text, "/团队")
	if arg == "" {
		return r.listUserTeams(ctx, msg, bot)
	}
	return r.switchToTeamByName(ctx, msg, bot, arg)
}

func (r *router) listUserTeams(ctx context.Context, msg IncomingMessage, bot domain.Bot) (RouterResult, error) {
	teams, err := r.teams.ListUserTeams(ctx, bot.OwnerID)
	if err != nil {
		return RouterResult{}, fmt.Errorf("list user teams: %w", err)
	}
	if len(teams) == 0 {
		reply := "你还没有加入任何团队\n使用 /邀请码 <code> 加入团队"
		_ = r.transport.SendMessage(ctx, msg.BotUUID, msg.IlinkUserID, reply)
		return RouterResult{Action: ActionReplied, Reply: reply, UserID: bot.OwnerID}, nil
	}
	var sb strings.Builder
	sb.WriteString("你的团队：\n")
	for i, t := range teams {
		sb.WriteString(fmt.Sprintf("%d. %s\n", i+1, t.Name))
	}
	sb.WriteString("\n发送 /团队 <名称> 进入团队上下文")
	_ = r.transport.SendMessage(ctx, msg.BotUUID, msg.IlinkUserID, sb.String())
	return RouterResult{Action: ActionReplied, Reply: sb.String(), UserID: bot.OwnerID}, nil
}

func (r *router) switchToTeamByName(ctx context.Context, msg IncomingMessage, bot domain.Bot, name string) (RouterResult, error) {
	teams, err := r.teams.ListUserTeams(ctx, bot.OwnerID)
	if err != nil {
		return RouterResult{}, fmt.Errorf("list user teams: %w", err)
	}
	var matched []domain.Team
	for _, t := range teams {
		// Exact team ID always wins: it is the disambiguator for duplicate
		// names, and UUIDs cannot collide with human team names.
		if t.ID == name {
			matched = []domain.Team{t}
			break
		}
		if t.Name == name {
			matched = append(matched, t)
		}
	}
	if len(matched) == 0 {
		reply := fmt.Sprintf("未找到团队【%s】\n发送 /团队 查看已加入的团队", name)
		_ = r.transport.SendMessage(ctx, msg.BotUUID, msg.IlinkUserID, reply)
		return RouterResult{Action: ActionReplied, Reply: reply, UserID: bot.OwnerID}, nil
	}
	if len(matched) > 1 {
		var sb strings.Builder
		sb.WriteString(fmt.Sprintf("找到 %d 个同名团队【%s】，请用 ID 精确切换：\n", len(matched), name))
		for i, t := range matched {
			sb.WriteString(fmt.Sprintf("%d. %s (ID: %s)\n", i+1, t.Name, t.ID))
		}
		sb.WriteString("\n发送 /团队 <ID> 进入")
		reply := sb.String()
		_ = r.transport.SendMessage(ctx, msg.BotUUID, msg.IlinkUserID, reply)
		return RouterResult{Action: ActionReplied, Reply: reply, UserID: bot.OwnerID}, nil
	}
	team := matched[0]
	if _, err := r.teams.SwitchToTeam(ctx, bot.OwnerID, team.ID); err != nil {
		if errors.Is(err, domain.ErrActiveContextBlocked) {
			reply := "你不是该团队的批准成员，无法进入"
			_ = r.transport.SendMessage(ctx, msg.BotUUID, msg.IlinkUserID, reply)
			return RouterResult{Action: ActionRejected, Reply: reply, UserID: bot.OwnerID}, nil
		}
		return RouterResult{}, fmt.Errorf("switch to team: %w", err)
	}
	// One-shot privacy notice: only on first entry into this team.
	if firstTime, _ := r.teams.NotifyContextShared(ctx, bot.OwnerID, team.ID); firstTime {
		notice := fmt.Sprintf("你已进入团队【%s】上下文。\n团队会话内容（含 key_info）对全体成员可见，请勿输入私人敏感信息。\n发送 /个人 回到私人助手。", team.Name)
		_ = r.transport.SendMessage(ctx, msg.BotUUID, msg.IlinkUserID, notice)
	}
	reply := fmt.Sprintf("已切换到团队【%s】上下文", team.Name)
	_ = r.transport.SendMessage(ctx, msg.BotUUID, msg.IlinkUserID, reply)
	return RouterResult{Action: ActionReplied, Reply: reply, UserID: bot.OwnerID}, nil
}

// handleInviteCode submits a team invite code: /邀请码 <code>.
func (r *router) handleInviteCode(ctx context.Context, msg IncomingMessage, bot domain.Bot, text string) (RouterResult, error) {
	if r.teams == nil {
		reply := "团队功能未启用"
		_ = r.transport.SendMessage(ctx, msg.BotUUID, msg.IlinkUserID, reply)
		return RouterResult{Action: ActionReplied, Reply: reply, UserID: bot.OwnerID}, nil
	}
	code := parseCommandArg(text, "/邀请码")
	if code == "" {
		reply := "用法：/邀请码 <code>"
		_ = r.transport.SendMessage(ctx, msg.BotUUID, msg.IlinkUserID, reply)
		return RouterResult{Action: ActionRejected, Reply: reply, UserID: bot.OwnerID}, nil
	}
	member, err := r.teams.SubmitInviteCode(ctx, code, bot.OwnerID)
	if err != nil {
		reply := formatTeamError(err, "提交邀请码失败")
		_ = r.transport.SendMessage(ctx, msg.BotUUID, msg.IlinkUserID, reply)
		return RouterResult{Action: ActionRejected, Reply: reply, UserID: bot.OwnerID}, nil
	}
	reply := fmt.Sprintf("申请已提交，等待团队 Owner 批准\n你的成员编号：%s", member.ShortID())
	_ = r.transport.SendMessage(ctx, msg.BotUUID, msg.IlinkUserID, reply)
	return RouterResult{Action: ActionReplied, Reply: reply, UserID: bot.OwnerID}, nil
}

// handleAcceptInvite accepts a direct owner invitation: /同意 t-456.
func (r *router) handleAcceptInvite(ctx context.Context, msg IncomingMessage, bot domain.Bot, text string) (RouterResult, error) {
	if r.teams == nil {
		reply := "团队功能未启用"
		_ = r.transport.SendMessage(ctx, msg.BotUUID, msg.IlinkUserID, reply)
		return RouterResult{Action: ActionReplied, Reply: reply, UserID: bot.OwnerID}, nil
	}
	shortID := parseCommandArg(text, "/同意")
	if shortID == "" {
		reply := "用法：/同意 t-456"
		_ = r.transport.SendMessage(ctx, msg.BotUUID, msg.IlinkUserID, reply)
		return RouterResult{Action: ActionRejected, Reply: reply, UserID: bot.OwnerID}, nil
	}
	_, err := r.teams.AcceptInvite(ctx, shortID, bot.OwnerID)
	if err != nil {
		reply := formatTeamError(err, "同意邀请失败")
		_ = r.transport.SendMessage(ctx, msg.BotUUID, msg.IlinkUserID, reply)
		return RouterResult{Action: ActionRejected, Reply: reply, UserID: bot.OwnerID}, nil
	}
	reply := "已同意邀请，等待 Owner 批准"
	_ = r.transport.SendMessage(ctx, msg.BotUUID, msg.IlinkUserID, reply)
	return RouterResult{Action: ActionReplied, Reply: reply, UserID: bot.OwnerID}, nil
}

// handleApprove approves a pending member: /批准 t-456. Owner only.
func (r *router) handleApprove(ctx context.Context, msg IncomingMessage, bot domain.Bot, text string) (RouterResult, error) {
	if r.teams == nil {
		reply := "团队功能未启用"
		_ = r.transport.SendMessage(ctx, msg.BotUUID, msg.IlinkUserID, reply)
		return RouterResult{Action: ActionReplied, Reply: reply, UserID: bot.OwnerID}, nil
	}
	shortID := parseCommandArg(text, "/批准")
	if shortID == "" {
		reply := "用法：/批准 t-456"
		_ = r.transport.SendMessage(ctx, msg.BotUUID, msg.IlinkUserID, reply)
		return RouterResult{Action: ActionRejected, Reply: reply, UserID: bot.OwnerID}, nil
	}
	_, err := r.teams.ApproveMember(ctx, shortID, bot.OwnerID)
	if err != nil {
		reply := formatTeamError(err, "批准失败")
		_ = r.transport.SendMessage(ctx, msg.BotUUID, msg.IlinkUserID, reply)
		return RouterResult{Action: ActionRejected, Reply: reply, UserID: bot.OwnerID}, nil
	}
	reply := fmt.Sprintf("成员 %s 已批准", shortID)
	_ = r.transport.SendMessage(ctx, msg.BotUUID, msg.IlinkUserID, reply)
	return RouterResult{Action: ActionReplied, Reply: reply, UserID: bot.OwnerID}, nil
}

// handleReject rejects a pending member: /拒绝 t-456. Owner only.
func (r *router) handleReject(ctx context.Context, msg IncomingMessage, bot domain.Bot, text string) (RouterResult, error) {
	if r.teams == nil {
		reply := "团队功能未启用"
		_ = r.transport.SendMessage(ctx, msg.BotUUID, msg.IlinkUserID, reply)
		return RouterResult{Action: ActionReplied, Reply: reply, UserID: bot.OwnerID}, nil
	}
	shortID := parseCommandArg(text, "/拒绝")
	if shortID == "" {
		reply := "用法：/拒绝 t-456"
		_ = r.transport.SendMessage(ctx, msg.BotUUID, msg.IlinkUserID, reply)
		return RouterResult{Action: ActionRejected, Reply: reply, UserID: bot.OwnerID}, nil
	}
	_, err := r.teams.RejectMember(ctx, shortID, bot.OwnerID)
	if err != nil {
		reply := formatTeamError(err, "拒绝失败")
		_ = r.transport.SendMessage(ctx, msg.BotUUID, msg.IlinkUserID, reply)
		return RouterResult{Action: ActionRejected, Reply: reply, UserID: bot.OwnerID}, nil
	}
	reply := fmt.Sprintf("成员 %s 已拒绝", shortID)
	_ = r.transport.SendMessage(ctx, msg.BotUUID, msg.IlinkUserID, reply)
	return RouterResult{Action: ActionReplied, Reply: reply, UserID: bot.OwnerID}, nil
}

// handleRemove removes an approved member: /移除 t-456. Owner only.
func (r *router) handleRemove(ctx context.Context, msg IncomingMessage, bot domain.Bot, text string) (RouterResult, error) {
	if r.teams == nil {
		reply := "团队功能未启用"
		_ = r.transport.SendMessage(ctx, msg.BotUUID, msg.IlinkUserID, reply)
		return RouterResult{Action: ActionReplied, Reply: reply, UserID: bot.OwnerID}, nil
	}
	shortID := parseCommandArg(text, "/移除")
	if shortID == "" {
		reply := "用法：/移除 t-456"
		_ = r.transport.SendMessage(ctx, msg.BotUUID, msg.IlinkUserID, reply)
		return RouterResult{Action: ActionRejected, Reply: reply, UserID: bot.OwnerID}, nil
	}
	_, err := r.teams.RemoveMember(ctx, shortID, bot.OwnerID)
	if err != nil {
		reply := formatTeamError(err, "移除失败")
		_ = r.transport.SendMessage(ctx, msg.BotUUID, msg.IlinkUserID, reply)
		return RouterResult{Action: ActionRejected, Reply: reply, UserID: bot.OwnerID}, nil
	}
	reply := fmt.Sprintf("成员 %s 已移除", shortID)
	_ = r.transport.SendMessage(ctx, msg.BotUUID, msg.IlinkUserID, reply)
	return RouterResult{Action: ActionReplied, Reply: reply, UserID: bot.OwnerID}, nil
}

// parseCommandArg extracts the argument after a command prefix.
// Example: parseCommandArg("/邀请码 ABC123", "/邀请码") → "ABC123".
func parseCommandArg(text, prefix string) string {
	text = strings.TrimSpace(text)
	if !strings.HasPrefix(text, prefix) {
		return ""
	}
	return strings.TrimSpace(strings.TrimPrefix(text, prefix))
}

// formatTeamError maps domain errors to user-facing messages.
func formatTeamError(err error, fallback string) string {
	switch {
	case errors.Is(err, domain.ErrTeamNotFound):
		return "团队不存在"
	case errors.Is(err, domain.ErrMemberNotFound):
		return "成员不存在或编号无效"
	case errors.Is(err, domain.ErrAlreadyMember):
		return "已是团队成员"
	case errors.Is(err, domain.ErrInviteCodeInvalid):
		return "邀请码无效或已过期"
	case errors.Is(err, domain.ErrNotTeamOwner):
		return "仅团队 Owner 可执行此操作"
	case errors.Is(err, domain.ErrWrongMemberStatus):
		return "成员状态不允许此操作"
	case errors.Is(err, domain.ErrActiveContextBlocked):
		return "你不是该团队的批准成员"
	default:
		return fmt.Sprintf("%s: %s", fallback, err.Error())
	}
}

// teamNameByID looks up a team name by ID among the user's joined teams.
// Returns "未知" on any error to keep the identity reply non-blocking.
func (r *router) teamNameByID(ctx context.Context, userID int64, teamID string) string {
	teams, err := r.teams.ListUserTeams(ctx, userID)
	if err != nil || teams == nil {
		return "未知"
	}
	for _, t := range teams {
		if t.ID == teamID {
			return t.Name
		}
	}
	return "未知"
}
