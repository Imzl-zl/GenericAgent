package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/domain"
)

// InsertMediaAsset persists metadata for one media file. When the partial
// UNIQUE index (message_id, storage_path) rejects a duplicate, the function
// returns domain.ErrDuplicateMediaAsset so the caller can treat it as
// idempotent success (the file is already recorded for this message).
//
// The constraint is the cross-instance idempotency backstop: when the
// in-memory `seen` map in transport.ILinkAdapter is cold (restart) or split
// across instances, the DB constraint rejects the duplicate INSERT.
func (s *Store) InsertMediaAsset(ctx context.Context, m domain.MediaAsset) (domain.MediaAsset, error) {
	if m.UserID <= 0 || m.BotID <= 0 {
		return domain.MediaAsset{}, fmt.Errorf("user id and bot id are required")
	}
	if m.StoragePath == "" {
		return domain.MediaAsset{}, fmt.Errorf("storage path is required")
	}
	if m.ContentType == "" {
		m.ContentType = "application/octet-stream"
	}
	if m.Direction == "" {
		m.Direction = domain.MessageInbound
	}
	var out domain.MediaAsset
	err := s.withTx(ctx, func(tx pgx.Tx) error {
		return scanMediaAsset(tx.QueryRow(ctx, `
INSERT INTO media_assets (user_id, bot_id, message_id, file_name, storage_path,
                          content_type, size_bytes, sha256, direction)
VALUES ($1, $2, NULLIF($3, 0), $4, $5, $6, $7, NULLIF($8, ''), $9)
ON CONFLICT (message_id, storage_path) WHERE message_id IS NOT NULL
DO NOTHING
RETURNING id, user_id, bot_id, COALESCE(message_id, 0), file_name, storage_path,
          content_type, size_bytes, COALESCE(sha256, ''), direction, created_at
`, m.UserID, m.BotID, m.MessageID, m.FileName, m.StoragePath,
			m.ContentType, m.SizeBytes, m.SHA256, string(m.Direction)), &out)
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// ON CONFLICT DO NOTHING returned no rows: duplicate. Caller
			// treats this as idempotent success.
			return domain.MediaAsset{}, domain.ErrDuplicateMediaAsset
		}
		return domain.MediaAsset{}, fmt.Errorf("insert media asset: %w", err)
	}
	return out, nil
}

func scanMediaAsset(row pgx.Row, m *domain.MediaAsset) error {
	var direction string
	err := row.Scan(&m.ID, &m.UserID, &m.BotID, &m.MessageID, &m.FileName,
		&m.StoragePath, &m.ContentType, &m.SizeBytes, &m.SHA256,
		&direction, &m.CreatedAt)
	if err != nil {
		return err
	}
	m.Direction = domain.MessageDirection(direction)
	return nil
}

// DeleteExpiredMediaAssets 删除超过保留期的媒体审计行(2026-08-13 审查
// I4/D7: 媒体字节=用户隐私数据, 审计行 90d 保留期)。返回删除行数。
func (s *Store) DeleteExpiredMediaAssets(ctx context.Context, before time.Time) (int64, error) {
	tag, err := s.pool.Exec(ctx, `
DELETE FROM media_assets WHERE created_at < $1
`, before)
	if err != nil {
		return 0, fmt.Errorf("delete expired media assets: %w", err)
	}
	return tag.RowsAffected(), nil
}
