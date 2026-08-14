package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/domain"
)

const providerColumns = `id, name, provider_type, base_url, model,
api_key_ciphertext, api_key_key_version, session_config, transport_config,
capabilities, revision, is_default, state, created_at, updated_at`

func (s *Store) CreateProvider(ctx context.Context, input domain.LLMProviderCreate) (domain.LLMProvider, error) {
	input.TransportConfig = normalizeTransportConfig(input.TransportConfig)
	if err := validateProviderCreate(input, true); err != nil {
		return domain.LLMProvider{}, err
	}
	sessionJSON, transportJSON, capabilitiesJSON, err := marshalProviderConfigs(input)
	if err != nil {
		return domain.LLMProvider{}, err
	}

	var provider domain.LLMProvider
	err = s.withTx(ctx, func(tx pgx.Tx) error {
		var count int
		if err := tx.QueryRow(ctx, `SELECT COUNT(*) FROM llm_providers`).Scan(&count); err != nil {
			return err
		}
		query := `INSERT INTO llm_providers (
			name, provider_type, base_url, model, api_key_ciphertext,
			api_key_key_version, session_config, transport_config, capabilities, is_default
		) VALUES ($1, $2, $3, $4, $5, $6, $7::jsonb, $8::jsonb, $9::jsonb, $10)
		RETURNING ` + providerColumns
		return scanProvider(tx.QueryRow(ctx, query,
			input.Name, string(input.ProviderType), input.BaseURL, input.Model,
			input.APIKeyCiphertext, input.APIKeyKeyVersion,
			string(sessionJSON), string(transportJSON), string(capabilitiesJSON), count == 0,
		), &provider)
	})
	return provider, err
}

func (s *Store) GetProvider(ctx context.Context, id int64) (domain.LLMProvider, error) {
	var provider domain.LLMProvider
	query := `SELECT ` + providerColumns + ` FROM llm_providers WHERE id = $1`
	err := scanProvider(s.pool.QueryRow(ctx, query, id), &provider)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.LLMProvider{}, domain.ErrProviderNotFound
	}
	return provider, err
}

func (s *Store) GetDefaultProvider(ctx context.Context) (domain.LLMProvider, error) {
	var provider domain.LLMProvider
	query := `SELECT ` + providerColumns + ` FROM llm_providers
		WHERE is_default = TRUE AND state = 'active'`
	err := scanProvider(s.pool.QueryRow(ctx, query), &provider)
	return provider, err
}

func (s *Store) ListProviders(ctx context.Context) ([]domain.LLMProvider, error) {
	query := `SELECT ` + providerColumns + ` FROM llm_providers ORDER BY created_at, id`
	rows, err := s.pool.Query(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanProviders(rows)
}

func (s *Store) ListActiveProviders(ctx context.Context) ([]domain.LLMProvider, error) {
	query := `SELECT ` + providerColumns + ` FROM llm_providers
		WHERE state = 'active' ORDER BY is_default DESC, id ASC`
	rows, err := s.pool.Query(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanProviders(rows)
}

func (s *Store) UpdateProvider(ctx context.Context, id int64, input domain.LLMProviderUpdate) (domain.LLMProvider, error) {
	input.TransportConfig = normalizeTransportConfig(input.TransportConfig)
	if err := validateProviderCreate(input.LLMProviderCreate, input.RotateAPIKey); err != nil {
		return domain.LLMProvider{}, err
	}
	sessionJSON, transportJSON, capabilitiesJSON, err := marshalProviderConfigs(input.LLMProviderCreate)
	if err != nil {
		return domain.LLMProvider{}, err
	}

	query := `UPDATE llm_providers SET
		name = $2,
		provider_type = $3,
		base_url = $4,
		model = $5,
		api_key_ciphertext = CASE WHEN $6 THEN $7 ELSE api_key_ciphertext END,
		api_key_key_version = CASE WHEN $6 THEN $8 ELSE api_key_key_version END,
		session_config = $9::jsonb,
		transport_config = $10::jsonb,
		capabilities = $11::jsonb,
		revision = revision + CASE WHEN
			provider_type IS DISTINCT FROM $3 OR
			base_url IS DISTINCT FROM $4 OR
			model IS DISTINCT FROM $5 OR
			session_config IS DISTINCT FROM $9::jsonb OR
			transport_config IS DISTINCT FROM $10::jsonb OR
			capabilities IS DISTINCT FROM $11::jsonb
		THEN 1 ELSE 0 END,
		updated_at = NOW()
	WHERE id = $1
	RETURNING ` + providerColumns

	var provider domain.LLMProvider
	err = scanProvider(s.pool.QueryRow(ctx, query,
		id, input.Name, string(input.ProviderType), input.BaseURL, input.Model,
		input.RotateAPIKey, input.APIKeyCiphertext, input.APIKeyKeyVersion,
		string(sessionJSON), string(transportJSON), string(capabilitiesJSON),
	), &provider)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.LLMProvider{}, domain.ErrProviderNotFound
	}
	return provider, err
}

func (s *Store) SetProviderState(
	ctx context.Context,
	id int64,
	state domain.LLMProviderState,
) (domain.LLMProvider, error) {
	if !state.Valid() {
		return domain.LLMProvider{}, fmt.Errorf("%w: invalid provider state %q", domain.ErrValidation, state)
	}

	var provider domain.LLMProvider
	err := s.withTx(ctx, func(tx pgx.Tx) error {
		query := `SELECT ` + providerColumns + ` FROM llm_providers WHERE id = $1 FOR UPDATE`
		var current domain.LLMProvider
		if err := scanProvider(tx.QueryRow(ctx, query, id), &current); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.ErrProviderNotFound
			}
			return fmt.Errorf("get provider %d: %w", id, err)
		}
		if current.State == state {
			provider = current
			return nil
		}
		if state == domain.ProviderDisabled && current.IsDefault {
			return domain.ErrProviderStateConflict
		}
		update := `UPDATE llm_providers SET
			state = $2, revision = revision + 1, updated_at = NOW()
			WHERE id = $1 RETURNING ` + providerColumns
		return scanProvider(tx.QueryRow(ctx, update, id, string(state)), &provider)
	})
	return provider, err
}

func (s *Store) SetDefaultProvider(ctx context.Context, id int64) error {
	return s.withTx(ctx, func(tx pgx.Tx) error {
		query := `SELECT ` + providerColumns + ` FROM llm_providers WHERE id = $1 FOR UPDATE`
		var provider domain.LLMProvider
		if err := scanProvider(tx.QueryRow(ctx, query, id), &provider); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.ErrProviderNotFound
			}
			return fmt.Errorf("get provider %d: %w", id, err)
		}
		if !provider.IsActive() {
			return domain.ErrProviderStateConflict
		}
		if provider.IsDefault {
			return nil
		}
		if _, err := tx.Exec(ctx, `UPDATE llm_providers SET is_default = FALSE WHERE id != $1`, id); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `UPDATE llm_providers SET is_default = TRUE, updated_at = NOW() WHERE id = $1`, id)
		return err
	})
}

func (s *Store) DeleteProvider(ctx context.Context, id int64) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM llm_providers WHERE id = $1`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return domain.ErrProviderNotFound
	}
	return nil
}

func validateProviderCreate(input domain.LLMProviderCreate, requireKey bool) error {
	if strings.TrimSpace(input.Name) == "" {
		return fmt.Errorf("provider name is required")
	}
	if input.ProviderType != domain.ProviderNativeOAI && input.ProviderType != domain.ProviderNativeClaude {
		return fmt.Errorf("invalid provider type: %s", input.ProviderType)
	}
	if strings.TrimSpace(input.BaseURL) == "" || strings.TrimSpace(input.Model) == "" {
		return fmt.Errorf("base_url and model are required")
	}
	if requireKey && (len(input.APIKeyCiphertext) == 0 || strings.TrimSpace(input.APIKeyKeyVersion) == "") {
		return fmt.Errorf("api key ciphertext and key version are required")
	}
	if err := input.SessionConfig.Validate(input.ProviderType); err != nil {
		return fmt.Errorf("session config: %w", err)
	}
	if err := input.TransportConfig.Validate(); err != nil {
		return fmt.Errorf("transport config: %w", err)
	}
	// 能力维度校验(0058): 空 = [chat](存量兼容); 元素合法、无重复、
	// native_claude 仅 chat。
	seen := map[domain.ProviderCapability]bool{}
	for _, cap := range input.Capabilities {
		if !domain.ValidProviderCapability(cap) {
			return fmt.Errorf("invalid capability %q (valid: chat, image)", cap)
		}
		if seen[cap] {
			return fmt.Errorf("duplicate capability %q", cap)
		}
		seen[cap] = true
	}
	if input.ProviderType == domain.ProviderNativeClaude {
		for _, cap := range input.Capabilities {
			if cap == domain.ProviderCapabilityImage {
				return fmt.Errorf("image capability is only supported by native_oai providers")
			}
		}
	}
	return nil
}

func normalizeTransportConfig(config domain.ProviderTransportConfig) domain.ProviderTransportConfig {
	if config.AuthMode == "" {
		config.AuthMode = domain.ProviderAuthAuto
	}
	return config
}

func marshalProviderConfigs(input domain.LLMProviderCreate) ([]byte, []byte, []byte, error) {
	sessionJSON, err := json.Marshal(input.SessionConfig)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("marshal session config: %w", err)
	}
	transportJSON, err := json.Marshal(input.TransportConfig)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("marshal transport config: %w", err)
	}
	// 省略 = [chat] 由 DB 默认值兜底, 但显式序列化保持读写一致。
	caps := input.Capabilities
	if len(caps) == 0 {
		caps = []domain.ProviderCapability{domain.ProviderCapabilityChat}
	}
	capabilitiesJSON, err := json.Marshal(caps)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("marshal capabilities: %w", err)
	}
	return sessionJSON, transportJSON, capabilitiesJSON, nil
}

func scanProvider(row pgx.Row, provider *domain.LLMProvider) error {
	var sessionJSON, transportJSON, capabilitiesJSON []byte
	err := row.Scan(
		&provider.ID, &provider.Name, &provider.ProviderType, &provider.BaseURL, &provider.Model,
		&provider.APIKeyCiphertext, &provider.APIKeyKeyVersion, &sessionJSON, &transportJSON,
		&capabilitiesJSON,
		&provider.Revision, &provider.IsDefault, &provider.State,
		&provider.CreatedAt, &provider.UpdatedAt,
	)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(sessionJSON, &provider.SessionConfig); err != nil {
		return fmt.Errorf("unmarshal session config: %w", err)
	}
	if err := json.Unmarshal(transportJSON, &provider.TransportConfig); err != nil {
		return fmt.Errorf("unmarshal transport config: %w", err)
	}
	if len(capabilitiesJSON) > 0 {
		if err := json.Unmarshal(capabilitiesJSON, &provider.Capabilities); err != nil {
			return fmt.Errorf("unmarshal capabilities: %w", err)
		}
	}
	return nil
}

func scanProviders(rows pgx.Rows) ([]domain.LLMProvider, error) {
	var providers []domain.LLMProvider
	for rows.Next() {
		var provider domain.LLMProvider
		if err := scanProvider(rows, &provider); err != nil {
			return nil, err
		}
		providers = append(providers, provider)
	}
	return providers, rows.Err()
}
