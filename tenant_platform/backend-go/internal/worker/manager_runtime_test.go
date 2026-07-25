package worker

import (
	"context"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc"

	managerv1 "github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/gen/worker/manager/v1"
	workerv1 "github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/gen/worker/v1"
)

type testManagerServer struct {
	managerv1.UnimplementedWorkerManagerServiceServer
	workerAddr   string
	allocateCall atomic.Bool
	releaseCall  atomic.Bool
}

func (s *testManagerServer) AllocateWorker(_ context.Context, req *managerv1.AllocateWorkerRequest) (*managerv1.AllocateWorkerResponse, error) {
	s.allocateCall.Store(true)
	return &managerv1.AllocateWorkerResponse{
		WorkerInstanceId: "test-instance-1",
		DialAddress:      s.workerAddr,
	}, nil
}

func (s *testManagerServer) ReleaseWorker(_ context.Context, _ *managerv1.ReleaseWorkerRequest) (*managerv1.ReleaseWorkerResponse, error) {
	s.releaseCall.Store(true)
	return &managerv1.ReleaseWorkerResponse{Released: true}, nil
}

type testWorkerServer struct {
	workerv1.UnimplementedWorkerServiceServer
	shutdownCall atomic.Bool
}

func (s *testWorkerServer) Health(_ context.Context, _ *workerv1.HealthRequest) (*workerv1.HealthResponse, error) {
	return &workerv1.HealthResponse{Ready: true}, nil
}

func (s *testWorkerServer) Shutdown(_ context.Context, _ *workerv1.ShutdownRequest) (*workerv1.ShutdownResponse, error) {
	s.shutdownCall.Store(true)
	return &workerv1.ShutdownResponse{}, nil
}

func startTestManager(t *testing.T, mgr *testManagerServer) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen manager: %v", err)
	}
	srv := grpc.NewServer()
	managerv1.RegisterWorkerManagerServiceServer(srv, mgr)
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { srv.Stop(); _ = ln.Close() })
	return ln.Addr().String()
}

func startTestWorker(t *testing.T, worker *testWorkerServer) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen worker: %v", err)
	}
	srv := grpc.NewServer()
	workerv1.RegisterWorkerServiceServer(srv, worker)
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { srv.Stop(); _ = ln.Close() })
	return ln.Addr().String()
}

func TestNewManagerRejectsEmptyAddr(t *testing.T) {
	_, err := NewManager(ManagerConfig{})
	if err == nil {
		t.Fatal("expected error for empty manager address")
	}
}

func TestManagerRuntimeStartAndCleanup(t *testing.T) {
	workerSrv := &testWorkerServer{}
	workerAddr := startTestWorker(t, workerSrv)
	managerSrv := &testManagerServer{workerAddr: workerAddr}
	managerAddr := startTestManager(t, managerSrv)

	runtime, err := NewManager(ManagerConfig{ManagerAddr: managerAddr})
	if err != nil {
		t.Fatalf("new manager runtime: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	inst, err := runtime.Start(ctx, StartRequest{
		SessionKey: "session-1",
		ConfigDir:  "/tmp/config",
		RuntimeDir: "/tmp/runtime",
	})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if inst.InstID != "test-instance-1" {
		t.Fatalf("unexpected instance id: %s", inst.InstID)
	}
	if inst.Client == nil {
		t.Fatal("expected non-nil client")
	}
	if !managerSrv.allocateCall.Load() {
		t.Fatal("expected AllocateWorker to be called")
	}

	healthCtx, healthCancel := context.WithTimeout(ctx, 2*time.Second)
	defer healthCancel()
	resp, err := inst.Client.Health(healthCtx)
	if err != nil {
		t.Fatalf("health: %v", err)
	}
	if !resp.GetReady() {
		t.Fatal("expected worker to be ready")
	}

	inst.Cleanup()

	if !workerSrv.shutdownCall.Load() {
		t.Fatal("expected Shutdown to be called on worker")
	}
	if !managerSrv.releaseCall.Load() {
		t.Fatal("expected ReleaseWorker to be called on manager")
	}
}
