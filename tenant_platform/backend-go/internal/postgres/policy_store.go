// Package postgres — admin-configurable command registry + tool policy store
// (migration 0004).
package postgres

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/domain"
)

// ListEnabledCommands returns all enabled platform commands, ordered by sort_order.
// Called by the router's cache to decide which commands to intercept.
func (s *Store) ListEnabledCommands(ctx context.Context) ([]domain.PlatformCommand, error) {
	rows, err := s.pool.Query(ctx, `
SELECT id, command, action, handler, COALESCE(help_text, ''), enabled, sort_order,
       COALESCE(updated_by, 0), updated_at
FROM platform_commands
WHERE enabled = true
ORDER BY sort_order ASC, id ASC
`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.PlatformCommand
	for rows.Next() {
		var c domain.PlatformCommand
		if err := rows.Scan(&c.ID, &c.Command, &c.Action, &c.Handler, &c.HelpText,
			&c.Enabled, &c.SortOrder, &c.UpdatedBy, &c.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// CommandRegistryVersion returns a cheap fingerprint of the current command
// registry state (MAX(updated_at)). The router caches this for 5 seconds and
// only reloads the full command list when the version changes — avoiding
// per-message DB load while keeping config changes near-real-time.
func (s *Store) CommandRegistryVersion(ctx context.Context) (string, error) {
	var version *string
	err := s.pool.QueryRow(ctx, `
SELECT MAX(updated_at)::text FROM platform_commands WHERE enabled = true
`).Scan(&version)
	if err != nil {
		return "", err
	}
	if version == nil {
		return "", nil // no enabled commands
	}
	return *version, nil
}

// ListAllCommands returns all platform commands (including disabled), for admin UI.
func (s *Store) ListAllCommands(ctx context.Context) ([]domain.PlatformCommand, error) {
	rows, err := s.pool.Query(ctx, `
SELECT id, command, action, handler, COALESCE(help_text, ''), enabled, sort_order,
       COALESCE(updated_by, 0), updated_at
FROM platform_commands
ORDER BY sort_order ASC, id ASC
`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.PlatformCommand
	for rows.Next() {
		var c domain.PlatformCommand
		if err := rows.Scan(&c.ID, &c.Command, &c.Action, &c.Handler, &c.HelpText,
			&c.Enabled, &c.SortOrder, &c.UpdatedBy, &c.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// UpdateCommand updates an existing platform command's action, help_text, enabled,
// and sort_order. Admin-only; handler key is immutable (changing handler semantics
// requires code). Returns the updated command.
func (s *Store) UpdateCommand(ctx context.Context, id int64, action domain.CommandAction,
	helpText string, enabled bool, sortOrder int, updatedBy int64) (domain.PlatformCommand, error) {
	var c domain.PlatformCommand
	err := s.pool.QueryRow(ctx, `
UPDATE platform_commands
SET action = $2, help_text = $3, enabled = $4, sort_order = $5,
    updated_by = $6, updated_at = timezone('utc', now())
WHERE id = $1
RETURNING id, command, action, handler, COALESCE(help_text, ''), enabled, sort_order,
          COALESCE(updated_by, 0), updated_at
`, id, action, helpText, enabled, sortOrder, updatedBy).Scan(
	&c.ID, &c.Command, &c.Action, &c.Handler, &c.HelpText,
	&c.Enabled, &c.SortOrder, &c.UpdatedBy, &c.UpdatedAt)
	return c, err
}

// GetToolPolicy returns a tool policy by its version string.
// The router/task submission uses this to resolve the version → allowed_tools.
func (s *Store) GetToolPolicy(ctx context.Context, version string) (domain.ToolPolicy, error) {
	var p domain.ToolPolicy
	var toolsJSON []byte
	err := s.pool.QueryRow(ctx, `
SELECT id, version, allowed_tools, COALESCE(description, ''), enabled,
       COALESCE(created_by, 0), created_at, updated_at
FROM tool_policies
WHERE version = $1 AND enabled = true
`, version).Scan(&p.ID, &p.Version, &toolsJSON, &p.Description, &p.Enabled,
	&p.CreatedBy, &p.CreatedAt, &p.UpdatedAt)
	if err != nil {
		return domain.ToolPolicy{}, err
	}
	if err := json.Unmarshal(toolsJSON, &p.AllowedTools); err != nil {
		return domain.ToolPolicy{}, fmt.Errorf("unmarshal allowed_tools: %w", err)
	}
	return p, nil
}

// ListToolPolicies returns all tool policies (including disabled), for admin UI.
func (s *Store) ListToolPolicies(ctx context.Context) ([]domain.ToolPolicy, error) {
	rows, err := s.pool.Query(ctx, `
SELECT id, version, allowed_tools, COALESCE(description, ''), enabled,
       COALESCE(created_by, 0), created_at, updated_at
FROM tool_policies
ORDER BY created_at DESC
`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.ToolPolicy
	for rows.Next() {
		var p domain.ToolPolicy
		var toolsJSON []byte
		if err := rows.Scan(&p.ID, &p.Version, &toolsJSON, &p.Description, &p.Enabled,
			&p.CreatedBy, &p.CreatedAt, &p.UpdatedAt); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(toolsJSON, &p.AllowedTools); err != nil {
			return nil, fmt.Errorf("unmarshal allowed_tools: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// CreateToolPolicy creates a new tool policy version. Admin-only.
// allowedTools is serialized to JSONB for storage.
func (s *Store) CreateToolPolicy(ctx context.Context, version, description string,
	allowedTools []string, createdBy int64) (domain.ToolPolicy, error) {
	if version == "" {
		return domain.ToolPolicy{}, fmt.Errorf("version is required")
	}
	toolsJSON, err := json.Marshal(allowedTools)
	if err != nil {
		return domain.ToolPolicy{}, fmt.Errorf("marshal allowed_tools: %w", err)
	}
	var p domain.ToolPolicy
	var storedJSON []byte
	err = s.pool.QueryRow(ctx, `
INSERT INTO tool_policies (version, allowed_tools, description, created_by)
VALUES ($1, $2::jsonb, $3, $4)
RETURNING id, version, allowed_tools, COALESCE(description, ''), enabled,
          COALESCE(created_by, 0), created_at, updated_at
`, version, toolsJSON, description, createdBy).Scan(
	&p.ID, &p.Version, &storedJSON, &p.Description, &p.Enabled,
	&p.CreatedBy, &p.CreatedAt, &p.UpdatedAt)
	if err != nil {
		return domain.ToolPolicy{}, err
	}
	if err := json.Unmarshal(storedJSON, &p.AllowedTools); err != nil {
		return domain.ToolPolicy{}, fmt.Errorf("unmarshal allowed_tools: %w", err)
	}
	return p, nil
}

// EnsureToolPolicyExists is a bootstrap helper that inserts a policy if it
// doesn't exist yet (used by main.go to seed from the JSON file on first run).
func (s *Store) EnsureToolPolicyExists(ctx context.Context, version, description string,
	allowedTools []string) error {
	_, err := s.GetToolPolicy(ctx, version)
	if err == nil {
		return nil // already exists
	}
	if err != pgx.ErrNoRows {
		return err
	}
	_, err = s.CreateToolPolicy(ctx, version, description, allowedTools, 0)
	return err
}

// GetUserToolPolicy returns the tool policy version assigned to a user.
// Per-user policy (migration 0005) replaces the global policy — admins can
// grant different capabilities to different users at runtime.
func (s *Store) GetUserToolPolicy(ctx context.Context, userID int64) (string, error) {
	if userID <= 0 {
		return "", fmt.Errorf("user id must be positive")
	}
	var version string
	err := s.pool.QueryRow(ctx, `
SELECT tool_policy_version FROM users WHERE id = $1
`, userID).Scan(&version)
	if err != nil {
		return "", fmt.Errorf("get user tool policy: %w", err)
	}
	return version, nil
}

// UpdateUserToolPolicy changes the tool policy version assigned to a user.
// Admin-only; takes effect on the user's next task submission.
func (s *Store) UpdateUserToolPolicy(ctx context.Context, userID int64, version string, updatedBy int64) error {
	if userID <= 0 {
		return fmt.Errorf("user id must be positive")
	}
	if version == "" {
		return fmt.Errorf("tool policy version is required")
	}
	tag, err := s.pool.Exec(ctx, `
UPDATE users SET tool_policy_version = $2, updated_at = timezone('utc', now())
WHERE id = $1
`, userID, version)
	if err != nil {
		return fmt.Errorf("update user tool policy: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("user %d not found", userID)
	}
	return nil
}

var _ = time.Now // keep import for future timestamp helpers
