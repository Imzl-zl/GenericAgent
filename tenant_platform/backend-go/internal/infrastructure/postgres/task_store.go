package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/domain"
)

// queueAdvisoryNamespace 是 per-user 队列限流 advisory lock 的命名空间前缀
// (审查 round9: 只锁 workspace 行时, 同一 requester 并发向个人与多个团队
// workspace 提交会各自看到未超限的计数并同时插入, 突破硬上限)。
const queueAdvisoryNamespace = "ga:per-user-queue:"

// SubmitTask inserts a queued task or returns the existing dedupe row unchanged.
// PerUserQueueLimit, when > 0, is enforced inside this transaction (after the
// workspace row lock serializes concurrent submits for the same user) to
// prevent TOCTOU races.
func (s *Store) SubmitTask(ctx context.Context, cmd domain.SubmitTaskCommand) (domain.Task, error) {
	var task domain.Task
	err := s.withTx(ctx, func(tx pgx.Tx) error {
		var err error
		task, err = s.submitTaskTx(ctx, tx, cmd)
		return err
	})
	return task, err
}

// SubmitTaskWithInboundMessage 在同一事务内持久化入站消息行与任务提交
// (round10 审查 B7): 消息行与任务原子写入, 消除"任务已提交但消息行未写"
// 与"消息行已写但任务未提交"的崩溃/并发窗口——重试要么被消息行唯一键短路,
// 要么完整重放(任务唯一键兜底不重复), relay/命令不会因任务路径的残留窗口
// 重复执行, 任务也不会因先写消息行后崩溃而永久丢失。消息行已存在(23505)
// 时返回 domain.ErrDuplicateInboundMessage, 整个事务回滚。
func (s *Store) SubmitTaskWithInboundMessage(ctx context.Context, cmd domain.SubmitTaskCommand, msg domain.Message) (domain.Task, domain.Message, error) {
	if msg.BotID <= 0 || msg.UserID <= 0 || msg.MessageID == "" || msg.SessionKey == "" {
		return domain.Task{}, domain.Message{}, fmt.Errorf("inbound message fields required for atomic submit")
	}
	var task domain.Task
	var out domain.Message
	err := s.withTx(ctx, func(tx pgx.Tx) error {
		var err error
		if err := scanMessage(tx.QueryRow(ctx, `
INSERT INTO messages (user_id, bot_id, session_key, direction, message_id,
                      message_type, content, media_path, task_id)
VALUES ($1, $2, $3, 'inbound', $4, $5, $6, $7, NULL)
RETURNING id, user_id, bot_id, session_key, direction, COALESCE(message_id, ''),
          message_type, COALESCE(content, ''), COALESCE(media_path, ''),
          COALESCE(task_id, ''), created_at
`, msg.UserID, msg.BotID, msg.SessionKey, msg.MessageID, msg.MessageType,
			msg.Content, nullString(msg.MediaPath)), &out); err != nil {
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) && pgErr.Code == "23505" {
				return domain.ErrDuplicateInboundMessage
			}
			return err
		}
		if task, err = s.submitTaskTx(ctx, tx, cmd); err != nil {
			return err
		}
		// 任务 id 回填消息行(审计关联; 失败非致命, 消息行已持久化)。
		if task.ID != "" {
			if _, err := tx.Exec(ctx, `UPDATE messages SET task_id = $1 WHERE id = $2`, task.ID, out.ID); err != nil {
				slog.WarnContext(ctx, "submit task with message: backfill task_id failed", "task_id", task.ID, "message_id", out.ID, "error", err)
			}
		}
		return nil
	})
	return task, out, err
}

// submitTaskTx 是 SubmitTask 的事务体(round10 审查 B7 抽取, 供原子消息+
// 任务提交复用)。
func (s *Store) submitTaskTx(ctx context.Context, tx pgx.Tx, cmd domain.SubmitTaskCommand) (domain.Task, error) {
	if err := validateSubmit(cmd); err != nil {
		return domain.Task{}, err
	}
	promptBytes := len([]byte(cmd.Prompt))
	personaRaw, personaBytes, err := encodePersona(cmd.PersonaSnapshot)
	if err != nil {
		return domain.Task{}, err
	}
	if promptBytes > domain.MaxPromptBytes {
		return domain.Task{}, fmt.Errorf("prompt exceeds max bytes (%d > %d)", promptBytes, domain.MaxPromptBytes)
	}
	if personaBytes > domain.MaxPersonaBytes {
		return domain.Task{}, fmt.Errorf("persona_snapshot exceeds max bytes (%d > %d)", personaBytes, domain.MaxPersonaBytes)
	}

	var task domain.Task
	submitErr := func() error {
		// 审查 round9: 按 requester 持有事务级 advisory lock, 把同一用户跨
		// 所有 workspace(个人+多个团队)的提交串行化——workspace 行锁只覆盖
		// 单 workspace, 跨 workspace 并发会让队列计数检查失效。锁在所有
		// workspace 读取之前获取, 全局统一串行点, 不会形成锁序环。
		if s.perUserQueueLimit > 0 && cmd.RequesterUserID > 0 {
			if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, queueAdvisoryNamespace+strconv.FormatInt(cmd.RequesterUserID, 10)); err != nil {
				return err
			}
		}
		var workspaceID uuid.UUID
		var ownerID int64
		var kind, teamID string
		err := tx.QueryRow(ctx, `
SELECT id, owner_user_id, kind, COALESCE(team_id::text, '') FROM workspaces WHERE session_key = $1 FOR UPDATE
`, cmd.SessionKey).Scan(&workspaceID, &ownerID, &kind, &teamID)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				// Round16-P1: 哨兵化, api 层据此映射 404(而非全 500)。
				return fmt.Errorf("%w: %q", domain.ErrWorkspaceNotFound, cmd.SessionKey)
			}
			return err
		}
		requester := ownerID
		if cmd.RequesterUserID != 0 {
			requester = cmd.RequesterUserID
		}
		if err := authorizeSubmitter(tx, ctx, kind, ownerID, teamID, requester); err != nil {
			return err
		}
		// /new 桶级判定(IM_CHANNEL_ARCHITECTURE §3): 本对话单元桶是否有未消费的
		// reset 标记——有则本任务 fresh(空 history 开始)。审查 R4-I8: 标记
		// 不在提交时清除——它保留到 fresh 任务成功终态, 因此 fresh 任务
		// 失败/取消后, 下一个任务仍然 fresh, 不会静默恢复 /new 前的旧
		// snapshot。并发提交的多个任务共享同一 fresh 语义。
		freshSession := false
		var resetAt *time.Time
		if err := tx.QueryRow(ctx, `
SELECT reset_at FROM conversation_resets WHERE workspace_id = $1 AND conversation_key = $2
`, workspaceID, cmd.ConversationKey).Scan(&resetAt); err != nil {
			if !errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("read conversation reset: %w", err)
			}
		}
		if resetAt != nil {
			freshSession = true
		}

		// Hard per-user queue cap inside the workspace lock. Concurrent
		// submits for the same user serialize on the workspace FOR UPDATE
		// above, so the count is consistent. The soft pre-check in
		// TaskService.SubmitTask handles the obvious case without entering a tx.
		if s.perUserQueueLimit > 0 && requester > 0 {
			queued, err := s.CountQueuedTasksByRequesterTx(ctx, tx, requester)
			if err != nil {
				return fmt.Errorf("count queued (tx): %w", err)
			}
			if queued >= s.perUserQueueLimit {
				return domain.ErrPerUserQueueFull
			}
		}

		var nextSeq int64
		if err := tx.QueryRow(ctx, `
SELECT COALESCE(MAX(session_sequence), 0) + 1 FROM tasks WHERE session_key = $1
`, cmd.SessionKey).Scan(&nextSeq); err != nil {
			return err
		}

		taskID := uuid.NewString()
		idemKey := cmd.MessageID
		mediaRaw, err := json.Marshal(cmd.Media)
		if err != nil {
			return fmt.Errorf("marshal task media: %w", err)
		}
		if len(cmd.Media) > 0 {
			if err := validateTaskMedia(cmd.Media); err != nil {
				return err
			}
		}
		row := tx.QueryRow(ctx, `
INSERT INTO tasks (
  id, workspace_id, session_key, session_sequence, requester_user_id,
  source, source_instance_id, message_id, message_idempotency_key, conversation_key,
  conversation_type,
  prompt, persona_snapshot, tool_policy_version, prompt_bytes, persona_bytes,
  media,
  status, fresh_session
) VALUES (
  $1,$2,$3,$4,$5,
  $6,$7,$8,$9,$10,
  $11,
  $12,$13::jsonb,$14,$15,$16,
  $17::jsonb,
  'queued', $18
)
ON CONFLICT (source, source_instance_id, message_idempotency_key) DO NOTHING
RETURNING `+taskSelectColumns, taskID, workspaceID, cmd.SessionKey, nextSeq, requester,
			cmd.Source, cmd.SourceInstanceID, cmd.MessageID, idemKey, cmd.ConversationKey,
			domain.NormalizeConversationType(cmd.ConversationType),
			cmd.Prompt, string(personaRaw), cmd.ToolPolicyVersion, promptBytes, personaBytes,
			string(mediaRaw),
			freshSession,
		)
		t, scanErr := scanTask(row)
		if scanErr == nil {
			task = t
			if err := insertEvent(ctx, tx, t.ID, "status_transition", 0, nil, nil, "", "queued", "", ""); err != nil {
				return err
			}
			// Create initial "task started" delivery for immediate user feedback.
			// This gives users a "processing..." notification instead of silent waiting.
			if err := insertDelivery(ctx, tx, t.ID, domain.DeliveryTaskStarted, "", "", "", "", ""); err != nil {
				return err
			}
			return nil
		}
		if !errors.Is(scanErr, pgx.ErrNoRows) {
			return scanErr
		}
		// Conflict: return existing row FOR UPDATE unchanged.
		row = tx.QueryRow(ctx, `
SELECT `+taskSelectColumns+` FROM tasks
WHERE source = $1 AND source_instance_id = $2 AND message_idempotency_key = $3
FOR UPDATE
`, cmd.Source, cmd.SourceInstanceID, idemKey)
		t, err = scanTask(row)
		if err != nil {
			return err
		}
		task = t
		return nil
	}()
	return task, submitErr
}

// GetTask loads a task by id.
func (s *Store) GetTask(ctx context.Context, taskID string) (domain.Task, error) {
	row := s.pool.QueryRow(ctx, `SELECT `+taskSelectColumns+` FROM tasks WHERE id = $1`, taskID)
	t, err := scanTask(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Task{}, fmt.Errorf("task not found: %s", taskID)
	}
	if err != nil {
		return domain.Task{}, err
	}
	return t, nil
}

// MarkTaskStreamFinal 置位任务的流式最终交付标记(IM_STREAMING_DELIVERY
// §4.2): scheduler 在流式回复 commit 成功后调用, delivery 据此跳过文本
// part(文件照发)。幂等: 已置位/任务已终态时无操作。返回置位后的
// stream_final_at(未置位返回 nil)。
func (s *Store) MarkTaskStreamFinal(ctx context.Context, taskID string) (*time.Time, error) {
	var finalAt *time.Time
	err := s.pool.QueryRow(ctx, `
UPDATE tasks SET
  stream_final_at = timezone('utc', now()),
  updated_at = timezone('utc', now())
WHERE id = $1 AND stream_final_at IS NULL
RETURNING stream_final_at
`, taskID).Scan(&finalAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("mark task stream final: %w", err)
	}
	return finalAt, nil
}

// WorkspaceIsFresh 返回本对话单元桶的 reset 标记是否仍待消费(/new 后首个成功
// 任务之前)。dispatch 时实时判定并写回 tasks.fresh_session(MarkDispatchStarted
// 事务), 解决多条 queued 任务共享提交时 fresh 快照、第二条任务错误空启动
// 丢失连续上下文的问题(审查 F2)。
// recoveryRevocationTTL 是崩溃恢复撤销 JTI 时写入 revocation 表的有效期:
// capability 签发 TTL(默认 1h)与校验 leeway(30s)的总上界取 2h, 覆盖
// token 的完整剩余寿命, 保证恢复后旧 token 立即且持续失效。
const recoveryRevocationTTL = 2 * time.Hour

// SetTaskCapabilityJTIs 持久化任务实际签发的 capability JTI 列表
// (供 RecoverAfterRestart 在中断任务时同一事务内撤销)。
// 审查 R5-I2: 更新带任务活跃 claim 条件——status 必须 starting/running 且
// claim_owner 是本实例且 lease 未过期。已终态/被接管/lease 丢失的任务行
// 上的 JTI 由终态事务撤销, 新签发的 token 若挂到这样的行上, 崩溃窗口内
// 无人会再撤销(恢复扫描只处理未终态任务)。
func (s *Store) SetTaskCapabilityJTIs(ctx context.Context, taskID, platformInstanceID string, jtis []string) error {
	if taskID == "" {
		return fmt.Errorf("task id is required")
	}
	if platformInstanceID == "" {
		return fmt.Errorf("platform instance id is required")
	}
	if len(jtis) == 0 {
		return nil
	}
	// 审查 C1(I4): 追加去重而不是整体替换——刷新凭据时旧 JTI 对应的 token
	// 在 Worker 确认前尚未撤销, 若被覆盖, Platform 崩溃后恢复事务(读全量
	// capability_jtis 撤销)无法撤销旧 token, 其存活至 TTL。终态事务的
	// revokeTaskCapabilityJTIs 读全量数组, 历史 JTI 一并撤销。
	tag, err := s.pool.Exec(ctx, `
UPDATE tasks SET
  capability_jtis = ARRAY(
    SELECT DISTINCT x FROM unnest(COALESCE(capability_jtis, ARRAY[]::text[]) || $2::text[]) AS x
  ),
  updated_at = timezone('utc', now())
WHERE id = $1
  AND status IN `+activeTaskStatusesSQL+`
  AND claim_owner = $3
  AND claim_lease_until > timezone('utc', now())
`, taskID, jtis, platformInstanceID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("persist capability jtis: task %s not active under %s", taskID, platformInstanceID)
	}
	return nil
}

// ResetWorkspaceForNewSession marks the session for fresh start (/new):
// 在 workspace 行锁下原子完成三件事(审查 F3):
//  1. 设置本对话单元桶的 reset 标记(/new 语义, 保留到该桶 fresh 任务成功终态);
//  2. 终态化该桶所有 queued 任务与未派发的 starting 任务(worker_dispatch_started_at
//     IS NULL), 防止旧上下文任务带旧 snapshot 执行; 其他桶任务不受影响
//     (IM_CHANNEL_ARCHITECTURE §3: /new 清当前桶);
//  3. 对已派发的 starting/running 任务写入 durable cancel_requested_at
//     (tick 驱动 Worker cancel RPC, dispatch 观察后终态化), 闭合
//     "查询活跃任务 -> claim -> reset" 的竞态窗口——即使任务在
//     FindRunningTaskBySession 之后才被 claim, 也会在此事务内被取消。
func (s *Store) ResetWorkspaceForNewSession(ctx context.Context, sessionKey, conversationKey string) (int, error) {
	var cancelled int
	err := s.withTx(ctx, func(tx pgx.Tx) error {
		// workspace 行锁: 与 SubmitTask/ClaimNextTask 串行, 确保取消与
		// reset 的原子性(审查 F3: /new 不得在返回成功后仍有旧上下文任务执行)。
		var workspaceID string
		if err := tx.QueryRow(ctx, `SELECT id FROM workspaces WHERE session_key = $1 FOR UPDATE`, sessionKey).Scan(&workspaceID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
INSERT INTO conversation_resets (workspace_id, conversation_key, reset_at)
VALUES ($1::uuid, $2, timezone('utc', now()))
ON CONFLICT (workspace_id, conversation_key) DO UPDATE SET reset_at = timezone('utc', now())
`, workspaceID, conversationKey); err != nil {
			return err
		}
		// queued + 未派发的 starting: 直接终态化为 cancelled(与 CancelTask
		// 的未派发分支语义一致)。按桶过滤, 其他桶任务不受 /new 影响。
		rows, err := tx.Query(ctx, `
SELECT `+taskSelectColumns+` FROM tasks
WHERE session_key = $1 AND conversation_key = $2
  AND (status = 'queued' OR (status = 'starting' AND worker_dispatch_started_at IS NULL))
FOR UPDATE
`, sessionKey, conversationKey)
		if err != nil {
			return err
		}
		var queued []domain.Task
		for rows.Next() {
			t, scanErr := scanTask(rows)
			if scanErr != nil {
				rows.Close()
				return scanErr
			}
			queued = append(queued, t)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for _, t := range queued {
			if _, err := finalizeTerminal(ctx, tx, t, domain.TaskCancelled, domain.DeliveryTaskCancelled,
				"TASK_CANCELLED", "task cancelled by /new session reset", "", "", ""); err != nil {
				return err
			}
			cancelled++
		}
		// 已派发的 starting/running: 写入 durable cancel request(仅一次)。
		rows2, err := tx.Query(ctx, `
SELECT `+taskSelectColumns+` FROM tasks
WHERE session_key = $1 AND conversation_key = $2 AND status IN `+activeTaskStatusesSQL+`
  AND worker_dispatch_started_at IS NOT NULL AND cancel_requested_at IS NULL
FOR UPDATE
`, sessionKey, conversationKey)
		if err != nil {
			return err
		}
		var active []domain.Task
		for rows2.Next() {
			t, scanErr := scanTask(rows2)
			if scanErr != nil {
				rows2.Close()
				return scanErr
			}
			active = append(active, t)
		}
		rows2.Close()
		if err := rows2.Err(); err != nil {
			return err
		}
		for _, t := range active {
			row := tx.QueryRow(ctx, `
UPDATE tasks SET cancel_requested_at = timezone('utc', now()), updated_at = timezone('utc', now())
WHERE id = $1 AND cancel_requested_at IS NULL
RETURNING `+taskSelectColumns, t.ID)
			if _, err := scanTask(row); err != nil {
				return err
			}
			if err := insertNextEvent(ctx, tx, t.ID, "cancel_request", nil, nil, string(t.Status), string(t.Status), t.WorkerInstanceID, "TASK_CANCELLED"); err != nil {
				return err
			}
		}
		return nil
	})
	return cancelled, err
}

// WorkspaceIsFresh 返回本对话单元桶的 reset 标记是否仍待消费(/new 后首个
// 成功任务之前)。dispatch 时实时判定并写回 tasks.fresh_session(MarkDispatchStarted
// 事务), 解决多条 queued 任务共享提交时 fresh 快照、第二条任务错误空启动
// 丢失连续上下文的问题(审查 F2)。桶级判定见 IM_CHANNEL_ARCHITECTURE §3。
func (s *Store) WorkspaceIsFresh(ctx context.Context, sessionKey, conversationKey string) (bool, error) {
	var fresh bool
	err := s.pool.QueryRow(ctx, `
SELECT EXISTS (
  SELECT 1 FROM conversation_resets AS r
  JOIN workspaces AS w ON w.id = r.workspace_id
  WHERE w.session_key = $1 AND r.conversation_key = $2
)
`, sessionKey, conversationKey).Scan(&fresh)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, fmt.Errorf("workspace not found for session_key %q", sessionKey)
	}
	return fresh, err
}

// ListClaimableSessionKeys returns session keys with queued work and no active
// claim, ordered by cross-tenant round-robin fairness: each requester's oldest
// queued task gets a ROW_NUMBER, and sessions are ordered by (rn, oldest) so
// every requester rotates through instead of one user monopolizing the queue.
// When perRequesterRunningLimit > 0, sessions whose next-task requester already has
// >= perRequesterRunningLimit starting/running tasks are excluded.
func (s *Store) ListClaimableSessionKeys(ctx context.Context, limit, perRequesterRunningLimit int) ([]string, error) {
	if limit <= 0 {
		limit = 32
	}
	rows, err := s.pool.Query(ctx, `
SELECT session_key FROM (
  SELECT
    t.session_key,
    ROW_NUMBER() OVER (PARTITION BY t.requester_user_id ORDER BY MIN(t.created_at)) AS rn,
    MIN(t.created_at) AS oldest
  FROM tasks t
  WHERE t.status = 'queued'
  AND NOT EXISTS (
    SELECT 1 FROM tasks t2
    WHERE t2.session_key = t.session_key AND t2.status IN `+activeTaskStatusesSQL+`
  )
  AND (
    $2 = 0 OR NOT EXISTS (
      SELECT 1 FROM tasks t3
      WHERE t3.requester_user_id = t.requester_user_id
      AND t3.status IN `+activeTaskStatusesSQL+`
      HAVING COUNT(*) >= $2
    )
  )
  GROUP BY t.session_key, t.requester_user_id
) ranked
ORDER BY ranked.rn, ranked.oldest
LIMIT $1
`, limit, perRequesterRunningLimit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var sk string
		if err := rows.Scan(&sk); err != nil {
			return nil, err
		}
		out = append(out, sk)
	}
	return out, rows.Err()
}

// CountRunningTasks returns the global number of starting/running tasks.
// Used by the scheduler to enforce MaxRunningTasks before claiming.
func (s *Store) CountRunningTasks(ctx context.Context) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx, `
SELECT COUNT(*) FROM tasks WHERE status IN `+activeTaskStatusesSQL+`
`).Scan(&n)
	if err != nil {
		return 0, err
	}
	return n, nil
}

// CountQueuedTasksByRequester returns the number of queued tasks owned by
// the given requester. Used by SubmitTask to enforce PerUserQueueLimit.
// Reads inside the caller's transaction when called via CountQueuedTasksByRequesterTx.
func (s *Store) CountQueuedTasksByRequester(ctx context.Context, requesterUserID int64) (int, error) {
	if requesterUserID <= 0 {
		return 0, fmt.Errorf("requester user id must be positive")
	}
	var n int
	err := s.pool.QueryRow(ctx, `
SELECT COUNT(*) FROM tasks WHERE requester_user_id = $1 AND status = 'queued'
`, requesterUserID).Scan(&n)
	if err != nil {
		return 0, err
	}
	return n, nil
}

// CountQueuedTasksByRequesterTx is the in-transaction variant used by
// SubmitTask to avoid TOCTOU races. The count is taken after the workspace
// row lock so concurrent submits for the same user serialize on the workspace.
func (s *Store) CountQueuedTasksByRequesterTx(ctx context.Context, tx pgx.Tx, requesterUserID int64) (int, error) {
	if requesterUserID <= 0 {
		return 0, fmt.Errorf("requester user id must be positive")
	}
	var n int
	err := tx.QueryRow(ctx, `
SELECT COUNT(*) FROM tasks WHERE requester_user_id = $1 AND status = 'queued'
`, requesterUserID).Scan(&n)
	if err != nil {
		return 0, err
	}
	return n, nil
}

// ListOwnedActiveTasks returns starting/running tasks for this platform instance.
func (s *Store) ListOwnedActiveTasks(ctx context.Context, platformInstanceID string) ([]domain.Task, error) {
	rows, err := s.pool.Query(ctx, `
SELECT `+taskSelectColumns+` FROM tasks
WHERE claim_owner = $1 AND status IN `+activeTaskStatusesSQL+`
ORDER BY claimed_at ASC NULLS LAST
`, platformInstanceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.Task
	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// GetConversationResetAt 返回本对话单元桶最近一次 /new 的 reset_at
// (2026-08-15 会话文件引用隔离真值源)。无记录返回零值 time + nil
// (从未 /new = 不过滤)。DB 错误上抛, 调用方决定降级策略。
func (s *Store) GetConversationResetAt(ctx context.Context, sessionKey, conversationKey string) (time.Time, error) {
	var resetAt time.Time
	err := s.pool.QueryRow(ctx, `
SELECT cr.reset_at
FROM conversation_resets cr
JOIN workspaces w ON w.id = cr.workspace_id
WHERE w.session_key = $1 AND cr.conversation_key = $2
`, sessionKey, conversationKey).Scan(&resetAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return time.Time{}, nil
	}
	if err != nil {
		return time.Time{}, fmt.Errorf("get conversation reset at: %w", err)
	}
	return resetAt, nil
}
