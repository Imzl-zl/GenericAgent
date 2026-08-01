package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/domain"
)

// BindChannelAccount upserts the channel account → canonical user binding.
// Rebinding an already-bound account moves it to the new user (latest binding wins).
func (s *Store) BindChannelAccount(ctx context.Context, channelType, channelAccountID string, canonicalUserID int64) (domain.ChannelBinding, error) {
	if channelType == "" || channelAccountID == "" {
		return domain.ChannelBinding{}, fmt.Errorf("channel type and account id are required")
	}
	if canonicalUserID <= 0 {
		return domain.ChannelBinding{}, fmt.Errorf("canonical user id must be positive")
	}
	var binding domain.ChannelBinding
	err := s.withTx(ctx, func(tx pgx.Tx) error {
		return scanChannelBinding(tx.QueryRow(ctx, `
INSERT INTO channel_bindings (channel_type, channel_account_id, canonical_user_id)
VALUES ($1, $2, $3)
ON CONFLICT (channel_type, channel_account_id)
DO UPDATE SET canonical_user_id = EXCLUDED.canonical_user_id, updated_at = timezone('utc', now())
RETURNING channel_type, channel_account_id, canonical_user_id, created_at, updated_at
`, channelType, channelAccountID, canonicalUserID), &binding)
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
