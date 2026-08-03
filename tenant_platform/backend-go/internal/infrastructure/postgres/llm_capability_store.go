package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

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
	if err != nil {
		return err
	}
	// 计量行随撤销一并清理(审查 R4-I9): 防止 capability_usage 无界增长。
	_, err = s.pool.Exec(ctx, `DELETE FROM capability_usage WHERE jti_hash = $1`, digest[:])
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

// ConsumeCapabilityCall 原子递增 JTI 的调用计数(审查 R4-I9): 首调插入行
// (used=1), 后续每次 +1; 当 used_calls 已到 max_calls 时不再更新并返回
// (false, nil)。llm-proxy 在转发前消费预算, 防止 Runner 绕过 Worker 的
// RuntimePolicy 直接刷 LLM Proxy。
func (s *Store) ConsumeCapabilityCall(ctx context.Context, jtiHash [32]byte, maxCalls int64) (bool, error) {
	if maxCalls <= 0 {
		return false, fmt.Errorf("max calls must be positive")
	}
	var used int64
	err := s.pool.QueryRow(ctx, `
		INSERT INTO capability_usage (jti_hash, max_calls, used_calls)
		VALUES ($1, $2, 1)
		ON CONFLICT (jti_hash) DO UPDATE
		SET used_calls = capability_usage.used_calls + 1,
		    updated_at = timezone('utc', now())
		WHERE capability_usage.used_calls < $2
		RETURNING used_calls
	`, jtiHash[:], maxCalls).Scan(&used)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// DeleteCapabilityUsage 删除 JTI 的计量行(终态撤销时同步清理, 防止
// capability_usage 无限增长)。
func (s *Store) DeleteCapabilityUsage(ctx context.Context, jtiHash [32]byte) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM capability_usage WHERE jti_hash = $1`, jtiHash[:])
	return err
}
