package application

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/domain"
)

func runtimeDocumentPoolSettings(version int64, maxActive int) domain.DocumentPoolSettings {
	return domain.DocumentPoolSettings{
		Enabled: true, MaxActive: maxActive, MinReady: 0,
		JobIdleTTLSeconds: 600, ReadyIdleTTLSeconds: 300,
		GlobalQueueLimit: 100, PerTenantQueueLimit: 20, PerTenantActiveLimit: 1,
		JobTimeoutSeconds: 3600, CommandTimeoutSeconds: 300,
		Version: version, UpdatedBy: 1, UpdatedAt: time.Now().UTC(), Reason: "test",
	}
}

func TestDocumentPoolSettingsRuntimeAtomicallyPublishesCompleteSnapshot(t *testing.T) {
	runtime, err := NewDocumentPoolSettingsRuntime(runtimeDocumentPoolSettings(1, 1), 4)
	if err != nil {
		t.Fatal(err)
	}
	next := runtimeDocumentPoolSettings(2, 3)
	if err := runtime.ApplyDocumentPoolSettings(context.Background(), next); err != nil {
		t.Fatal(err)
	}
	if got := runtime.CurrentDocumentPoolSettings(); got.Version != 2 || got.MaxActive != 3 {
		t.Fatalf("current=%+v", got)
	}
}

func TestDocumentPoolSettingsRuntimeDoesNotRegressToOlderVersion(t *testing.T) {
	runtime, err := NewDocumentPoolSettingsRuntime(runtimeDocumentPoolSettings(2, 3), 4)
	if err != nil {
		t.Fatal(err)
	}
	if err := runtime.ApplyDocumentPoolSettings(context.Background(), runtimeDocumentPoolSettings(1, 1)); err != nil {
		t.Fatal(err)
	}
	if got := runtime.CurrentDocumentPoolSettings(); got.Version != 2 || got.MaxActive != 3 {
		t.Fatalf("current regressed: %+v", got)
	}
}

type reconcilingDocumentPoolSource struct {
	settings domain.DocumentPoolSettings
}

func (s reconcilingDocumentPoolSource) GetDocumentPoolSettings(context.Context) (domain.DocumentPoolSettings, error) {
	return s.settings, nil
}

type retryingDocumentPoolRuntime struct {
	mu       sync.Mutex
	attempts int
	applied  domain.DocumentPoolSettings
	done     chan struct{}
}

func (r *retryingDocumentPoolRuntime) ApplyDocumentPoolSettings(_ context.Context, settings domain.DocumentPoolSettings) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.attempts++
	if r.attempts == 1 {
		return errors.New("temporary apply failure")
	}
	r.applied = settings
	select {
	case <-r.done:
	default:
		close(r.done)
	}
	return nil
}

func TestDocumentPoolSettingsReconcilerRetriesPersistedSnapshot(t *testing.T) {
	runtime := &retryingDocumentPoolRuntime{done: make(chan struct{})}
	reconciler, err := NewDocumentPoolSettingsReconciler(
		reconcilingDocumentPoolSource{settings: runtimeDocumentPoolSettings(2, 2)},
		runtime,
		10*time.Millisecond,
	)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = reconciler.Run(ctx) }()
	select {
	case <-runtime.done:
	case <-time.After(time.Second):
		t.Fatal("persisted settings were not retried")
	}
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	if runtime.attempts < 2 || runtime.applied.Version != 2 {
		t.Fatalf("attempts=%d applied=%+v", runtime.attempts, runtime.applied)
	}
}
