package workermanager

import (
	"context"
	"fmt"
	"strings"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	managerv1 "github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/gen/worker/manager/v1"
)

// Server implements the worker-manager gRPC service.
type Server struct {
	managerv1.UnimplementedWorkerManagerServiceServer
	runtime *Runtime
}

// NewServer wraps a Podman runtime.
func NewServer(runtime *Runtime) (*Server, error) {
	if runtime == nil {
		return nil, fmt.Errorf("runtime is required")
	}
	return &Server{runtime: runtime}, nil
}

// AllocateWorker starts a container for the session and returns the socket path.
func (s *Server) AllocateWorker(ctx context.Context, req *managerv1.AllocateWorkerRequest) (*managerv1.AllocateWorkerResponse, error) {
	if err := validateAllocateRequest(req); err != nil {
		return nil, err
	}
	c, err := s.runtime.Allocate(ctx, req.GetSessionKey(), req.GetConfigRootPath(), req.GetRuntimeRootPath())
	if err != nil {
		return nil, fmt.Errorf("allocate container: %w", err)
	}
	return &managerv1.AllocateWorkerResponse{
		WorkerInstanceId: c.ID,
		DialAddress:      dialAddress(c.SocketPath),
	}, nil
}

func validateAllocateRequest(req *managerv1.AllocateWorkerRequest) error {
	if req == nil {
		return fmt.Errorf("request is nil")
	}
	if strings.TrimSpace(req.GetSessionKey()) == "" {
		return fmt.Errorf("session_key is required")
	}
	if strings.TrimSpace(req.GetConfigRootPath()) == "" {
		return fmt.Errorf("config_root_path is required")
	}
	if strings.TrimSpace(req.GetRuntimeRootPath()) == "" {
		return fmt.Errorf("runtime_root_path is required")
	}
	return nil
}

func dialAddress(socketPath string) string {
	return "unix:" + socketPath
}

// ReleaseWorker stops the container identified by the request.
func (s *Server) ReleaseWorker(ctx context.Context, req *managerv1.ReleaseWorkerRequest) (*managerv1.ReleaseWorkerResponse, error) {
	if req == nil || strings.TrimSpace(req.GetWorkerInstanceId()) == "" {
		return nil, fmt.Errorf("worker_instance_id is required")
	}
	if err := s.runtime.Release(ctx, req.GetWorkerInstanceId()); err != nil {
		return nil, err
	}
	return &managerv1.ReleaseWorkerResponse{Released: true}, nil
}

// ListWorkers returns the active containers.
func (s *Server) ListWorkers(_ context.Context, _ *managerv1.ListWorkersRequest) (*managerv1.ListWorkersResponse, error) {
	containers := s.runtime.List()
	workers := make([]*managerv1.WorkerInstance, 0, len(containers))
	for _, c := range containers {
		workers = append(workers, &managerv1.WorkerInstance{
			WorkerInstanceId: c.ID,
			SessionKey:       c.SessionKey,
			DialAddress:      dialAddress(c.SocketPath),
			CreatedAt:        timestamppb.New(c.CreatedAt),
		})
	}
	return &managerv1.ListWorkersResponse{Workers: workers}, nil
}

// Health reports whether the manager is reachable.
func (s *Server) Health(_ context.Context, _ *managerv1.HealthRequest) (*managerv1.HealthResponse, error) {
	return &managerv1.HealthResponse{Healthy: true}, nil
}

// Ensure *Server implements WorkerManagerServiceServer.
var _ managerv1.WorkerManagerServiceServer = (*Server)(nil)

const allocateWaitTimeout = 60 * time.Second
