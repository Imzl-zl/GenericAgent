package workermanager

import (
	"context"
	"testing"

	managerv1 "github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/gen/worker/manager/v1"
)

func TestServerAllocateAndRelease(t *testing.T) {
	exec := &fakeExecutor{nextID: "container-abc"}
	runtimeRoot := t.TempDir()
	rt, err := NewRuntime(RuntimeConfig{Image: "ga-worker:latest", Executor: exec})
	if err != nil {
		t.Fatalf("new runtime: %v", err)
	}
	srv, err := NewServer(rt)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}

	ctx := context.Background()
	resp, err := srv.AllocateWorker(ctx, &managerv1.AllocateWorkerRequest{
		SessionKey:      "session-1",
		ConfigRootPath:  t.TempDir(),
		RuntimeRootPath: runtimeRoot,
	})
	if err != nil {
		t.Fatalf("allocate: %v", err)
	}
	if resp.GetWorkerInstanceId() != "container-abc" {
		t.Fatalf("unexpected instance id: %s", resp.GetWorkerInstanceId())
	}
	if resp.GetDialAddress() == "" {
		t.Fatal("expected dial address")
	}

	releaseResp, err := srv.ReleaseWorker(ctx, &managerv1.ReleaseWorkerRequest{
		WorkerInstanceId: resp.GetWorkerInstanceId(),
	})
	if err != nil {
		t.Fatalf("release: %v", err)
	}
	if !releaseResp.GetReleased() {
		t.Fatal("expected released=true")
	}
}

func TestServerAllocateRejectsEmptySessionKey(t *testing.T) {
	rt, _ := NewRuntime(RuntimeConfig{Image: "img", Executor: &fakeExecutor{}})
	srv, _ := NewServer(rt)
	_, err := srv.AllocateWorker(context.Background(), &managerv1.AllocateWorkerRequest{
		ConfigRootPath:  t.TempDir(),
		RuntimeRootPath: t.TempDir(),
	})
	if err == nil {
		t.Fatal("expected error for empty session key")
	}
}

func TestServerHealth(t *testing.T) {
	rt, _ := NewRuntime(RuntimeConfig{Image: "img", Executor: &fakeExecutor{}})
	srv, _ := NewServer(rt)
	resp, err := srv.Health(context.Background(), &managerv1.HealthRequest{})
	if err != nil {
		t.Fatalf("health: %v", err)
	}
	if !resp.GetHealthy() {
		t.Fatal("expected healthy")
	}
}

func TestServerListWorkers(t *testing.T) {
	exec := &fakeExecutor{nextID: "container-x"}
	rt, _ := NewRuntime(RuntimeConfig{Image: "img", Executor: exec})
	srv, _ := NewServer(rt)
	ctx := context.Background()
	_, _ = srv.AllocateWorker(ctx, &managerv1.AllocateWorkerRequest{
		SessionKey:      "session-list",
		ConfigRootPath:  t.TempDir(),
		RuntimeRootPath: t.TempDir(),
	})
	listResp, err := srv.ListWorkers(ctx, &managerv1.ListWorkersRequest{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(listResp.GetWorkers()) != 1 {
		t.Fatalf("expected 1 worker, got %d", len(listResp.GetWorkers()))
	}
}

func TestNewServerRejectsNilRuntime(t *testing.T) {
	_, err := NewServer(nil)
	if err == nil {
		t.Fatal("expected error for nil runtime")
	}
}
