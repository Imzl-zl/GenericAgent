// Package postgres — admin-configurable platform command store (migration 0004).
// 审查 D1(去分级): tool_policies 的 CRUD/用户分配存储已移除, 工具能力由静态
// policy manifest(foundation.v1.json) 决定, 不再有 DB 动态工具策略真值。
package postgres

import (
	"context"

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
