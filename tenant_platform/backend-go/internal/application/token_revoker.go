package application

import (
	"context"
	"fmt"
	"time"
)

const revokeTimeout = 5 * time.Second

type TokenRevoker interface {
	Revoke(ctx context.Context, jti string) error
}

type CapabilityRevocationWriter interface {
	RevokeCapability(ctx context.Context, jti string, expiresAt time.Time) error
}

type persistentTokenRevoker struct {
	store     CapabilityRevocationWriter
	retention time.Duration
	clock     func() time.Time
}

func NewPersistentTokenRevoker(store CapabilityRevocationWriter, retention time.Duration) (TokenRevoker, error) {
	if store == nil {
		return nil, fmt.Errorf("capability revocation store is required")
	}
	if retention <= 0 {
		return nil, fmt.Errorf("capability revocation retention must be positive")
	}
	return &persistentTokenRevoker{store: store, retention: retention, clock: time.Now}, nil
}

func (r *persistentTokenRevoker) Revoke(ctx context.Context, jti string) error {
	if jti == "" {
		return nil
	}
	expiresAt := r.clock().UTC().Add(r.retention)
	if err := r.store.RevokeCapability(ctx, jti, expiresAt); err != nil {
		return fmt.Errorf("persist capability revocation: %w", err)
	}
	return nil
}
