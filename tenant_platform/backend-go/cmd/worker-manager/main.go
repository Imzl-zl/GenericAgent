// Command worker-manager owns rootless Podman container lifecycle for Workers.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"os"
	"os/signal"
	"syscall"

	"google.golang.org/grpc"

	managerv1 "github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/gen/worker/manager/v1"
	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/workermanager"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "worker-manager: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	addr := flag.String("listen", "127.0.0.1:50051", "gRPC listen address")
	image := flag.String("image", "ga-worker:latest", "Worker container image")
	podman := flag.String("podman", "podman", "Podman binary path")
	flag.Parse()

	if *image == "" {
		return errors.New("--image is required")
	}

	runtime, err := workermanager.NewRuntime(workermanager.RuntimeConfig{
		Image:    *image,
		Executor: &workermanager.CLIExecutor{Binary: *podman},
	})
	if err != nil {
		return fmt.Errorf("runtime: %w", err)
	}

	server, err := workermanager.NewServer(runtime)
	if err != nil {
		return fmt.Errorf("server: %w", err)
	}

	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		return fmt.Errorf("listen %s: %w", *addr, err)
	}

	grpcServer := grpc.NewServer()
	managerv1.RegisterWorkerManagerServiceServer(grpcServer, server)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() { errCh <- grpcServer.Serve(ln) }()

	fmt.Fprintf(os.Stderr, "worker-manager: listening on %s\n", ln.Addr().String())
	select {
	case <-ctx.Done():
		grpcServer.GracefulStop()
		return nil
	case err := <-errCh:
		return err
	}
}
