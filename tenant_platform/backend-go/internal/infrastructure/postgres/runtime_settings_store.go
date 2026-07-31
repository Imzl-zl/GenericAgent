package postgres

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/domain"
)

const documentPoolSettingsColumns = `
enabled, max_active, min_ready, job_idle_ttl_seconds, ready_idle_ttl_seconds,
global_queue_limit, per_tenant_queue_limit, per_tenant_active_limit,
job_timeout_seconds, command_timeout_seconds, version, updated_by, updated_at, reason`

func scanDocumentPoolSettings(row pgx.Row) (domain.DocumentPoolSettings, error) {
	var settings domain.DocumentPoolSettings
	err := row.Scan(
		&settings.Enabled, &settings.MaxActive, &settings.MinReady,
		&settings.JobIdleTTLSeconds, &settings.ReadyIdleTTLSeconds,
		&settings.GlobalQueueLimit, &settings.PerTenantQueueLimit,
		&settings.PerTenantActiveLimit, &settings.JobTimeoutSeconds,
		&settings.CommandTimeoutSeconds, &settings.Version,
		&settings.UpdatedBy, &settings.UpdatedAt, &settings.Reason,
	)
	return settings, err
}

func (s *Store) GetDocumentPoolSettings(ctx context.Context) (domain.DocumentPoolSettings, error) {
	settings, err := scanDocumentPoolSettings(s.pool.QueryRow(ctx,
		"SELECT "+documentPoolSettingsColumns+" FROM document_pool_settings WHERE singleton = TRUE"))
	if err != nil {
		return domain.DocumentPoolSettings{}, fmt.Errorf("get document pool settings: %w", err)
	}
	if err := domain.ValidateDocumentPoolSettings(settings, s.documentPoolDeploymentMaxActive); err != nil {
		return domain.DocumentPoolSettings{}, fmt.Errorf("persisted document pool settings violate deployment policy: %w", err)
	}
	if err := domain.ValidateDocumentPoolSettingsReason(settings.Reason); err != nil {
		return domain.DocumentPoolSettings{}, fmt.Errorf("persisted document pool settings have invalid reason: %w", err)
	}
	return settings, nil
}

func (s *Store) UpdateDocumentPoolSettings(ctx context.Context, settings domain.DocumentPoolSettings, expectedVersion, updatedBy int64, reason string) (domain.DocumentPoolSettings, error) {
	if err := domain.ValidateDocumentPoolSettings(settings, s.documentPoolDeploymentMaxActive); err != nil {
		return domain.DocumentPoolSettings{}, err
	}
	reason = strings.TrimSpace(reason)
	if err := domain.ValidateDocumentPoolSettingsReason(reason); err != nil {
		return domain.DocumentPoolSettings{}, err
	}
	if expectedVersion <= 0 || updatedBy <= 0 {
		return domain.DocumentPoolSettings{}, fmt.Errorf("expected_version and updated_by must be positive")
	}
	row := s.pool.QueryRow(ctx, `
UPDATE document_pool_settings
SET enabled = $1, max_active = $2, min_ready = $3,
    job_idle_ttl_seconds = $4, ready_idle_ttl_seconds = $5,
    global_queue_limit = $6, per_tenant_queue_limit = $7,
    per_tenant_active_limit = $8, job_timeout_seconds = $9,
    command_timeout_seconds = $10, version = version + 1,
    updated_by = $11, updated_at = now(), reason = $12
WHERE singleton = TRUE AND version = $13
RETURNING `+documentPoolSettingsColumns,
		settings.Enabled, settings.MaxActive, settings.MinReady,
		settings.JobIdleTTLSeconds, settings.ReadyIdleTTLSeconds,
		settings.GlobalQueueLimit, settings.PerTenantQueueLimit,
		settings.PerTenantActiveLimit, settings.JobTimeoutSeconds,
		settings.CommandTimeoutSeconds, updatedBy, reason, expectedVersion)
	stored, err := scanDocumentPoolSettings(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.DocumentPoolSettings{}, domain.ErrDocumentPoolSettingsConflict
	}
	if err != nil {
		return domain.DocumentPoolSettings{}, fmt.Errorf("update document pool settings: %w", err)
	}
	return stored, nil
}

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
