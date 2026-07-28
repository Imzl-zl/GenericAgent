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

// AdminCreatePersona inserts an admin-authored persona in a single statement.
// When public is true the persona is published straight to the pool (approved,
// is_public=true, admin_id set) with no separate moderation step; otherwise it
// is stored private. This keeps admin publishing atomic (no pending orphan on
// a failed second step).
func (s *Store) AdminCreatePersona(ctx context.Context, adminUserID int64, name, description, systemPrompt string, isPublic bool) (domain.Persona, error) {
	if adminUserID <= 0 || name == "" || systemPrompt == "" {
		return domain.Persona{}, fmt.Errorf("author, name, and system prompt are required")
	}
	status := domain.PersonaPrivate
	var adminID *int64
	if isPublic {
		status = domain.PersonaApproved
		adminID = &adminUserID
	}
	var p domain.Persona
	err := s.pool.QueryRow(ctx, `
INSERT INTO personas (id, author_user_id, name, description, system_prompt, is_public, status, admin_id)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
RETURNING id, author_user_id, name, description, system_prompt, is_public, status, admin_id, admin_note, created_at, updated_at
`, uuid.New().String(), adminUserID, name, description, systemPrompt, isPublic, status, adminID).Scan(
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

// ListPersonasByAuthor returns every persona authored by the given user,
// regardless of lifecycle status. Used by the admin "my personas" view so the
// admin sees exactly the personas they created (including private/pending/
// rejected ones), not the whole pool.
func (s *Store) ListPersonasByAuthor(ctx context.Context, authorUserID int64) ([]domain.Persona, error) {
	rows, err := s.pool.Query(ctx, `
SELECT id, author_user_id, name, description, system_prompt, is_public, status, admin_id, admin_note, created_at, updated_at
FROM personas
WHERE author_user_id = $1
ORDER BY created_at DESC
`, authorUserID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanPersonas(rows)
}

// ListAllPersonas returns every persona for admin management. When status is
// non-empty it filters by that lifecycle status; otherwise all rows are returned.
func (s *Store) ListAllPersonas(ctx context.Context, status string) ([]domain.Persona, error) {
	const base = `
SELECT id, author_user_id, name, description, system_prompt, is_public, status, admin_id, admin_note, created_at, updated_at
FROM personas
`
	var (
		rows pgx.Rows
		err  error
	)
	if status == "" {
		rows, err = s.pool.Query(ctx, base+"ORDER BY created_at DESC")
	} else {
		rows, err = s.pool.Query(ctx, base+"WHERE status = $1 ORDER BY created_at DESC", status)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanPersonas(rows)
}

// UpdatePersona updates an existing persona owned by the user. Editing a
// persona that is currently approved sends it back to pending for re-review;
// pending personas cannot be edited while under review. Private and rejected
// personas keep their status.
func (s *Store) UpdatePersona(ctx context.Context, id string, authorUserID int64, name, description, systemPrompt string) (domain.Persona, error) {
	var p domain.Persona
	tag, err := s.pool.Exec(ctx, `
UPDATE personas
SET name = $3,
    description = $4,
    system_prompt = $5,
    status = CASE WHEN status = 'approved' THEN 'pending' ELSE status END,
    updated_at = $6
WHERE id = $1 AND author_user_id = $2 AND status <> 'pending'
`, id, authorUserID, name, description, systemPrompt, time.Now().UTC())
	if err != nil {
		return p, err
	}
	if tag.RowsAffected() == 0 {
		return p, fmt.Errorf("persona %s not found or under review", id)
	}
	return s.GetPersona(ctx, id)
}

// AdminUpdatePersona edits any persona regardless of author. The admin is the
// moderation authority, so the lifecycle status is preserved (an approved
// persona stays public after an admin edit).
func (s *Store) AdminUpdatePersona(ctx context.Context, id string, adminUserID int64, name, description, systemPrompt string) (domain.Persona, error) {
	var p domain.Persona
	tag, err := s.pool.Exec(ctx, `
UPDATE personas
SET name = $2, description = $3, system_prompt = $4, admin_id = $5, updated_at = $6
WHERE id = $1
`, id, name, description, systemPrompt, adminUserID, time.Now().UTC())
	if err != nil {
		return p, err
	}
	if tag.RowsAffected() == 0 {
		return p, fmt.Errorf("persona %s not found", id)
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

// AdminDeletePersona deletes any persona regardless of author. The users
// default_persona_id foreign key is ON DELETE SET NULL, so removing a persona
// automatically clears it from any user who had it as their default.
func (s *Store) AdminDeletePersona(ctx context.Context, id string) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM personas WHERE id = $1`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("persona %s not found", id)
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

// ModeratePersona approves or rejects a public persona. It works on any
// submitted persona (pending, approved, or rejected) so an admin can approve a
// pending submission, take down an already-approved persona (reject), or
// re-list a previously rejected one (approve). Approving forces is_public=true
// so the persona becomes visible in the public pool.
// Rejecting (taking down) a persona also clears it from every user who had it
// as their default, so no bot keeps applying a de-listed prompt. The two
// writes run in one transaction to avoid a window where the persona is
// rejected but stale defaults still point at it.
func (s *Store) ModeratePersona(ctx context.Context, id string, adminUserID int64, status domain.PersonaStatus, note string) error {
	if status != domain.PersonaApproved && status != domain.PersonaRejected {
		return fmt.Errorf("invalid moderation status %s", status)
	}
	isPublic := status == domain.PersonaApproved
	return s.withTx(ctx, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
UPDATE personas
SET status = $2, is_public = $3, admin_id = $4, admin_note = $5, updated_at = $6
WHERE id = $1 AND status IN ('pending','approved','rejected')
`, id, status, isPublic, adminUserID, note, time.Now().UTC())
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return fmt.Errorf("persona %s not found or not a public submission", id)
		}
		if status == domain.PersonaRejected {
			if _, err := tx.Exec(ctx, `
UPDATE users SET default_persona_id = NULL WHERE default_persona_id = $1
`, id); err != nil {
				return err
			}
		}
		return nil
	})
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
