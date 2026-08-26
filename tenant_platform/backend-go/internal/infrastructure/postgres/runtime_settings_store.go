package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/domain"
	"github.com/jackc/pgx/v5"
)

func (s *Store) GetIMInboundCoalesceWindowMS(ctx context.Context) (int, error) {
	var windowMS int
	err := s.pool.QueryRow(ctx, `
SELECT int_value
FROM platform_runtime_settings
WHERE setting_key = $1
`, domain.IMInboundCoalesceWindowSettingKey).Scan(&windowMS)
	if err != nil {
		// 审查 M2(单真值源): 无行 = 从未配置/种子被删 = 回退域默认值。
		// domain.DefaultIMInboundCoalesceWindowMS 是"默认"的唯一定义,
		// migration 0025 种子与其一致由契约测试守护; 此前直接返回错误
		// 会让 GET /admin/settings 500、ReconcileBots 持续报错——DB 行
		// 是运行期覆盖, 缺省应回落默认而非故障。
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.DefaultIMInboundCoalesceWindowMS, nil
		}
		return 0, fmt.Errorf("get im inbound coalesce window: %w", err)
	}
	return windowMS, nil
}

func (s *Store) UpdateIMInboundCoalesceWindowMS(ctx context.Context, windowMS int, updatedBy int64) (int, error) {
	if err := domain.ValidateIMInboundCoalesceWindowMS(windowMS); err != nil {
		return 0, err
	}
	var stored int
	err := s.pool.QueryRow(ctx, `
UPDATE platform_runtime_settings
SET int_value = $2,
    updated_by = $3,
    updated_at = timezone('utc', now())
WHERE setting_key = $1
RETURNING int_value
`, domain.IMInboundCoalesceWindowSettingKey, windowMS, updatedBy).Scan(&stored)
	if err != nil {
		return 0, fmt.Errorf("update im inbound coalesce window: %w", err)
	}
	return stored, nil
}

func (s *Store) GetAgentMaxTurns(ctx context.Context) (int, error) {
	var maxTurns int
	err := s.pool.QueryRow(ctx, `
SELECT int_value
FROM platform_runtime_settings
WHERE setting_key = $1
`, domain.AgentMaxTurnsSettingKey).Scan(&maxTurns)
	if err != nil {
		return 0, fmt.Errorf("get agent max turns: %w", err)
	}
	return maxTurns, nil
}

func (s *Store) UpdateAgentMaxTurns(ctx context.Context, maxTurns int, updatedBy int64) (int, error) {
	if err := domain.ValidateAgentMaxTurns(maxTurns); err != nil {
		return 0, err
	}
	var stored int
	err := s.pool.QueryRow(ctx, `
UPDATE platform_runtime_settings
SET int_value = $2,
    updated_by = $3,
    updated_at = timezone('utc', now())
WHERE setting_key = $1
RETURNING int_value
`, domain.AgentMaxTurnsSettingKey, maxTurns, updatedBy).Scan(&stored)
	if err != nil {
		return 0, fmt.Errorf("update agent max turns: %w", err)
	}
	return stored, nil
}

// GetIMStreamingMode 解析 IM 流式输出开关; 缺失/非法值回退默认
// (streaming——设计: 私聊默认开, 群聊由转发判定收敛)。
func (s *Store) GetIMStreamingMode(ctx context.Context) (domain.IMStreamingMode, error) {
	var raw string
	err := s.pool.QueryRow(ctx, `
SELECT text_value
FROM platform_runtime_settings
WHERE setting_key = $1
`, domain.IMStreamingModeSettingKey).Scan(&raw)
	if err != nil {
		return domain.NormalizeIMStreamingMode(""), fmt.Errorf("get im streaming mode: %w", err)
	}
	return domain.NormalizeIMStreamingMode(domain.IMStreamingMode(raw)), nil
}

// UpdateIMStreamingMode 更新 IM 流式输出开关并返回归一后的模式。
func (s *Store) UpdateIMStreamingMode(ctx context.Context, mode domain.IMStreamingMode, updatedBy int64) (domain.IMStreamingMode, error) {
	if err := domain.ValidateIMStreamingMode(string(mode)); err != nil {
		return "", err
	}
	var stored string
	err := s.pool.QueryRow(ctx, `
UPDATE platform_runtime_settings
SET text_value = $2,
    updated_by = $3,
    updated_at = timezone('utc', now())
WHERE setting_key = $1
RETURNING text_value
`, domain.IMStreamingModeSettingKey, string(mode), updatedBy).Scan(&stored)
	if err != nil {
		return "", fmt.Errorf("update im streaming mode: %w", err)
	}
	return domain.NormalizeIMStreamingMode(domain.IMStreamingMode(stored)), nil
}
