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
		// 审查 C1(I5): queued 与未派发 starting 任务统一走 finalizeTerminal——
		// 在同一事务内撤销已签发 capability JTI(签发先于 MarkDispatchStarted,
		// 裸 UPDATE 会绕过撤销, Platform 崩溃后终态行不被恢复扫描、旧 token
		// 在 TTL 内仍可调用模型)、写 status_transition 事件并取消 pending
		// task_started delivery, 与 CancelTask 的 queued 分支语义一致。
		teamSessionKey := fmt.Sprintf("team:%s", m.TeamID)
		if err := cancelRemovedMemberTasks(ctx, tx, m.UserID, teamSessionKey); err != nil {
			return err
		}
		// 审查 R5-I4: 已派发(starting/running)任务写 durable cancel_requested_at,
		// 由 scheduler tick 轮询执行 Worker cancel; 终态事务撤销其 capability
		// JTI(revokeTaskCapabilityJTIs), 成员移除后既有任务不得继续调用模型。
		// 未派发(starting + dispatch NULL)任务由 cancelRemovedMemberTasks 终态化。
		// round9 审查: 已派发任务的 JTI 必须在移除事务内立即撤销, 而不是等
		// 终态事务——若 scheduler 停机/取消 RPC 卡住/Worker 不返回终态, 被移除
		// 成员的任务在 TTL 内仍可调用 LLM/Sophub。逐行 FOR UPDATE 后先撤销
		// JTI 再写 cancel_requested_at, 撤销失败则整个移除事务回滚。
		rows, err := tx.Query(ctx, `
SELECT `+taskSelectColumns+` FROM tasks
WHERE requester_user_id = $1 AND session_key = $2
  AND status IN `+activeTaskStatusesSQL+`
  AND worker_dispatch_started_at IS NOT NULL
  AND cancel_requested_at IS NULL
FOR UPDATE
`, m.UserID, teamSessionKey)
		if err != nil {
			return err
		}
		var dispatched []domain.Task
		for rows.Next() {
			t, scanErr := scanTask(rows)
			if scanErr != nil {
				rows.Close()
				return scanErr
			}
			dispatched = append(dispatched, t)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for _, t := range dispatched {
			if err := revokeTaskCapabilityJTIs(ctx, tx, t.ID); err != nil {
				return fmt.Errorf("revoke dispatched task capability jtis on member removal: %w", err)
			}
			if _, err := tx.Exec(ctx, `
UPDATE tasks SET
  cancel_requested_at = timezone('utc', now()),
  updated_at = timezone('utc', now())
WHERE id = $1 AND cancel_requested_at IS NULL
`, t.ID); err != nil {
				return err
			}
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

// cancelRemovedMemberTasks 终态化被移除成员在团队工作区的 queued 与未派发
// starting 任务(审查 C1/I5)。逐行 FOR UPDATE 锁后走 finalizeTerminal:
// 同一事务内撤销已签发 capability JTI、写 status_transition 事件、取消
// pending task_started 并生成 cancelled delivery。裸 UPDATE 会绕过撤销,
// Platform 崩溃后终态行不被恢复扫描, 旧 token 在 TTL 内仍可调用模型。
func cancelRemovedMemberTasks(ctx context.Context, tx pgx.Tx, userID int64, sessionKey string) error {
	rows, err := tx.Query(ctx, `
SELECT `+taskSelectColumns+` FROM tasks
WHERE requester_user_id = $1 AND session_key = $2
  AND ((status = 'queued') OR
       (status = 'starting' AND worker_dispatch_started_at IS NULL AND cancel_requested_at IS NULL))
FOR UPDATE
`, userID, sessionKey)
	if err != nil {
		return err
	}
	defer rows.Close()
	var list []domain.Task
	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			return err
		}
		list = append(list, t)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for _, t := range list {
		if _, err := finalizeTerminal(ctx, tx, t, domain.TaskCancelled, domain.DeliveryTaskCancelled,
			"TASK_CANCELLED", "member removed from team", "", "", ""); err != nil {
			return err
		}
	}
	return nil
}
