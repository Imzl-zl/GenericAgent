package postgres

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/domain"
)

// ErrRunnerLeaseOwned is returned when a live lease is owned by another owner.
var ErrRunnerLeaseOwned = errors.New("runner lease owned by another platform instance")

// ErrRunnerLeaseCapacity is returned when creating a new Runner lease would
// exceed the global active-Runner limit (GA_RUNNER_MAX_ACTIVE).
var ErrRunnerLeaseCapacity = errors.New("runner lease capacity exceeded")

// runnerCapacityLockKey 是容量检查的 advisory lock 常量键: 所有 Platform
// 实例在同一 Postgres 上串行化 count+insert, 保证全局上限不被并发超卖。
const runnerCapacityLockKey = 0x47524143 // "GRAC"

// AcquireRunnerLease obtains or reuses the lease for a runner_key.
//   - No row or expired row: creates a new generation (existing generation + 1)
//     with the caller as owner and reports created=true. If maxActive > 0 and
//     creating a new lease would exceed the active lease count, fails with
//     ErrRunnerLeaseCapacity (advisory-lock serialized, multi-instance safe).
//   - Live row with same owner: refreshes expiry, keeps generation, created=false.
//   - Live row with different owner: fails with ErrRunnerLeaseOwned.
func (s *Store) AcquireRunnerLease(ctx context.Context, runnerKey, owner string, leaseTTL time.Duration, maxActive int64) (domain.RunnerLease, bool, error) {
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
		if maxActive > 0 {
			// 容量检查仅作用于“需要新建 lease”的分支; 续租不占新名额。
			if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, runnerCapacityLockKey); err != nil {
				return fmt.Errorf("acquire runner capacity lock: %w", err)
			}
		}
		var current domain.RunnerLease
		rowErr := scanRunnerLease(tx.QueryRow(ctx, `
SELECT runner_key, owner, generation, container_id, stale_container_id, control_endpoint, status, expires_at, created_at, updated_at
FROM runner_leases WHERE runner_key = $1
`, runnerKey), &current)
		if rowErr != nil && !errors.Is(rowErr, pgx.ErrNoRows) {
			return rowErr
		}

		if rowErr == nil {
			if current.IsExpired(time.Now().UTC()) {
				if maxActive > 0 {
					active, err := s.countActiveRunnerLeases(ctx, tx)
					if err != nil {
						return err
					}
					if active >= maxActive {
						return ErrRunnerLeaseCapacity
					}
				}
				// 过期或已释放:以新 owner 接管,generation 单调 +1,清空容器与端点。
				created = true
				taken, takeErr := s.takeoverRunnerLease(ctx, tx, runnerKey, owner, expiresAt)
				if takeErr != nil {
					return takeErr
				}
				lease = taken
				return nil
			}
			if current.Owner != owner {
				// 审查: 活跃 lease 异主时, 若 owner 已无任何活跃 task claim
				// (Platform 崩溃重启/长期停机), 允许接管并重建容器, 否则视为
				// 被其他活跃实例持有。判定用持久 task claim, 不依赖进程内状态。
				ownerBusy, err := s.ownerHasActiveTaskClaims(ctx, tx, runnerKey, current.Owner)
				if err != nil {
					return err
				}
				if ownerBusy {
					return fmt.Errorf("%w: %s", ErrRunnerLeaseOwned, current.Owner)
				}
				created = true
				taken, takeErr := s.takeoverRunnerLease(ctx, tx, runnerKey, owner, expiresAt)
				if takeErr != nil {
					return takeErr
				}
				lease = taken
				return nil
			}
			// 同 owner 活跃:仅刷新到期时间,保持 generation 与容器。
			created = false
			return scanRunnerLease(tx.QueryRow(ctx, `
UPDATE runner_leases
SET expires_at = $2, updated_at = timezone('utc', now())
WHERE runner_key = $1
RETURNING runner_key, owner, generation, container_id, stale_container_id, control_endpoint, status, expires_at, created_at, updated_at
`, runnerKey, expiresAt), &lease)
		}

		if maxActive > 0 {
			active, err := s.countActiveRunnerLeases(ctx, tx)
			if err != nil {
				return err
			}
			if active >= maxActive {
				return ErrRunnerLeaseCapacity
			}
		}
		return scanRunnerLease(tx.QueryRow(ctx, `
INSERT INTO runner_leases (runner_key, owner, generation, status, expires_at)
VALUES ($1, $2, 1, 'active', $3)
RETURNING runner_key, owner, generation, container_id, stale_container_id, control_endpoint, status, expires_at, created_at, updated_at
`, runnerKey, owner, expiresAt), &lease)
	})
	if err != nil {
		return domain.RunnerLease{}, false, err
	}
	return lease, created, nil
}

// takeoverRunnerLease 以新 owner 接管 lease(generation 单调 +1, 旧容器
// 移入 stale_container_id 供定向清理)并返回接管后的完整 lease 行。
// 过期接管与异主陈旧接管共用; 返回值必须回传给调用方, 否则调用方拿到
// 的是零值 lease(generation=0), 后续签发/续租会丢失 generation 绑定。
func (s *Store) takeoverRunnerLease(ctx context.Context, tx pgx.Tx, runnerKey, owner string, expiresAt time.Time) (domain.RunnerLease, error) {
	var lease domain.RunnerLease
	err := scanRunnerLease(tx.QueryRow(ctx, `
UPDATE runner_leases
SET owner = $2, generation = generation + 1, container_id = '',
    stale_container_id = CASE WHEN container_id <> '' THEN container_id ELSE stale_container_id END,
    control_endpoint = '',
    status = 'active', expires_at = $3, updated_at = timezone('utc', now())
WHERE runner_key = $1
RETURNING runner_key, owner, generation, container_id, stale_container_id, control_endpoint, status, expires_at, created_at, updated_at
`, runnerKey, owner, expiresAt), &lease)
	return lease, err
}

// ownerHasActiveTaskClaims 判断 lease owner 是否仍有活跃 task claim
// (starting/running 且 claim 未过期)。用于异主接管判定(审查: 崩溃重启后
// 新实例可接管旧 owner 已无活跃任务的 lease, 无需等待 TTL)。
func (s *Store) ownerHasActiveTaskClaims(ctx context.Context, tx pgx.Tx, runnerKey, owner string) (bool, error) {
	var exists bool
	if err := tx.QueryRow(ctx, `
SELECT EXISTS(
  SELECT 1 FROM tasks
  WHERE session_key = $1 AND claim_owner = $2
    AND status IN ('starting','running')
    AND claim_lease_until > timezone('utc', now())
)
`, runnerKey, owner).Scan(&exists); err != nil {
		return false, fmt.Errorf("check owner active task claims: %w", err)
	}
	return exists, nil
}

// countActiveRunnerLeases counts non-expired leases inside the caller's
// transaction (capacity check; advisory lock held by AcquireRunnerLease).
func (s *Store) countActiveRunnerLeases(ctx context.Context, tx pgx.Tx) (int64, error) {
	var count int64
	if err := tx.QueryRow(ctx, `
SELECT count(*) FROM runner_leases WHERE expires_at > timezone('utc', now())
`).Scan(&count); err != nil {
		return 0, fmt.Errorf("count active runner leases: %w", err)
	}
	return count, nil
}

// CountActiveRunnerLeases returns the number of non-expired Runner leases
// (scheduler capacity check for GA_RUNNER_MAX_ACTIVE).
func (s *Store) CountActiveRunnerLeases(ctx context.Context, now time.Time) (int64, error) {
	var count int64
	if err := s.pool.QueryRow(ctx, `
SELECT count(*) FROM runner_leases WHERE expires_at > $1
`, now.UTC()).Scan(&count); err != nil {
		return 0, fmt.Errorf("count active runner leases: %w", err)
	}
	return count, nil
}

// RenewRunnerLease extends the expiry of a live lease owned by owner at the
// given generation. The generation condition (审查 C6) fences stale renewers:
// 同一实例的旧 cleanup/renewer 无法续租后来创建的新 generation lease。
func (s *Store) RenewRunnerLease(ctx context.Context, runnerKey, owner string, generation uint64, leaseTTL time.Duration) error {
	if runnerKey == "" || owner == "" || leaseTTL <= 0 {
		return fmt.Errorf("runner key, owner and positive ttl are required")
	}
	if generation == 0 {
		return fmt.Errorf("runner generation must be positive")
	}
	expiresAt := time.Now().UTC().Add(leaseTTL)
	tag, err := s.pool.Exec(ctx, `
UPDATE runner_leases SET expires_at = $4, updated_at = timezone('utc', now())
WHERE runner_key = $1 AND owner = $2 AND generation = $3 AND expires_at > timezone('utc', now())
`, runnerKey, owner, generation, expiresAt)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("runner lease %s not owned by %s at generation %d", runnerKey, owner, generation)
	}
	return nil
}

// AttachRunnerContainer binds the immutable container ID to the lease,
// guarded by the generation so a stale attach from an older Runner cannot
// overwrite the current lease (generation fencing).
// 审查 R5-I6: 附加 owner + lease 有效期 + 不可变 container_id 条件——异主/
// 过期/覆盖不同 ID 的 attach 一律拒绝(RowsAffected=0), 同值重试幂等成功。
func (s *Store) AttachRunnerContainer(ctx context.Context, runnerKey, containerID string, generation uint64, owner string) error {
	if runnerKey == "" || containerID == "" || strings.TrimSpace(owner) == "" {
		return fmt.Errorf("runner key, container id and owner are required")
	}
	tag, err := s.pool.Exec(ctx, `
UPDATE runner_leases SET container_id = $2, updated_at = timezone('utc', now())
WHERE runner_key = $1 AND generation = $3 AND owner = $4
  AND expires_at > timezone('utc', now())
  AND (container_id = '' OR container_id = $2)
`, runnerKey, containerID, generation, owner)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("runner lease %s generation %d not owned by %s or container id already bound", runnerKey, generation, owner)
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
// takes over with generation + 1. The generation condition (审查 C6) prevents
// a stale cleanup from releasing a newer generation's lease.
// round10 审查(B1): release 语义是"容器已销毁、lease 归还"——必须同时清空
// container_id/stale_container_id/control_endpoint, 否则接管事务会把已删除的
// 容器 ID 移入 stale_container_id, 下次 Start 对不存在的容器销毁失败
// (Manager 归属校验拒绝), 该工作区永久无法重建 Runner。
func (s *Store) ReleaseRunnerLease(ctx context.Context, runnerKey, owner string, generation uint64) error {
	if generation == 0 {
		return fmt.Errorf("runner generation must be positive")
	}
	tag, err := s.pool.Exec(ctx, `
UPDATE runner_leases
SET expires_at = timezone('utc', now()) - interval '1 second',
    container_id = '',
    stale_container_id = '',
    control_endpoint = '',
    updated_at = timezone('utc', now())
WHERE runner_key = $1 AND owner = $2 AND generation = $3
`, runnerKey, owner, generation)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("runner lease %s not owned by %s at generation %d", runnerKey, owner, generation)
	}
	return nil
}

// GetRunnerLease returns the current lease for a runner_key.
func (s *Store) GetRunnerLease(ctx context.Context, runnerKey string) (domain.RunnerLease, error) {
	var lease domain.RunnerLease
	err := scanRunnerLease(s.pool.QueryRow(ctx, `
SELECT runner_key, owner, generation, container_id, stale_container_id, control_endpoint, status, expires_at, created_at, updated_at
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
SELECT runner_key, owner, generation, container_id, stale_container_id, control_endpoint, status, expires_at, created_at, updated_at
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
		&lease.StaleContainerID, &lease.ControlEndpoint, &lease.Status, &lease.ExpiresAt, &lease.CreatedAt, &lease.UpdatedAt)
}
