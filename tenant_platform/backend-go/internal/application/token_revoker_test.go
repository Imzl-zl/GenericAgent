package application

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

type fakeCapabilityRevocationStore struct {
	jti       string
	expiresAt time.Time
	calls     atomic.Int32
	err       error
}

func (f *fakeCapabilityRevocationStore) RevokeCapability(_ context.Context, jti string, expiresAt time.Time) error {
	f.calls.Add(1)
	f.jti = jti
	f.expiresAt = expiresAt
	return f.err
}

func TestPersistentTokenRevokerWritesJTIAndExpiry(t *testing.T) {
	store := &fakeCapabilityRevocationStore{}
	revoker, err := NewPersistentTokenRevoker(store, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	fixedNow := time.Date(2026, 7, 27, 1, 2, 3, 0, time.UTC)
	revoker.(*persistentTokenRevoker).clock = func() time.Time { return fixedNow }
	if err := revoker.Revoke(context.Background(), "jti-abc-123"); err != nil {
		t.Fatal(err)
	}
	if store.jti != "jti-abc-123" {
		t.Fatalf("jti = %q", store.jti)
	}
	if !store.expiresAt.Equal(fixedNow.Add(time.Hour)) {
		t.Fatalf("expires_at = %s", store.expiresAt)
	}
}

func TestPersistentTokenRevokerEmptyJTINoWrite(t *testing.T) {
	store := &fakeCapabilityRevocationStore{}
	revoker, err := NewPersistentTokenRevoker(store, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if err := revoker.Revoke(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	if store.calls.Load() != 0 {
		t.Fatalf("calls = %d, want 0", store.calls.Load())
	}
}

func TestPersistentTokenRevokerPropagatesStoreError(t *testing.T) {
	storeErr := errors.New("database unavailable")
	store := &fakeCapabilityRevocationStore{err: storeErr}
	revoker, err := NewPersistentTokenRevoker(store, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if err := revoker.Revoke(context.Background(), "jti-1"); !errors.Is(err, storeErr) {
		t.Fatalf("err = %v", err)
	}
}

func TestPersistentTokenRevokerRejectsInvalidConfig(t *testing.T) {
	if _, err := NewPersistentTokenRevoker(nil, time.Hour); err == nil {
		t.Fatal("expected nil store error")
	}
	if _, err := NewPersistentTokenRevoker(&fakeCapabilityRevocationStore{}, 0); err == nil {
		t.Fatal("expected non-positive retention error")
	}
}
