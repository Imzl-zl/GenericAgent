package postgres

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/domain"
)

// CreateProvider inserts a new LLM provider. If it is the first row, it is
// automatically marked default.
func (s *Store) CreateProvider(ctx context.Context, name string, providerType domain.LLMProviderType, baseURL, model string, apiKeyCiphertext []byte, keyVersion string) (domain.LLMProvider, error) {
	if name == "" {
		return domain.LLMProvider{}, fmt.Errorf("provider name is required")
	}
	if providerType != domain.ProviderOpenAICompatible && providerType != domain.ProviderAnthropicMessages {
		return domain.LLMProvider{}, fmt.Errorf("invalid provider type: %s", providerType)
	}
	if baseURL == "" || model == "" {
		return domain.LLMProvider{}, fmt.Errorf("base_url and model are required")
	}
	if len(apiKeyCiphertext) == 0 {
		return domain.LLMProvider{}, fmt.Errorf("api key ciphertext is required")
	}

	var p domain.LLMProvider
	err := s.withTx(ctx, func(tx pgx.Tx) error {
		var count int
		if err := tx.QueryRow(ctx, `SELECT COUNT(*) FROM llm_providers`).Scan(&count); err != nil {
			return err
		}
		isDefault := count == 0
		return scanProvider(tx.QueryRow(ctx, `
INSERT INTO llm_providers (name, provider_type, base_url, model, api_key_ciphertext, api_key_key_version, is_default)
VALUES ($1, $2, $3, $4, $5, $6, $7)
RETURNING id, name, provider_type, base_url, model, api_key_ciphertext, api_key_key_version, is_default, state, created_at, updated_at
`, name, string(providerType), baseURL, model, apiKeyCiphertext, keyVersion, isDefault), &p)
	})
	return p, err
}

// GetProvider returns a provider by ID.
func (s *Store) GetProvider(ctx context.Context, id int64) (domain.LLMProvider, error) {
	var p domain.LLMProvider
	err := scanProvider(s.pool.QueryRow(ctx, `
SELECT id, name, provider_type, base_url, model, api_key_ciphertext, api_key_key_version, is_default, state, created_at, updated_at
FROM llm_providers WHERE id = $1
`, id), &p)
	return p, err
}

// GetDefaultProvider returns the current default active provider.
func (s *Store) GetDefaultProvider(ctx context.Context) (domain.LLMProvider, error) {
	var p domain.LLMProvider
	err := scanProvider(s.pool.QueryRow(ctx, `
SELECT id, name, provider_type, base_url, model, api_key_ciphertext, api_key_key_version, is_default, state, created_at, updated_at
FROM llm_providers WHERE is_default = TRUE AND state = 'active'
`), &p)
	return p, err
}

// ListProviders returns all providers ordered by creation time.
func (s *Store) ListProviders(ctx context.Context) ([]domain.LLMProvider, error) {
	rows, err := s.pool.Query(ctx, `
SELECT id, name, provider_type, base_url, model, api_key_ciphertext, api_key_key_version, is_default, state, created_at, updated_at
FROM llm_providers ORDER BY created_at
`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanProviders(rows)
}

// UpdateProvider updates name/base_url/model/api_key of an existing provider.
func (s *Store) UpdateProvider(ctx context.Context, id int64, name string, providerType domain.LLMProviderType, baseURL, model string, apiKeyCiphertext []byte, keyVersion string) (domain.LLMProvider, error) {
	var p domain.LLMProvider
	err := s.withTx(ctx, func(tx pgx.Tx) error {
		return scanProvider(tx.QueryRow(ctx, `
UPDATE llm_providers SET
    name = $2, provider_type = $3, base_url = $4, model = $5,
    api_key_ciphertext = $6, api_key_key_version = $7, updated_at = $8
WHERE id = $1
RETURNING id, name, provider_type, base_url, model, api_key_ciphertext, api_key_key_version, is_default, state, created_at, updated_at
`, id, name, string(providerType), baseURL, model, apiKeyCiphertext, keyVersion, time.Now().UTC()), &p)
	})
	return p, err
}

// SetDefaultProvider marks the given provider as default and clears the flag
// from all other rows.
func (s *Store) SetDefaultProvider(ctx context.Context, id int64) error {
	return s.withTx(ctx, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE llm_providers SET is_default = FALSE WHERE id != $1`, id)
		if err != nil {
			return err
		}
		_ = tag
		tag, err = tx.Exec(ctx, `UPDATE llm_providers SET is_default = TRUE, updated_at = $2 WHERE id = $1`, id, time.Now().UTC())
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return fmt.Errorf("provider %d not found", id)
		}
		return nil
	})
}

// DeleteProvider hard-deletes a provider row.
func (s *Store) DeleteProvider(ctx context.Context, id int64) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM llm_providers WHERE id = $1`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("provider %d not found", id)
	}
	return nil
}

func scanProvider(row pgx.Row, p *domain.LLMProvider) error {
	return row.Scan(
		&p.ID, &p.Name, &p.ProviderType, &p.BaseURL, &p.Model,
		&p.APIKeyCiphertext, &p.APIKeyKeyVersion, &p.IsDefault, &p.State,
		&p.CreatedAt, &p.UpdatedAt,
	)
}

func scanProviders(rows pgx.Rows) ([]domain.LLMProvider, error) {
	var out []domain.LLMProvider
	for rows.Next() {
		var p domain.LLMProvider
		if err := scanProvider(rows, &p); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}
