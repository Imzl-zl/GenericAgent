package application

import (
	"context"
	"fmt"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/domain"
)

const DefaultDocumentPoolSettingsReconcileInterval = 30 * time.Second

// DocumentPoolSettingsRuntime publishes complete persisted snapshots to runtime
// consumers. Older snapshots cannot overwrite a newer applied version.
type DocumentPoolSettingsRuntime struct {
	deploymentMaxActive int
	current             atomic.Pointer[domain.DocumentPoolSettings]
}

func NewDocumentPoolSettingsRuntime(initial domain.DocumentPoolSettings, deploymentMaxActive int) (*DocumentPoolSettingsRuntime, error) {
	runtime := &DocumentPoolSettingsRuntime{deploymentMaxActive: deploymentMaxActive}
	if err := runtime.ApplyDocumentPoolSettings(context.Background(), initial); err != nil {
		return nil, err
	}
	return runtime, nil
}

func (r *DocumentPoolSettingsRuntime) ApplyDocumentPoolSettings(ctx context.Context, settings domain.DocumentPoolSettings) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if settings.Version <= 0 {
		return fmt.Errorf("document pool settings version must be positive")
	}
	if err := domain.ValidateDocumentPoolSettings(settings, r.deploymentMaxActive); err != nil {
		return err
	}
	if err := domain.ValidateDocumentPoolSettingsReason(settings.Reason); err != nil {
		return err
	}
	next := settings
	for {
		current := r.current.Load()
		if current != nil && current.Version >= next.Version {
			return nil
		}
		if r.current.CompareAndSwap(current, &next) {
			return nil
		}
	}
}

func (r *DocumentPoolSettingsRuntime) CurrentDocumentPoolSettings() domain.DocumentPoolSettings {
	current := r.current.Load()
	if current == nil {
		return domain.DocumentPoolSettings{}
	}
	return *current
}

type DocumentPoolSettingsSource interface {
	GetDocumentPoolSettings(ctx context.Context) (domain.DocumentPoolSettings, error)
}

type DocumentPoolSettingsApplier interface {
	ApplyDocumentPoolSettings(ctx context.Context, settings domain.DocumentPoolSettings) error
}

// DocumentPoolSettingsReconciler retries the database-authoritative snapshot so
// a transient direct-apply failure cannot leave runtime configuration stale.
type DocumentPoolSettingsReconciler struct {
	source   DocumentPoolSettingsSource
	applier  DocumentPoolSettingsApplier
	interval time.Duration
}

func NewDocumentPoolSettingsReconciler(source DocumentPoolSettingsSource, applier DocumentPoolSettingsApplier, interval time.Duration) (*DocumentPoolSettingsReconciler, error) {
	if source == nil || applier == nil {
		return nil, fmt.Errorf("document pool settings source and applier are required")
	}
	if interval <= 0 {
		return nil, fmt.Errorf("document pool settings reconcile interval must be positive")
	}
	return &DocumentPoolSettingsReconciler{source: source, applier: applier, interval: interval}, nil
}

func (r *DocumentPoolSettingsReconciler) reconcile(ctx context.Context) error {
	settings, err := r.source.GetDocumentPoolSettings(ctx)
	if err != nil {
		return fmt.Errorf("load document pool settings: %w", err)
	}
	if err := r.applier.ApplyDocumentPoolSettings(ctx, settings); err != nil {
		return fmt.Errorf("apply document pool settings version %d: %w", settings.Version, err)
	}
	return nil
}

func (r *DocumentPoolSettingsReconciler) Run(ctx context.Context) error {
	ticker := time.NewTicker(r.interval)
	defer ticker.Stop()
	for {
		if err := r.reconcile(ctx); err != nil && ctx.Err() == nil {
			slog.ErrorContext(ctx, "document pool settings reconciliation failed", "error", err)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}
