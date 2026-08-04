package postgres

import (
	"context"
	"fmt"
)

// IsTaskCapabilityActive 在线校验 capability 是否仍可合法使用(round9 审查:
// capability 的 task_id/runner_generation 只是签发时绑定的声明字段, JWT 校验
// 只证明 token 有效且未撤销; 本方法在调用时刻联查:
//   - task 仍处于 starting/running 且 claim lease 未过期(未被接管/恢复终态化);
//   - runner lease 的 generation 仍等于签发值且未过期(旧 generation Runner 失效);
//     loopback/dev 模式无 runner_leases 行(claim 由 loopback scheduler 维护,
//     且任务 claim 检查已独立生效), 此时跳过 lease 校验;
//   - 团队任务的 requester 仍是 approved 成员(成员移除即时生效, 不等 JTI 撤销)。
//
// llm-proxy 与 sophub proxy 在每次调用前执行, 把撤销/接管/成员变更的生效
// 时间从"token TTL"收敛到"下一次调用"。
func (s *Store) IsTaskCapabilityActive(ctx context.Context, taskID string, runnerGeneration uint64) (bool, error) {
	if taskID == "" || runnerGeneration == 0 {
		return false, fmt.Errorf("task id and runner generation are required")
	}
	var active bool
	err := s.pool.QueryRow(ctx, `
SELECT
  t.status IN ('starting','running')
  AND t.claim_lease_until > timezone('utc', now())
  AND (
    NOT EXISTS (SELECT 1 FROM runner_leases rl0 WHERE rl0.runner_key = t.session_key)
    OR EXISTS (
      SELECT 1 FROM runner_leases rl
      WHERE rl.runner_key = t.session_key
        AND rl.generation = $2
        AND rl.expires_at > timezone('utc', now())
    )
  )
  AND (
    w.kind = 'personal'
    OR EXISTS (
      SELECT 1 FROM team_members tm
      WHERE tm.team_id = w.team_id
        AND tm.user_id = t.requester_user_id
        AND tm.status = 'approved'
    )
  )
FROM tasks t
JOIN workspaces w ON w.id = t.workspace_id
WHERE t.id = $1
`, taskID, int64(runnerGeneration)).Scan(&active)
	if err != nil {
		return false, fmt.Errorf("check task capability active: %w", err)
	}
	return active, nil
}
