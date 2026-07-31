package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/domain"
)

func (s *Store) UpsertSophubBinding(ctx context.Context, binding domain.SophubBinding) (domain.SophubBinding, error) {
	if len(binding.APIKeyCiphertext) == 0 || binding.APIKeyVersion <= 0 {
		return domain.SophubBinding{}, fmt.Errorf("Sophub API key ciphertext and version are required")
	}
	if binding.UpdatedBy <= 0 {
		return domain.SophubBinding{}, fmt.Errorf("Sophub binding updater must be positive")
	}
	var stored domain.SophubBinding
	err := scanSophubBinding(s.pool.QueryRow(ctx, `
INSERT INTO sophub_bindings(
  id,api_key_ciphertext,api_key_version,remote_author_type,remote_agent_uid,
  remote_display_name,verified_at,updated_by
)
VALUES(TRUE,$1,$2,$3,$4,$5,timezone('utc',now()),$6)
ON CONFLICT(id) DO UPDATE SET
  api_key_ciphertext=EXCLUDED.api_key_ciphertext,
  api_key_version=EXCLUDED.api_key_version,
  remote_author_type=EXCLUDED.remote_author_type,
  remote_agent_uid=EXCLUDED.remote_agent_uid,
  remote_display_name=EXCLUDED.remote_display_name,
  verified_at=EXCLUDED.verified_at,
  updated_by=EXCLUDED.updated_by,
  updated_at=timezone('utc',now())
RETURNING api_key_ciphertext,api_key_version,remote_author_type,remote_agent_uid,
          remote_display_name,verified_at,updated_by,created_at,updated_at
`, binding.APIKeyCiphertext, binding.APIKeyVersion, binding.Identity.AuthorType,
		binding.Identity.AgentUID, binding.Identity.DisplayName, binding.UpdatedBy), &stored)
	return stored, err
}

func (s *Store) GetSophubBinding(ctx context.Context) (domain.SophubBinding, error) {
	var binding domain.SophubBinding
	err := scanSophubBinding(s.pool.QueryRow(ctx, `
SELECT api_key_ciphertext,api_key_version,remote_author_type,remote_agent_uid,
       remote_display_name,verified_at,updated_by,created_at,updated_at
FROM sophub_bindings WHERE id=TRUE
`), &binding)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.SophubBinding{}, domain.ErrSophubNotConfigured
	}
	return binding, err
}

func scanSophubBinding(row pgx.Row, binding *domain.SophubBinding) error {
	return row.Scan(
		&binding.APIKeyCiphertext, &binding.APIKeyVersion, &binding.Identity.AuthorType,
		&binding.Identity.AgentUID, &binding.Identity.DisplayName, &binding.VerifiedAt,
		&binding.UpdatedBy, &binding.CreatedAt, &binding.UpdatedAt,
	)
}
