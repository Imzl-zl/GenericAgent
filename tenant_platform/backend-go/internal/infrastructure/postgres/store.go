// Package postgres implements the platform task store against PostgreSQL.
package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// 应用级上限常量已上移 domain(审查 F1): 见 domain/limits.go。

// AdminContext is the approved loopback user/workspace pair.
type AdminContext struct {
	UserID      int64
	Username    string
	WorkspaceID string
	SessionKey  string
}

// Store is the PostgreSQL-backed task store.
type Store struct {
	pool                            *pgxpool.Pool
	perUserQueueLimit               int // 0 = disabled (dev/test); enforced inside SubmitTask tx
	runningTaskLimit                int // 0 = disabled (dev/test); enforced inside ClaimNextTask tx (审查 D4)
}

type StoreOption func(*Store) error

// NewStore wraps a pgx pool.
func NewStore(pool *pgxpool.Pool, options ...StoreOption) (*Store, error) {
	if pool == nil {
		return nil, fmt.Errorf("pool is nil")
	}
	store := &Store{pool: pool}
	for _, option := range options {
		if option == nil {
			return nil, fmt.Errorf("store option is nil")
		}
		if err := option(store); err != nil {
			return nil, err
		}
	}
	return store, nil
}

// SetPerUserQueueLimit sets the per-requester queued-task cap. Must be called
// before the scheduler starts. 0 disables the check (dev/test only).
func (s *Store) SetPerUserQueueLimit(limit int) {
	s.perUserQueueLimit = limit
}

// SetRunningTaskLimit sets the global starting+running task cap (审查 D4).
// Must be called before the scheduler starts. 0 disables the check.
// 与 scheduler 侧的 MaxRunningTasks 预检查不同, 这里在 ClaimNextTask 的
// 同一事务内以 advisory lock 串行化计数+claim, 跨 Platform 实例原子——
// 多个实例同时观察到 limit-1 时, 只有一个能成功 claim, 不会超卖。
func (s *Store) SetRunningTaskLimit(limit int) {
	s.runningTaskLimit = limit
}

// Pool exposes the underlying pool for tests.
func (s *Store) Pool() *pgxpool.Pool { return s.pool }

func (s *Store) withTx(ctx context.Context, fn func(pgx.Tx) error) error {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := fn(tx); err != nil {
		return err
	}
	return classifyCommitError(tx.Commit(ctx))
}

// ErrCommitOutcomeUnknown 标记事务提交结果不确定(网络中断/超时/连接关闭等
// 非 rollback 提交错误)。与 pgx.ErrTxCommitRollback(确定回滚)相对:
// 只有确定回滚时才允许调用方清理已物化但未被 DB 引用的外部文件;
// 结果不确定时必须保留文件并交给对账流程, 防止误删已生效的恢复点
// (round11 审查 C2)。
var ErrCommitOutcomeUnknown = errors.New("transaction commit outcome unknown")

// classifyCommitError 把 tx.Commit 的错误分为"确定回滚"与"结果不确定"两类。
// 依据 pgx v5 Commit 文档: commit 处于 rollback 状态时返回
// ErrTxCommitRollback(确定回滚); 其余错误(网络、超时、连接已关闭)无法
// 证明事务未提交, 一律归为不确定。
func classifyCommitError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, pgx.ErrTxCommitRollback) {
		return err
	}
	return fmt.Errorf("%w: %v", ErrCommitOutcomeUnknown, err)
}
