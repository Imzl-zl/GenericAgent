package postgres

import (
	"context"
	"fmt"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/domain"
)

func (s *Store) GetIMInboundCoalesceWindowMS(ctx context.Context) (int, error) {
	var windowMS int
	err := s.pool.QueryRow(ctx, `
SELECT int_value
FROM platform_runtime_settings
WHERE setting_key = $1
`, domain.IMInboundCoalesceWindowSettingKey).Scan(&windowMS)
	if err != nil {
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
