// Package postgres implements the platform task store against PostgreSQL.
package postgres

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Application-enforced limits (not volatile DB now() checks).
const (
	MaxPromptBytes          = 64 * 1024
	MaxPersonaBytes         = 16 * 1024
	MaxTerminalErrorBytes   = 4 * 1024
	MaxToolPolicyVersionLen = 128
	MaxSourceLen            = 64
	MaxSourceInstanceLen    = 128
	MaxMessageIDLen         = 256
)

// DevelopmentContext is the approved loopback user/workspace pair.
type DevelopmentContext struct {
	UserID      int64
	Username    string
	WorkspaceID string
	SessionKey  string
}

// Store is the PostgreSQL-backed task store.
type Store struct {
	pool                            *pgxpool.Pool
	perUserQueueLimit               int // 0 = disabled (dev/test); enforced inside SubmitTask tx
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
	return tx.Commit(ctx)
}
