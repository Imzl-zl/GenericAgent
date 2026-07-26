package workermanager

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
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

// sessionKeyPattern guards against path traversal and container-name
// injection. Allowed chars: alphanumerics, underscore, dot, colon, hyphen.
// Length capped at 128 to bound filesystem paths and container names.
var sessionKeyPattern = regexp.MustCompile(`^[a-zA-Z0-9_.:-]{1,128}$`)

// Runtime owns container allocation and release for the worker-manager.
// All map mutations are guarded by mu; container create/stop are run outside
// the write lock so a slow Podman call cannot block List/Release of others.
type Runtime struct {
	cfg        RuntimeConfig
	mu         sync.RWMutex
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

// validateSessionKey rejects empty or pattern-mismatched keys, preventing
// path traversal (../, /, NUL) and container-name injection.
func validateSessionKey(sessionKey string) error {
	if sessionKey == "" {
		return fmt.Errorf("session key is required")
	}
	if !sessionKeyPattern.MatchString(sessionKey) {
		return fmt.Errorf("session key contains illegal characters or exceeds 128 bytes: %q", sessionKey)
	}
	return nil
}

// safeSessionDir joins root and sessionKey and verifies the result stays
// inside root (defense-in-depth against traversal even after pattern check).
func safeSessionDir(root, sessionKey string) (string, error) {
	cleanRoot := filepath.Clean(root)
	joined := filepath.Join(cleanRoot, sessionKey)
	if !strings.HasPrefix(joined+string(filepath.Separator), cleanRoot+string(filepath.Separator)) {
		return "", fmt.Errorf("session path escapes root: %q", joined)
	}
	return joined, nil
}

// Allocate runs a new container for the session and returns its metadata.
func (r *Runtime) Allocate(ctx context.Context, sessionKey, configRoot, runtimeRoot string) (*Container, error) {
	if err := validateSessionKey(sessionKey); err != nil {
		return nil, err
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
	r.mu.Lock()
	r.containers[c.ID] = c
	r.mu.Unlock()
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
	r.mu.Lock()
	delete(r.containers, id)
	r.mu.Unlock()
	return nil
}

// List returns a snapshot of active containers.
func (r *Runtime) List() []*Container {
	r.mu.RLock()
	out := make([]*Container, 0, len(r.containers))
	for _, c := range r.containers {
		out = append(out, c)
	}
	r.mu.RUnlock()
	return out
}

func prepareSessionDirs(runtimeRoot, sessionKey string) error {
	base, err := safeSessionDir(runtimeRoot, sessionKey)
	if err != nil {
		return err
	}
	for _, sub := range []string{"runtime", "rpc"} {
		d := filepath.Join(base, sub)
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
	base, _ := safeSessionDir(runtimeRoot, sessionKey)
	return []string{
		"run", "-d", "--rm",
		"--name", name,
		// P1 hardening: drop all capabilities, run as non-root, read-only rootfs.
		// "--cap-drop=ALL", "--security-opt=no-new-privileges", "--read-only",
		"-v", filepath.Clean(configRoot) + ":/ga/config:ro",
		"-v", filepath.Join(base, "runtime") + ":/ga/runtime:rw",
		"-v", filepath.Join(base, "rpc") + ":/ga/rpc:rw",
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

func socketPath(runtimeRoot, sessionKey string) string {
	base, _ := safeSessionDir(runtimeRoot, sessionKey)
	return filepath.Join(base, "rpc", "worker.sock")
}
