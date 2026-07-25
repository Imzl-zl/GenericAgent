package worker

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	managerv1 "github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/gen/worker/manager/v1"
	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/workerclient"
)

const (
	managerAllocateTimeout = 60 * time.Second
	managerReleaseTimeout  = 30 * time.Second
	workerDialTimeout      = 30 * time.Second
)

// ManagerConfig carries the gRPC client settings for the worker-manager.
type ManagerConfig struct {
	// ManagerAddr is the gRPC dial target for worker-manager, e.g. "127.0.0.1:50051".
	ManagerAddr string
	// DialOpts are appended to the default insecure options when dialing.
	DialOpts []grpc.DialOption
	// NewManagerClient is injectable for tests; nil uses the generated client.
	NewManagerClient func(cc grpc.ClientConnInterface) managerv1.WorkerManagerServiceClient
}

// ManagerWorkerRuntime creates Workers by asking a worker-manager process to
// allocate a rootless container, then dials the Worker gRPC socket returned by
// the manager.
type ManagerWorkerRuntime struct {
	cfg ManagerConfig
}

// NewManager validates config and returns a manager-backed runtime.
func NewManager(cfg ManagerConfig) (*ManagerWorkerRuntime, error) {
	if strings.TrimSpace(cfg.ManagerAddr) == "" {
		return nil, errors.New("ManagerConfig.ManagerAddr is required")
	}
	return &ManagerWorkerRuntime{cfg: cfg}, nil
}

// Start allocates a containerized Worker and dials its gRPC socket.
func (r *ManagerWorkerRuntime) Start(ctx context.Context, req StartRequest) (*Instance, error) {
	if strings.TrimSpace(req.SessionKey) == "" {
		return nil, errors.New("StartRequest.SessionKey is required")
	}
	managerConn, managerClient, err := r.dialManager(ctx)
	if err != nil {
		return nil, err
	}
	allocateCtx, cancel := context.WithTimeout(ctx, managerAllocateTimeout)
	defer cancel()
	resp, err := managerClient.AllocateWorker(allocateCtx, &managerv1.AllocateWorkerRequest{
		SessionKey:      req.SessionKey,
		ConfigRootPath:  req.ConfigDir,
		RuntimeRootPath: req.RuntimeDir,
	})
	if err != nil {
		_ = managerConn.Close()
		return nil, fmt.Errorf("allocate worker: %w", err)
	}
	workerConn, client, err := r.dialWorker(ctx, resp.GetDialAddress())
	if err != nil {
		_ = managerConn.Close()
		return nil, err
	}
	cleanup := r.cleanupFunc(managerConn, workerConn, client, resp.GetWorkerInstanceId())
	return &Instance{
		Client: client,
		InstID: resp.GetWorkerInstanceId(),
		Cleanup: cleanup,
	}, nil
}

func (r *ManagerWorkerRuntime) dialManager(ctx context.Context) (*grpc.ClientConn, managerv1.WorkerManagerServiceClient, error) {
	opts := append(r.defaultDialOpts(), r.cfg.DialOpts...)
	conn, err := grpc.DialContext(ctx, r.cfg.ManagerAddr, opts...)
	if err != nil {
		return nil, nil, fmt.Errorf("dial worker-manager %s: %w", r.cfg.ManagerAddr, err)
	}
	newClient := r.cfg.NewManagerClient
	if newClient == nil {
		newClient = managerv1.NewWorkerManagerServiceClient
	}
	return conn, newClient(conn), nil
}

func (r *ManagerWorkerRuntime) defaultDialOpts() []grpc.DialOption {
	return []grpc.DialOption{
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	}
}

func (r *ManagerWorkerRuntime) dialWorker(ctx context.Context, addr string) (*grpc.ClientConn, workerclient.WorkerClient, error) {
	if strings.TrimSpace(addr) == "" {
		return nil, nil, errors.New("worker-manager returned empty dial address")
	}
	dialCtx, cancel := context.WithTimeout(ctx, workerDialTimeout)
	defer cancel()
	conn, err := grpc.DialContext(dialCtx, addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, nil, fmt.Errorf("dial worker %s: %w", addr, err)
	}
	client, err := workerclient.New(conn)
	if err != nil {
		_ = conn.Close()
		return nil, nil, fmt.Errorf("wrap worker client: %w", err)
	}
	return conn, client, nil
}

func (r *ManagerWorkerRuntime) cleanupFunc(
	managerConn *grpc.ClientConn,
	workerConn *grpc.ClientConn,
	client workerclient.WorkerClient,
	instID string,
) func() {
	return func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), workerShutdownTimeout)
		_ = client.Shutdown(cleanupCtx, "scheduler-stop")
		cancel()
		_ = workerConn.Close()
		r.releaseWorker(instID, managerConn)
		_ = managerConn.Close()
	}
}

func (r *ManagerWorkerRuntime) releaseWorker(instID string, managerConn *grpc.ClientConn) {
	if strings.TrimSpace(instID) == "" {
		return
	}
	newClient := r.cfg.NewManagerClient
	if newClient == nil {
		newClient = managerv1.NewWorkerManagerServiceClient
	}
	releaseCtx, cancel := context.WithTimeout(context.Background(), managerReleaseTimeout)
	defer cancel()
	_, _ = newClient(managerConn).ReleaseWorker(releaseCtx, &managerv1.ReleaseWorkerRequest{
		WorkerInstanceId: instID,
	})
}

// Ensure *ManagerWorkerRuntime implements WorkerRuntime.
var _ WorkerRuntime = (*ManagerWorkerRuntime)(nil)
