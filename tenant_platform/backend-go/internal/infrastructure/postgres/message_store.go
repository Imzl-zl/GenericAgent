package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/domain"
)

// InsertInboundMessage persists a received WeChat message. Returns
// domain.ErrDuplicateInboundMessage when the (bot_id, message_id) pair is
// already present, so the caller can short-circuit as idempotent.
//
// The partial UNIQUE index messages_inbound_dedup_uq is the cross-instance
// idempotency backstop: when the in-memory `seen` map in transport.ILinkAdapter
// is cold (restart) or split across instances, the DB constraint rejects the
// duplicate INSERT.
func (s *Store) InsertInboundMessage(ctx context.Context, m domain.Message) (domain.Message, error) {
	if m.UserID <= 0 || m.BotID <= 0 {
		return domain.Message{}, fmt.Errorf("user id and bot id are required")
	}
	if m.SessionKey == "" {
		return domain.Message{}, fmt.Errorf("session key is required")
	}
	if m.MessageType == "" {
		m.MessageType = domain.MessageTypeText
	}
	var out domain.Message
	err := s.withTx(ctx, func(tx pgx.Tx) error {
		return scanMessage(tx.QueryRow(ctx, `
INSERT INTO messages (user_id, bot_id, session_key, direction, message_id,
                      message_type, content, media_path, task_id)
VALUES ($1, $2, $3, 'inbound', $4, $5, $6, $7, NULLIF($8, ''))
RETURNING id, user_id, bot_id, session_key, direction, COALESCE(message_id, ''),
          message_type, COALESCE(content, ''), COALESCE(media_path, ''),
          COALESCE(task_id, ''), created_at
`, m.UserID, m.BotID, m.SessionKey, nullString(m.MessageID), m.MessageType,
			m.Content, nullString(m.MediaPath), m.TaskID), &out)
	})
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return domain.Message{}, domain.ErrDuplicateInboundMessage
		}
		return domain.Message{}, fmt.Errorf("insert inbound message: %w", err)
	}
	return out, nil
}

// DeleteInboundMessage 删除 claim 后副作用失败的消息行(round10 审查 B7):
// 命令/relay 路径先插入消息行(claim)再执行副作用, 副作用失败时删除该行,
// 让 Poller 重试能重新执行(而不是被残留行短路)。best-effort: 崩溃窗口内
// 残留的行会让重试短路(命令丢失, 用户可重发), 但不会重复执行副作用。
func (s *Store) DeleteInboundMessage(ctx context.Context, botID int64, messageID string) error {
	if botID <= 0 || messageID == "" {
		return fmt.Errorf("bot id and message id are required")
	}
	tag, err := s.pool.Exec(ctx, `
DELETE FROM messages
WHERE direction = 'inbound' AND bot_id = $1 AND message_id = $2
`, botID, messageID)
	if err != nil {
		return fmt.Errorf("delete inbound message: %w", err)
	}
	_ = tag
	return nil
}

// HasInboundMessage 只读检查 (bot_id, message_id) 是否已成功处理过
// (round9 审查: 入站消息行在路由成功后写入, 是跨重启/多实例的持久幂等
// 事实源——内存 seen 缓存变冷时, router 用它短路重复副作用)。
func (s *Store) HasInboundMessage(ctx context.Context, botID int64, messageID string) (bool, error) {
	if botID <= 0 || messageID == "" {
		return false, fmt.Errorf("bot id and message id are required")
	}
	var exists bool
	err := s.pool.QueryRow(ctx, `
SELECT EXISTS (
    SELECT 1 FROM messages
    WHERE direction = 'inbound' AND bot_id = $1 AND message_id = $2
)
`, botID, messageID).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("check inbound message: %w", err)
	}
	return exists, nil
}

// InsertOutboundMessage persists a reply sent to WeChat. Outbound messages have
// no iLink-assigned message_id, so they are exempt from the dedup index.
func (s *Store) InsertOutboundMessage(ctx context.Context, m domain.Message) (domain.Message, error) {
	if m.UserID <= 0 || m.BotID <= 0 {
		return domain.Message{}, fmt.Errorf("user id and bot id are required")
	}
	if m.SessionKey == "" {
		return domain.Message{}, fmt.Errorf("session key is required")
	}
	if m.MessageType == "" {
		m.MessageType = domain.MessageTypeText
	}
	var out domain.Message
	err := s.withTx(ctx, func(tx pgx.Tx) error {
		return scanMessage(tx.QueryRow(ctx, `
INSERT INTO messages (user_id, bot_id, session_key, direction, message_id,
                      message_type, content, media_path, task_id)
VALUES ($1, $2, $3, 'outbound', NULL, $4, $5, NULLIF($6, ''), NULLIF($7, ''))
RETURNING id, user_id, bot_id, session_key, direction, COALESCE(message_id, ''),
          message_type, COALESCE(content, ''), COALESCE(media_path, ''),
          COALESCE(task_id, ''), created_at
`, m.UserID, m.BotID, m.SessionKey, m.MessageType, m.Content,
			m.MediaPath, m.TaskID), &out)
	})
	if err != nil {
		return domain.Message{}, fmt.Errorf("insert outbound message: %w", err)
	}
	return out, nil
}

// HasOutboundMessage reports whether a delivery part was already accepted by
// iLink and journaled. Delivery retries use this as their durable per-part
// progress marker so a later file failure cannot resend an earlier text part.
func (s *Store) HasOutboundMessage(ctx context.Context, taskID, messageType, content, mediaPath string) (bool, error) {
	if taskID == "" || messageType == "" {
		return false, fmt.Errorf("task id and message type are required")
	}
	var exists bool
	err := s.pool.QueryRow(ctx, `
SELECT EXISTS (
    SELECT 1
    FROM messages
    WHERE direction = 'outbound'
      AND task_id = $1
      AND message_type = $2
      AND COALESCE(content, '') = $3
      AND COALESCE(media_path, '') = $4
)
`, taskID, messageType, content, mediaPath).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("check outbound message: %w", err)
	}
	return exists, nil
}

// ListMessagesByUser returns the most recent messages for a user, newest first.
// limit is capped at 200 to bound query cost; callers wanting more should paginate.
func (s *Store) ListMessagesByUser(ctx context.Context, userID int64, limit, offset int) ([]domain.Message, error) {
	if userID <= 0 {
		return nil, fmt.Errorf("user id must be positive")
	}
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	if offset < 0 {
		offset = 0
	}
	rows, err := s.pool.Query(ctx, `
SELECT id, user_id, bot_id, session_key, direction, COALESCE(message_id, ''),
       message_type, COALESCE(content, ''), COALESCE(media_path, ''),
       COALESCE(task_id, ''), created_at
FROM messages
WHERE user_id = $1
ORDER BY created_at DESC
LIMIT $2 OFFSET $3
`, userID, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("list messages: %w", err)
	}
	defer rows.Close()
	return scanMessages(rows)
}

func scanMessage(row pgx.Row, m *domain.Message) error {
	var direction string
	err := row.Scan(&m.ID, &m.UserID, &m.BotID, &m.SessionKey, &direction, &m.MessageID,
		&m.MessageType, &m.Content, &m.MediaPath, &m.TaskID, &m.CreatedAt)
	if err != nil {
		return err
	}
	m.Direction = domain.MessageDirection(direction)
	return nil
}

func scanMessages(rows pgx.Rows) ([]domain.Message, error) {
	var out []domain.Message
	for rows.Next() {
		var m domain.Message
		if err := scanMessage(rows, &m); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}
