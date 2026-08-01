package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/domain"
)

// ErrRunnerLeaseOwned is returned when a live lease is owned by another owner.
var ErrRunnerLeaseOwned = errors.New("runner lease owned by another platform instance")

// AcquireRunnerLease obtains or reuses the lease for a runner_key.
// - No row or expired row: creates a new generation (existing generation + 1)
//   with the caller as owner and reports created=true.
// - Live row with same owner: refreshes expiry, keeps generation, created=false.
// - Live row with different owner: fails with ErrRunnerLeaseOwned.
func (s *Store) AcquireRunnerLease(ctx context.Context, runnerKey, owner string, leaseTTL time.Duration) (domain.RunnerLease, bool, error) {
	if runnerKey == "" || owner == "" {
		return domain.RunnerLease{}, false, fmt.Errorf("runner key and owner are required")
	}
	if leaseTTL <= 0 {
		return domain.RunnerLease{}, false, fmt.Errorf("lease ttl must be positive")
	}
	expiresAt := time.Now().UTC().Add(leaseTTL)

	var lease domain.RunnerLease
	created := true
	err := s.withTx(ctx, func(tx pgx.Tx) error {
		var current domain.RunnerLease
		rowErr := scanRunnerLease(tx.QueryRow(ctx, `
SELECT runner_key, owner, generation, container_id, control_endpoint, status, expires_at, created_at, updated_at
FROM runner_leases WHERE runner_key = $1
`, runnerKey), &current)
		if rowErr != nil && !errors.Is(rowErr, pgx.ErrNoRows) {
			return rowErr
		}

		if rowErr == nil {
			if current.IsExpired(time.Now().UTC()) {
				// 过期或已释放:以新 owner 接管,generation 单调 +1,清空容器与端点。
				created = true
				return scanRunnerLease(tx.QueryRow(ctx, `
UPDATE runner_leases
SET owner = $2, generation = generation + 1, container_id = '', control_endpoint = '',
    status = 'active', expires_at = $3, updated_at = timezone('utc', now())
WHERE runner_key = $1
RETURNING runner_key, owner, generation, container_id, control_endpoint, status, expires_at, created_at, updated_at
`, runnerKey, owner, expiresAt), &lease)
			}
			if current.Owner != owner {
				return fmt.Errorf("%w: %s", ErrRunnerLeaseOwned, current.Owner)
			}
			// 同 owner 活跃:仅刷新到期时间,保持 generation 与容器。
			created = false
			return scanRunnerLease(tx.QueryRow(ctx, `
UPDATE runner_leases
SET expires_at = $2, updated_at = timezone('utc', now())
WHERE runner_key = $1
RETURNING runner_key, owner, generation, container_id, control_endpoint, status, expires_at, created_at, updated_at
`, runnerKey, expiresAt), &lease)
		}

		return scanRunnerLease(tx.QueryRow(ctx, `
INSERT INTO runner_leases (runner_key, owner, generation, status, expires_at)
VALUES ($1, $2, 1, 'active', $3)
RETURNING runner_key, owner, generation, container_id, control_endpoint, status, expires_at, created_at, updated_at
`, runnerKey, owner, expiresAt), &lease)
	})
	if err != nil {
		return domain.RunnerLease{}, false, err
	}
	return lease, created, nil
}

// AttachRunnerContainer binds the immutable container ID to the lease.
func (s *Store) AttachRunnerContainer(ctx context.Context, runnerKey, containerID string) error {
	if runnerKey == "" || containerID == "" {
		return fmt.Errorf("runner key and container id are required")
	}
	tag, err := s.pool.Exec(ctx, `
UPDATE runner_leases SET container_id = $2, updated_at = timezone('utc', now())
WHERE runner_key = $1
`, runnerKey, containerID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("runner lease %s not found", runnerKey)
	}
	return nil
}

// SetRunnerControlEndpoint records the Runner health/control endpoint.
func (s *Store) SetRunnerControlEndpoint(ctx context.Context, runnerKey, endpoint string) error {
	if runnerKey == "" || endpoint == "" {
		return fmt.Errorf("runner key and endpoint are required")
	}
	tag, err := s.pool.Exec(ctx, `
UPDATE runner_leases SET control_endpoint = $2, updated_at = timezone('utc', now())
WHERE runner_key = $1
`, runnerKey, endpoint)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("runner lease %s not found", runnerKey)
	}
	return nil
}

// ReleaseRunnerLease marks the lease immediately expired so the next acquire
// takes over with generation + 1. The row (and its generation counter) is kept
// so the generation sequence never regresses; the actual row can be removed by
// orphan cleanup once it is not referenced by any live Runner.
func (s *Store) ReleaseRunnerLease(ctx context.Context, runnerKey, owner string) error {
	tag, err := s.pool.Exec(ctx, `
UPDATE runner_leases
SET expires_at = timezone('utc', now()) - interval '1 second', updated_at = timezone('utc', now())
WHERE runner_key = $1 AND owner = $2
`, runnerKey, owner)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("runner lease %s not owned by %s", runnerKey, owner)
	}
	return nil
}

// GetRunnerLease returns the current lease for a runner_key.
func (s *Store) GetRunnerLease(ctx context.Context, runnerKey string) (domain.RunnerLease, error) {
	var lease domain.RunnerLease
	err := scanRunnerLease(s.pool.QueryRow(ctx, `
SELECT runner_key, owner, generation, container_id, control_endpoint, status, expires_at, created_at, updated_at
FROM runner_leases WHERE runner_key = $1
`, runnerKey), &lease)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.RunnerLease{}, fmt.Errorf("runner lease %s not found", runnerKey)
	}
	return lease, err
}

// ListExpiredRunnerLeases returns leases past the given time (orphan cleanup).
func (s *Store) ListExpiredRunnerLeases(ctx context.Context, now time.Time) ([]domain.RunnerLease, error) {
	rows, err := s.pool.Query(ctx, `
SELECT runner_key, owner, generation, container_id, control_endpoint, status, expires_at, created_at, updated_at
FROM runner_leases WHERE expires_at <= $1
ORDER BY expires_at
`, now.UTC())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var leases []domain.RunnerLease
	for rows.Next() {
		var lease domain.RunnerLease
		if err := scanRunnerLease(rows, &lease); err != nil {
			return nil, err
		}
		leases = append(leases, lease)
	}
	return leases, rows.Err()
}

func scanRunnerLease(row pgx.Row, lease *domain.RunnerLease) error {
	return row.Scan(&lease.RunnerKey, &lease.Owner, &lease.Generation, &lease.ContainerID,
		&lease.ControlEndpoint, &lease.Status, &lease.ExpiresAt, &lease.CreatedAt, &lease.UpdatedAt)
}
