package worker

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	workerv1 "github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/gen/worker/v1"
	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/infrastructure/workerclient"
)

type blockedShutdownClient struct{}

func (blockedShutdownClient) StartSession(context.Context, *workerv1.StartSessionRequest) (*workerv1.StartSessionResponse, error) {
	return nil, nil
}

func (blockedShutdownClient) ReloadCredentials(context.Context, *workerv1.ReloadCredentialsRequest) (*workerv1.ReloadCredentialsResponse, error) {
	return nil, nil
}

func (blockedShutdownClient) ExecuteTask(context.Context, *workerv1.ExecuteTaskRequest) (<-chan workerclient.WorkerEvent, <-chan error) {
	return nil, nil
}

func (blockedShutdownClient) BeginCheckpoint(context.Context, *workerv1.BeginCheckpointRequest) (*workerv1.CheckpointReady, error) {
	return nil, nil
}

func (blockedShutdownClient) CancelTask(context.Context, string) error { return nil }

func (blockedShutdownClient) Health(context.Context) (*workerv1.HealthResponse, error) {
	return nil, nil
}

func (blockedShutdownClient) Shutdown(ctx context.Context, _ string) error {
	<-ctx.Done()
	return ctx.Err()
}

func TestProcessCleanerBoundsShutdownAndContinuesCleanup(t *testing.T) {
	worker := blockedShutdownClient{}
	var closeCalled, killCalled, waitCalled atomic.Bool
	cleanup := processCleaner{
		client:      worker,
		closeConn:   func() error { closeCalled.Store(true); return nil },
		killProcess: func() error { killCalled.Store(true); return nil },
		waitProcess: func() error { waitCalled.Store(true); return nil },
	}
	started := time.Now()
	cleanup.run(50 * time.Millisecond)
	if elapsed := time.Since(started); elapsed >= 500*time.Millisecond {
		t.Fatalf("cleanup exceeded outer budget: %s", elapsed)
	}
	if !closeCalled.Load() || !killCalled.Load() || !waitCalled.Load() {
		t.Fatalf("cleanup did not continue: close=%v kill=%v wait=%v", closeCalled.Load(), killCalled.Load(), waitCalled.Load())
	}
}
