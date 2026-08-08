package postgres

import (
	"context"
	"fmt"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/domain"
)

// ListMyTasks returns the requester's tasks ordered by creation time
// descending, capped at limit (user self-service status page). Tenant
// isolation is enforced by the requester_user_id predicate — the API layer
// always passes the authenticated user's own id.
func (s *Store) ListMyTasks(ctx context.Context, requesterUserID int64, limit int) ([]domain.Task, error) {
	if requesterUserID <= 0 {
		return nil, fmt.Errorf("requester user id must be positive")
	}
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	rows, err := s.pool.Query(ctx, `
SELECT `+taskSelectColumns+`
FROM tasks
WHERE requester_user_id = $1
ORDER BY created_at DESC
LIMIT $2
`, requesterUserID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var tasks []domain.Task
	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			return nil, err
		}
		tasks = append(tasks, t)
	}
	return tasks, rows.Err()
}

// CountMyTaskStats returns per-status task counts for the requester in a
// single GROUP BY round trip. Statuses with zero count are omitted from the
// map; callers fill zeros for display.
func (s *Store) CountMyTaskStats(ctx context.Context, requesterUserID int64) (map[domain.TaskStatus]int, error) {
	if requesterUserID <= 0 {
		return nil, fmt.Errorf("requester user id must be positive")
	}
	rows, err := s.pool.Query(ctx, `
SELECT status, COUNT(*)
FROM tasks
WHERE requester_user_id = $1
GROUP BY status
`, requesterUserID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	stats := make(map[domain.TaskStatus]int)
	for rows.Next() {
		var status domain.TaskStatus
		var count int
		if err := rows.Scan(&status, &count); err != nil {
			return nil, err
		}
		stats[status] = count
	}
	return stats, rows.Err()
}
