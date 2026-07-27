package postgres

import (
	"context"
	"time"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/infrastructure/llmproxy"
)

func (s *Store) RevokeCapability(ctx context.Context, jti string, expiresAt time.Time) error {
	digest := llmproxy.HashJTI(jti)
	_, err := s.pool.Exec(ctx, `
		INSERT INTO llm_capability_revocations (jti_hash, expires_at)
		VALUES ($1, $2)
		ON CONFLICT (jti_hash) DO UPDATE
		SET expires_at = GREATEST(llm_capability_revocations.expires_at, EXCLUDED.expires_at)
	`, digest[:], expiresAt.UTC())
	return err
}

func (s *Store) IsCapabilityRevoked(ctx context.Context, digest [32]byte) (bool, error) {
	var revoked bool
	err := s.pool.QueryRow(ctx, `
		SELECT EXISTS(
			SELECT 1 FROM llm_capability_revocations
			WHERE jti_hash = $1 AND expires_at > NOW()
		)
	`, digest[:]).Scan(&revoked)
	return revoked, err
}

func (s *Store) DeleteExpiredCapabilityRevocations(ctx context.Context, before time.Time) (int64, error) {
	tag, err := s.pool.Exec(ctx, `
		DELETE FROM llm_capability_revocations WHERE expires_at <= $1
	`, before.UTC())
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}
