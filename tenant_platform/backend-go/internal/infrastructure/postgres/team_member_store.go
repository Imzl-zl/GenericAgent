package postgres

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/domain"
)

// AcceptDirectInvite advances a pending_member to pending_owner.
// Called by the invitee to accept a direct owner invitation.
func (s *Store) AcceptDirectInvite(ctx context.Context, shortID string, userID int64) (domain.TeamMember, error) {
	memberID, err := parseMemberShortID(shortID)
	if err != nil {
		return domain.TeamMember{}, domain.ErrMemberNotFound
	}
	var out domain.TeamMember
	err = s.withTx(ctx, func(tx pgx.Tx) error {
		m, err := scanMemberByID(ctx, tx, memberID)
		if err != nil {
			return err
		}
		if m.UserID != userID {
			return domain.ErrMemberNotFound
		}
		if m.Status != domain.MemberPendingMember {
			return domain.ErrWrongMemberStatus
		}
		now := time.Now().UTC()
		if _, err := tx.Exec(ctx, `
UPDATE team_members SET status = 'pending_owner', updated_at = $2
WHERE id = $1
`, memberID, now); err != nil {
			return err
		}
		m.Status = domain.MemberPendingOwner
		m.UpdatedAt = now
		out = m
		return nil
	})
	return out, err
}

// ApproveMember advances a pending_owner member to approved.
// Only the team owner may call this.
func (s *Store) ApproveMember(ctx context.Context, shortID string, ownerID int64) (domain.TeamMember, error) {
	return s.transitionMember(ctx, shortID, ownerID, domain.MemberPendingOwner, domain.MemberApproved, true)
}

// RejectMember transitions a pending_member or pending_owner to rejected.
// Only the team owner may call this.
func (s *Store) RejectMember(ctx context.Context, shortID string, ownerID int64) (domain.TeamMember, error) {
	memberID, err := parseMemberShortID(shortID)
	if err != nil {
		return domain.TeamMember{}, domain.ErrMemberNotFound
	}
	var out domain.TeamMember
	err = s.withTx(ctx, func(tx pgx.Tx) error {
		m, err := scanMemberByID(ctx, tx, memberID)
		if err != nil {
			return err
		}
		dbOwner, err := verifyTeamOwner(ctx, tx, m.TeamID)
		if err != nil {
			return err
		}
		if dbOwner != ownerID {
			return domain.ErrNotTeamOwner
		}
		if !m.Status.IsPending() {
			return domain.ErrWrongMemberStatus
		}
		now := time.Now().UTC()
		if _, err := tx.Exec(ctx, `
UPDATE team_members SET status = 'rejected', updated_at = $2 WHERE id = $1
`, memberID, now); err != nil {
			return err
		}
		m.Status = domain.MemberRejected
		m.UpdatedAt = now
		out = m
		return nil
	})
	return out, err
}

// RemoveMember transitions an approved member to removed and clears their
// active_contexts.team_id if it pointed at this team. Only the owner may call.
func (s *Store) RemoveMember(ctx context.Context, shortID string, ownerID int64) (domain.TeamMember, error) {
	memberID, err := parseMemberShortID(shortID)
	if err != nil {
		return domain.TeamMember{}, domain.ErrMemberNotFound
	}
	var out domain.TeamMember
	err = s.withTx(ctx, func(tx pgx.Tx) error {
		m, err := scanMemberByID(ctx, tx, memberID)
		if err != nil {
			return err
		}
		dbOwner, err := verifyTeamOwner(ctx, tx, m.TeamID)
		if err != nil {
			return err
		}
		if dbOwner != ownerID {
			return domain.ErrNotTeamOwner
		}
		if m.Role == domain.RoleOwner {
			return fmt.Errorf("cannot remove team owner")
		}
		if m.Status != domain.MemberApproved {
			return domain.ErrWrongMemberStatus
		}
		now := time.Now().UTC()
		if _, err := tx.Exec(ctx, `
UPDATE team_members SET status = 'removed', removed_at = $2, updated_at = $2
WHERE id = $1
`, memberID, now); err != nil {
			return err
		}
		// Clear active_contexts.team_id so the user's next message goes personal.
		// 审查 R5-I4: 必须限定当前团队——无条件清空会把用户正在使用的其他
		// 团队上下文也抹掉。
		if _, err := tx.Exec(ctx, `
UPDATE active_contexts SET team_id = NULL, updated_at = $2
WHERE user_id = $1 AND team_id = $3::uuid
`, m.UserID, now, m.TeamID); err != nil {
			return err
		}
		// Cancel the removed member's queued tasks in this team's workspace.
		// Without this, tasks submitted before removal would still dispatch to
		// a Worker after the member lost access (authorizeSubmitter would now
		// reject, but the task is already queued and may be claimed).
		teamSessionKey := fmt.Sprintf("team:%s", m.TeamID)
		if _, err := tx.Exec(ctx, `
UPDATE tasks SET
  status = 'cancelled',
  claim_owner = NULL,
  claim_lease_until = NULL,
  claimed_at = NULL,
  terminal_error_code = 'TASK_CANCELLED',
  terminal_error_message = 'member removed from team',
  terminal_at = timezone('utc', now()),
  updated_at = timezone('utc', now())
WHERE requester_user_id = $1 AND session_key = $2 AND status = 'queued'
`, m.UserID, teamSessionKey); err != nil {
			return err
		}
		// 审查 R5-I4: 已派发(starting/running)任务写 durable cancel_requested_at,
		// 由 scheduler tick 轮询执行 Worker cancel; 终态事务撤销其 capability
		// JTI(revokeTaskCapabilityJTIs), 成员移除后既有任务不得继续调用模型。
		// 未派发(starting + dispatch NULL)任务不在此列——由下方直接终态化。
		if _, err := tx.Exec(ctx, `
UPDATE tasks SET
  cancel_requested_at = timezone('utc', now()),
  updated_at = timezone('utc', now())
WHERE requester_user_id = $1 AND session_key = $2
  AND status IN ('starting','running')
  AND worker_dispatch_started_at IS NOT NULL
  AND cancel_requested_at IS NULL
`, m.UserID, teamSessionKey); err != nil {
			return err
		}
		// 审查 R5-I4: starting 但尚未 MarkDispatchStarted 的任务没有 dispatch
		// 收尾(dispatch 对取消+未派发直接 return), 只写 cancel_requested_at
		// 会永久卡在 starting——直接终态化(cancelled), 与 queued 取消一致。
		if _, err := tx.Exec(ctx, `
UPDATE tasks SET
  status = 'cancelled',
  claim_owner = NULL,
  claim_lease_until = NULL,
  claimed_at = NULL,
  terminal_error_code = 'TASK_CANCELLED',
  terminal_error_message = 'member removed from team',
  terminal_at = timezone('utc', now()),
  updated_at = timezone('utc', now())
WHERE requester_user_id = $1 AND session_key = $2
  AND status = 'starting' AND worker_dispatch_started_at IS NULL
  AND cancel_requested_at IS NULL
`, m.UserID, teamSessionKey); err != nil {
			return err
		}
		m.Status = domain.MemberRemoved
		m.RemovedAt = &now
		m.UpdatedAt = now
		out = m
		return nil
	})
	return out, err
}

// transitionMember is the shared path for approve-style transitions where
// the source status is known and the caller is the team owner.
func (s *Store) transitionMember(ctx context.Context, shortID string, ownerID int64, from, to domain.TeamMemberStatus, setApprovedAt bool) (domain.TeamMember, error) {
	memberID, err := parseMemberShortID(shortID)
	if err != nil {
		return domain.TeamMember{}, domain.ErrMemberNotFound
	}
	var out domain.TeamMember
	err = s.withTx(ctx, func(tx pgx.Tx) error {
		m, err := scanMemberByID(ctx, tx, memberID)
		if err != nil {
			return err
		}
		dbOwner, err := verifyTeamOwner(ctx, tx, m.TeamID)
		if err != nil {
			return err
		}
		if dbOwner != ownerID {
			return domain.ErrNotTeamOwner
		}
		if m.Status != from {
			return domain.ErrWrongMemberStatus
		}
		now := time.Now().UTC()
		query := `UPDATE team_members SET status = $2, updated_at = $3`
		if setApprovedAt {
			query += `, approved_at = $3`
		}
		query += ` WHERE id = $1`
		if _, err := tx.Exec(ctx, query, memberID, string(to), now); err != nil {
			return err
		}
		m.Status = to
		m.UpdatedAt = now
		if setApprovedAt {
			m.ApprovedAt = &now
		}
		out = m
		return nil
	})
	return out, err
}
