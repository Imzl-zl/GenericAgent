package postgres

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/domain"
)

// CreatePersona inserts a new persona.
func (s *Store) CreatePersona(ctx context.Context, authorUserID int64, name, description, systemPrompt string, isPublic bool) (domain.Persona, error) {
	if authorUserID <= 0 || name == "" || systemPrompt == "" {
		return domain.Persona{}, fmt.Errorf("author, name, and system prompt are required")
	}
	status := domain.PersonaPrivate
	if isPublic {
		status = domain.PersonaPending
	}
	var p domain.Persona
	err := s.pool.QueryRow(ctx, `
INSERT INTO personas (id, author_user_id, name, description, system_prompt, is_public, status)
VALUES ($1, $2, $3, $4, $5, $6, $7)
RETURNING id, author_user_id, name, description, system_prompt, is_public, status, admin_id, admin_note, created_at, updated_at
`, uuid.New().String(), authorUserID, name, description, systemPrompt, isPublic, status).Scan(
		&p.ID, &p.AuthorUserID, &p.Name, &p.Description, &p.SystemPrompt, &p.IsPublic, &p.Status, &p.AdminID, &p.AdminNote, &p.CreatedAt, &p.UpdatedAt,
	)
	return p, err
}

// GetPersona returns a persona by id.
func (s *Store) GetPersona(ctx context.Context, id string) (domain.Persona, error) {
	var p domain.Persona
	err := s.pool.QueryRow(ctx, `
SELECT id, author_user_id, name, description, system_prompt, is_public, status, admin_id, admin_note, created_at, updated_at
FROM personas WHERE id = $1
`, id).Scan(
		&p.ID, &p.AuthorUserID, &p.Name, &p.Description, &p.SystemPrompt, &p.IsPublic, &p.Status, &p.AdminID, &p.AdminNote, &p.CreatedAt, &p.UpdatedAt,
	)
	return p, err
}

// ListPersonasVisibleTo returns approved public personas plus the user's own personas.
func (s *Store) ListPersonasVisibleTo(ctx context.Context, userID int64) ([]domain.Persona, error) {
	rows, err := s.pool.Query(ctx, `
SELECT id, author_user_id, name, description, system_prompt, is_public, status, admin_id, admin_note, created_at, updated_at
FROM personas
WHERE author_user_id = $1 OR (is_public = true AND status = 'approved')
ORDER BY created_at DESC
`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanPersonas(rows)
}

// ListPendingPublicPersonas returns personas submitted for public review.
func (s *Store) ListPendingPublicPersonas(ctx context.Context) ([]domain.Persona, error) {
	rows, err := s.pool.Query(ctx, `
SELECT id, author_user_id, name, description, system_prompt, is_public, status, admin_id, admin_note, created_at, updated_at
FROM personas
WHERE status = 'pending'
ORDER BY created_at DESC
`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanPersonas(rows)
}

// UpdatePersona updates an existing persona if owned by the user.
func (s *Store) UpdatePersona(ctx context.Context, id string, authorUserID int64, name, description, systemPrompt string) (domain.Persona, error) {
	var p domain.Persona
	tag, err := s.pool.Exec(ctx, `
UPDATE personas
SET name = $3, description = $4, system_prompt = $5, updated_at = $6
WHERE id = $1 AND author_user_id = $2 AND status NOT IN ('pending','approved')
`, id, authorUserID, name, description, systemPrompt, time.Now().UTC())
	if err != nil {
		return p, err
	}
	if tag.RowsAffected() == 0 {
		return p, fmt.Errorf("persona %s not found or not editable in current state", id)
	}
	return s.GetPersona(ctx, id)
}

// DeletePersona deletes a persona if owned by the user.
func (s *Store) DeletePersona(ctx context.Context, id string, authorUserID int64) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM personas WHERE id = $1 AND author_user_id = $2`, id, authorUserID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("persona %s not found or not owned by user", id)
	}
	return nil
}

// SubmitPersonaForReview marks a private persona as pending public approval.
func (s *Store) SubmitPersonaForReview(ctx context.Context, id string, authorUserID int64) error {
	tag, err := s.pool.Exec(ctx, `
UPDATE personas
SET status = 'pending', is_public = true, updated_at = $3
WHERE id = $1 AND author_user_id = $2 AND status = 'private'
`, id, authorUserID, time.Now().UTC())
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("persona %s not found or not in private state", id)
	}
	return nil
}

// ModeratePersona approves or rejects a pending public persona.
func (s *Store) ModeratePersona(ctx context.Context, id string, adminUserID int64, status domain.PersonaStatus, note string) error {
	if status != domain.PersonaApproved && status != domain.PersonaRejected {
		return fmt.Errorf("invalid moderation status %s", status)
	}
	tag, err := s.pool.Exec(ctx, `
UPDATE personas
SET status = $3, admin_id = $4, admin_note = $5, updated_at = $6
WHERE id = $1 AND status = 'pending'
`, id, adminUserID, status, adminUserID, note, time.Now().UTC())
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("persona %s not found or not pending", id)
	}
	return nil
}

// SetUserDefaultPersona sets the user's default persona.
func (s *Store) SetUserDefaultPersona(ctx context.Context, userID int64, personaID *string) error {
	if personaID != nil && *personaID == "" {
		return fmt.Errorf("persona id must not be empty")
	}
	_, err := s.pool.Exec(ctx, `
UPDATE users SET default_persona_id = $2 WHERE id = $1
`, userID, personaID)
	return err
}

func scanPersonas(rows pgx.Rows) ([]domain.Persona, error) {
	var out []domain.Persona
	for rows.Next() {
		var p domain.Persona
		if err := rows.Scan(
			&p.ID, &p.AuthorUserID, &p.Name, &p.Description, &p.SystemPrompt, &p.IsPublic, &p.Status, &p.AdminID, &p.AdminNote, &p.CreatedAt, &p.UpdatedAt,
		); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}
