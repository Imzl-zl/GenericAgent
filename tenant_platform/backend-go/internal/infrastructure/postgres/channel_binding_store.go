package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/domain"
)

// BindChannelAccount upserts the channel account → canonical user binding.
// Rebinding an already-bound account moves it to the new user (latest binding
// wins)。审查 F12: 重绑(latest-wins 语义保留, 避免破坏既有绑定流程)必须
// 记录审计事件——渠道账号静默转移 canonical 身份必须有迹可查, 后续接入
// 第二渠道时的身份合并依赖此审计链。
func (s *Store) BindChannelAccount(ctx context.Context, channelType, channelAccountID string, canonicalUserID int64) (domain.ChannelBinding, error) {
	if channelType == "" || channelAccountID == "" {
		return domain.ChannelBinding{}, fmt.Errorf("channel type and account id are required")
	}
	if canonicalUserID <= 0 {
		return domain.ChannelBinding{}, fmt.Errorf("canonical user id must be positive")
	}
	var binding domain.ChannelBinding
	err := s.withTx(ctx, func(tx pgx.Tx) error {
		// 重绑检测: 锁定既有绑定行, 读取旧 canonical user(审查 F12 审计)。
		var previous int64
		var hasPrevious bool
		err := tx.QueryRow(ctx, `
SELECT canonical_user_id FROM channel_bindings
WHERE channel_type = $1 AND channel_account_id = $2
FOR UPDATE
`, channelType, channelAccountID).Scan(&previous)
		switch {
		case err == nil:
			hasPrevious = true
		case errors.Is(err, pgx.ErrNoRows):
			hasPrevious = false
		default:
			return err
		}
		if err := scanChannelBinding(tx.QueryRow(ctx, `
INSERT INTO channel_bindings (channel_type, channel_account_id, canonical_user_id)
VALUES ($1, $2, $3)
ON CONFLICT (channel_type, channel_account_id)
DO UPDATE SET canonical_user_id = EXCLUDED.canonical_user_id, updated_at = timezone('utc', now())
RETURNING channel_type, channel_account_id, canonical_user_id, created_at, updated_at
`, channelType, channelAccountID, canonicalUserID), &binding); err != nil {
			return err
		}
		if hasPrevious && previous != canonicalUserID {
			detail, _ := json.Marshal(map[string]int64{
				"previous_canonical_user_id": previous,
				"new_canonical_user_id":      canonicalUserID,
			})
			return s.AppendAuditEventTx(ctx, tx, domain.AuditEvent{
				ActorUserID: canonicalUserID,
				Action:      domain.AuditChannelRebound,
				TargetType:  channelType,
				TargetID:    channelAccountID,
				Detail:      detail,
			})
		}
		return nil
	})
	return binding, err
}

// ResolveCanonicalUserID returns the canonical user bound to a channel account.
func (s *Store) ResolveCanonicalUserID(ctx context.Context, channelType, channelAccountID string) (int64, error) {
	if channelType == "" || channelAccountID == "" {
		return 0, fmt.Errorf("channel type and account id are required")
	}
	var userID int64
	err := s.pool.QueryRow(ctx, `
SELECT canonical_user_id FROM channel_bindings
WHERE channel_type = $1 AND channel_account_id = $2
`, channelType, channelAccountID).Scan(&userID)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, domain.ErrChannelBindingNotFound
	}
	return userID, err
}

func scanChannelBinding(row pgx.Row, binding *domain.ChannelBinding) error {
	return row.Scan(&binding.ChannelType, &binding.ChannelAccountID, &binding.CanonicalUserID, &binding.CreatedAt, &binding.UpdatedAt)
}
