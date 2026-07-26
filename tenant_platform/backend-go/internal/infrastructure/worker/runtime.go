package worker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/infrastructure/workerclient"
)

// Instance is a started Worker process or container together with its gRPC
// client and cleanup function.
type Instance struct {
	Client  workerclient.WorkerClient
	InstID  string
	Cleanup func()
}

// StartRequest carries the session-scoped resources needed to start a Worker.
type StartRequest struct {
	SessionKey string
	ConfigDir  string // GA_CONFIG_ROOT (may be session-scoped)
	RuntimeDir string // GA_RUNTIME_DIR parent or session-scoped dir
}

// WorkerRuntime abstracts how the platform creates a Worker for a session.
type WorkerRuntime interface {
	Start(ctx context.Context, req StartRequest) (*Instance, error)
}

// LoopbackConfig carries the host paths needed to launch the Python Worker as a
// local subprocess. This is the development/Windows fallback runtime.
type LoopbackConfig struct {
	Python     string
	WorkerSrc  string
	LegacyRoot string
	PolicyFile string
}

// LoopbackWorkerRuntime starts a real Python Worker subprocess on the host and
// dials its loopback gRPC port. It is NOT a production isolation boundary.
type LoopbackWorkerRuntime struct {
	cfg LoopbackConfig
}

// NewLoopback validates config and returns a loopback runtime.
func NewLoopback(cfg LoopbackConfig) (*LoopbackWorkerRuntime, error) {
	if strings.TrimSpace(cfg.LegacyRoot) == "" {
		return nil, fmt.Errorf("LoopbackConfig.LegacyRoot is required")
	}
	return &LoopbackWorkerRuntime{cfg: cfg}, nil
}

// Start launches a Python Worker subprocess bound to a loopback TCP port.
func (r *LoopbackWorkerRuntime) Start(ctx context.Context, req StartRequest) (*Instance, error) {
	python := r.cfg.Python
	if python == "" {
		python = defaultPython(r.cfg.LegacyRoot)
	}
	workerSrc := r.cfg.WorkerSrc
	if workerSrc == "" {
		workerSrc = filepath.Join(r.cfg.LegacyRoot, "tenant_platform", "worker-python", "src")
	}
	listen := "127.0.0.1:0"
	cmd := exec.CommandContext(ctx, python, "-m", "ga_worker.entrypoint", "--listen", listen, "--grace-seconds", "5")
	configureWorkerProcess(cmd)
	if base := filepath.Base(workerSrc); base == "src" {
		cmd.Dir = filepath.Dir(workerSrc)
	} else {
		cmd.Dir = workerSrc
	}
	env := os.Environ()
	env = setEnv(env, "GA_CONFIG_ROOT", req.ConfigDir)
	env = setEnv(env, "GA_LEGACY_ROOT", r.cfg.LegacyRoot)
	env = setEnv(env, "GA_RUNTIME_DIR", req.RuntimeDir)
	env = setEnv(env, "GA_WORKER_LISTEN", listen)
	if r.cfg.PolicyFile != "" {
		env = setEnv(env, "GA_POLICY_FILE", r.cfg.PolicyFile)
	}
	env = unsetEnv(env, "OPENAI_API_KEY")
	env = unsetEnv(env, "ANTHROPIC_API_KEY")
	pp := workerSrc
	if existing := getEnv(env, "PYTHONPATH"); existing != "" {
		pp = workerSrc + string(os.PathListSeparator) + existing
	}
	env = setEnv(env, "PYTHONPATH", pp)
	cmd.Env = env
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	cmd.Stderr = cmd.Stdout
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start python worker: %w", err)
	}
	listenAddr, err := waitWorkerListen(stdout, workerStartTimeout)
	if err != nil {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
		rest, _ := io.ReadAll(stdout)
		return nil, fmt.Errorf("%w\nworker output:\n%s", err, string(rest))
	}
	go func() { _, _ = io.Copy(io.Discard, stdout) }()
	if !isLoopbackAddr(listenAddr) {
		_ = cmd.Process.Kill()
		return nil, fmt.Errorf("worker not loopback: %s", listenAddr)
	}
	conn, err := grpc.DialContext(ctx, listenAddr, grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithBlock())
	if err != nil {
		_ = cmd.Process.Kill()
		return nil, fmt.Errorf("dial worker %s: %w", listenAddr, err)
	}
	client, err := workerclient.New(conn)
	if err != nil {
		_ = conn.Close()
		_ = cmd.Process.Kill()
		return nil, err
	}
	instID := "loopback-" + listenAddr
	cleanup := func() {
		processCleaner{
			client:      client,
			closeConn:   conn.Close,
			killProcess: cmd.Process.Kill,
			waitProcess: func() error {
				_, err := cmd.Process.Wait()
				return err
			},
		}.run(workerShutdownTimeout)
	}
	return &Instance{Client: client, InstID: instID, Cleanup: cleanup}, nil
}

// StaticRuntime returns a fixed Worker client for unit tests. It is NOT a
// production runtime.
type StaticRuntime struct {
	dial func(ctx context.Context, sessionKey string) (workerclient.WorkerClient, func(), error)
}

// NewStaticRuntime builds a runtime that always returns the supplied client.
// The dial function matches the legacy SchedulerConfig.DialWorker signature so
// existing tests can migrate without change.
func NewStaticRuntime(dial func(ctx context.Context, sessionKey string) (workerclient.WorkerClient, func(), error)) *StaticRuntime {
	return &StaticRuntime{dial: dial}
}

func (s *StaticRuntime) Start(ctx context.Context, req StartRequest) (*Instance, error) {
	client, cleanup, err := s.dial(ctx, req.SessionKey)
	if err != nil {
		return nil, err
	}
	return &Instance{Client: client, InstID: "static-test-worker", Cleanup: cleanup}, nil
}

// --- helpers moved from scheduler.go ---

const workerShutdownTimeout = 5 * time.Second

var workerListenRE = regexp.MustCompile(`WORKER_LISTEN=(\S+)`)

const workerStartTimeout = 30 * time.Second

type processCleaner struct {
	client      workerclient.WorkerClient
	closeConn   func() error
	killProcess func() error
	waitProcess func() error
}

func (c processCleaner) run(timeout time.Duration) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	_ = c.client.Shutdown(ctx, "scheduler-stop")
	cancel()
	_ = c.closeConn()
	_ = c.killProcess()
	_ = c.waitProcess()
}

func waitWorkerListen(r io.Reader, timeout time.Duration) (string, error) {
	deadline := time.Now().Add(timeout)
	buf := make([]byte, 0, 4096)
	tmp := make([]byte, 512)
	for time.Now().Before(deadline) {
		n, err := r.Read(tmp)
		if n > 0 {
			buf = append(buf, tmp[:n]...)
			if m := workerListenRE.FindSubmatch(buf); m != nil {
				return string(m[1]), nil
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return "", fmt.Errorf("worker exited before WORKER_LISTEN; output:\n%s", string(buf))
			}
			return "", err
		}
	}
	return "", fmt.Errorf("timeout waiting for WORKER_LISTEN; output:\n%s", string(buf))
}

func isLoopbackAddr(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func defaultPython(legacyRoot string) string {
	candidates := []string{
		filepath.Join(legacyRoot, ".venv", "Scripts", "python.exe"),
		filepath.Join(legacyRoot, ".venv", "bin", "python"),
		"python3",
		"python",
	}
	for _, c := range candidates {
		if c == "python3" || c == "python" {
			return c
		}
		if st, err := os.Stat(c); err == nil && !st.IsDir() {
			return c
		}
	}
	return "python"
}

func setEnv(env []string, key, value string) []string {
	prefix := key + "="
	out := env[:0]
	for _, e := range env {
		if !strings.HasPrefix(e, prefix) {
			out = append(out, e)
		}
	}
	return append(out, prefix+value)
}

func unsetEnv(env []string, key string) []string {
	prefix := key + "="
	out := env[:0]
	for _, e := range env {
		if !strings.HasPrefix(e, prefix) {
			out = append(out, e)
		}
	}
	return out
}

func getEnv(env []string, key string) string {
	prefix := key + "="
	for _, e := range env {
		if strings.HasPrefix(e, prefix) {
			return strings.TrimPrefix(e, prefix)
		}
	}
	return ""
}
