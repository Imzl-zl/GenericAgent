package workermanager

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// Executor abstracts the container CLI. Tests inject a fake that returns
// scripted stdout without calling Podman.
type Executor interface {
	Run(ctx context.Context, args []string) (stdout []byte, err error)
}

// CLIExecutor shells out to a podman-compatible binary.
type CLIExecutor struct {
	Binary string
}

// Run executes the binary with the supplied arguments.
func (e *CLIExecutor) Run(ctx context.Context, args []string) (stdout []byte, err error) {
	cmd := exec.CommandContext(ctx, e.Binary, args...)
	return cmd.Output()
}

// Container holds the runtime state of one session Worker.
type Container struct {
	ID         string
	SessionKey string
	SocketPath string
	CreatedAt  time.Time
}

// RuntimeConfig carries Podman-specific settings.
type RuntimeConfig struct {
	Image    string
	Executor Executor
}

// Runtime owns container allocation and release for the worker-manager.
type Runtime struct {
	cfg        RuntimeConfig
	containers map[string]*Container
}

// NewRuntime validates config and returns a Podman runtime.
func NewRuntime(cfg RuntimeConfig) (*Runtime, error) {
	if err := validateRuntimeConfig(cfg); err != nil {
		return nil, err
	}
	return &Runtime{
		cfg:        cfg,
		containers: make(map[string]*Container),
	}, nil
}

func validateRuntimeConfig(cfg RuntimeConfig) error {
	if cfg.Image == "" {
		return fmt.Errorf("RuntimeConfig.Image is required")
	}
	if cfg.Executor == nil {
		return fmt.Errorf("RuntimeConfig.Executor is required")
	}
	return nil
}

// Allocate runs a new container for the session and returns its metadata.
func (r *Runtime) Allocate(ctx context.Context, sessionKey, configRoot, runtimeRoot string) (*Container, error) {
	if sessionKey == "" {
		return nil, fmt.Errorf("session key is required")
	}
	if err := prepareSessionDirs(runtimeRoot, sessionKey); err != nil {
		return nil, err
	}
	containerID, err := r.runContainer(ctx, sessionKey, configRoot, runtimeRoot)
	if err != nil {
		return nil, err
	}
	c := &Container{
		ID:         strings.TrimSpace(containerID),
		SessionKey: sessionKey,
		SocketPath: socketPath(runtimeRoot, sessionKey),
		CreatedAt:  time.Now(),
	}
	r.containers[c.ID] = c
	return c, nil
}

// Release stops and removes the container identified by id.
func (r *Runtime) Release(ctx context.Context, id string) error {
	if id == "" {
		return fmt.Errorf("container id is required")
	}
	_, err := r.cfg.Executor.Run(ctx, []string{"stop", "-t", "5", id})
	if err != nil {
		return fmt.Errorf("stop container %s: %w", id, err)
	}
	_, _ = r.cfg.Executor.Run(ctx, []string{"rm", id})
	delete(r.containers, id)
	return nil
}

// List returns a snapshot of active containers.
func (r *Runtime) List() []*Container {
	out := make([]*Container, 0, len(r.containers))
	for _, c := range r.containers {
		out = append(out, c)
	}
	return out
}

func prepareSessionDirs(runtimeRoot, sessionKey string) error {
	dirs := []string{
		runtimeDir(runtimeRoot, sessionKey),
		socketDir(runtimeRoot, sessionKey),
	}
	for _, d := range dirs {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return fmt.Errorf("mkdir %s: %w", d, err)
		}
	}
	return nil
}

func (r *Runtime) runContainer(ctx context.Context, sessionKey, configRoot, runtimeRoot string) (string, error) {
	args := r.containerArgs(sessionKey, configRoot, runtimeRoot)
	out, err := r.cfg.Executor.Run(ctx, args)
	if err != nil {
		return "", fmt.Errorf("podman run: %w (output: %s)", err, string(out))
	}
	return string(out), nil
}

func (r *Runtime) containerArgs(sessionKey, configRoot, runtimeRoot string) []string {
	name := containerName(sessionKey)
	return []string{
		"run", "-d", "--rm",
		"--name", name,
		"-v", configRoot + ":/ga/config:ro",
		"-v", runtimeDir(runtimeRoot, sessionKey) + ":/ga/runtime:rw",
		"-v", socketDir(runtimeRoot, sessionKey) + ":/ga/rpc:rw",
		"-e", "GA_CONFIG_ROOT=/ga/config",
		"-e", "GA_LEGACY_ROOT=/ga/legacy",
		"-e", "GA_RUNTIME_DIR=/ga/runtime",
		"-e", "GA_WORKER_LISTEN=unix:/ga/rpc/worker.sock",
		r.cfg.Image,
		"python", "-m", "ga_worker.entrypoint",
		"--listen", "unix:/ga/rpc/worker.sock",
	}
}

func containerName(sessionKey string) string {
	return "ga-worker-" + sessionKey
}

func runtimeDir(runtimeRoot, sessionKey string) string {
	return sessionDir(runtimeRoot, sessionKey) + "/runtime"
}

func socketDir(runtimeRoot, sessionKey string) string {
	return sessionDir(runtimeRoot, sessionKey) + "/rpc"
}

func socketPath(runtimeRoot, sessionKey string) string {
	return runtimeRoot + "/" + sessionKey + "/rpc/worker.sock"
}

func sessionDir(runtimeRoot, sessionKey string) string {
	return runtimeRoot + "/" + sessionKey
}
