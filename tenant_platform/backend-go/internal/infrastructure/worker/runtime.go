package worker

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
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
	// RunnerGeneration 是 Runner lease generation(方案 §7 fencing)。
	// loopback 路径恒为 1; Sandbox 路径来自持久 lease。
	RunnerGeneration uint64
}

// StartRequest carries the session-scoped resources needed to start a Worker.
type StartRequest struct {
	SessionKey string
	ConfigDir  string // GA_CONFIG_ROOT (may be session-scoped)
	RuntimeDir string // GA_RUNTIME_DIR parent or session-scoped dir
	// RuntimeConfigFiles 是随控制面材料一并注入容器 config/ 的运行时配置
	// (mykey.runtime.json 等)。Loopback 路径已直接写 ConfigDir, 忽略该字段;
	// Sandbox 路径经共享卷 config/ 挂载注入(方案 §7)。
	RuntimeConfigFiles map[string][]byte
}

// WorkerRuntime abstracts how the platform creates a Worker for a session.
type WorkerRuntime interface {
	Start(ctx context.Context, req StartRequest) (*Instance, error)
	// ResolveGeneration 返回会话工作区的 Runner lease generation(方案 §7
	// fencing): Sandbox 路径来自持久 lease, Loopback 恒为 1。签发 per-task
	// capability 与构造 StartSession 前需要该值。
	ResolveGeneration(ctx context.Context, sessionKey string) (uint64, error)
	// ReleaseRunnerLease 释放会话工作区的 Runner lease(审查: 初始化失败
	// 时归还容量)。Sandbox 路径委托持久 lease store; Loopback/Static 无
	// 持久 lease, 返回 nil。generation 条件防止释放新 generation。
	ReleaseRunnerLease(ctx context.Context, sessionKey string, generation uint64) error
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

// ResolveGeneration returns 1: loopback has no persistent Runner lease.
func (r *LoopbackWorkerRuntime) ResolveGeneration(context.Context, string) (uint64, error) {
	return 1, nil
}

// ReleaseRunnerLease is a no-op for loopback (no persistent lease).
func (r *LoopbackWorkerRuntime) ReleaseRunnerLease(context.Context, string, uint64) error {
	return nil
}

var _ WorkerRuntime = (*LoopbackWorkerRuntime)(nil)

// NewLoopback validates config and returns a loopback runtime.
func NewLoopback(cfg LoopbackConfig) (*LoopbackWorkerRuntime, error) {
	if strings.TrimSpace(cfg.LegacyRoot) == "" {
		return nil, fmt.Errorf("LoopbackConfig.LegacyRoot is required")
	}
	return &LoopbackWorkerRuntime{cfg: cfg}, nil
}

// Start launches a Python Worker subprocess bound to a loopback TCP port.
// RuntimeConfigFiles 在 loopback 路径下已由 scheduler 直接写入 ConfigDir,
// 此处忽略(接口兼容)。
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
	// Instance.Cleanup owns the resident Worker lifetime. The caller context is
	// a per-dispatch heartbeat context and is still used below for startup dial.
	cmd := exec.Command(python, "-m", "ga_worker.entrypoint", "--listen", listen, "--grace-seconds", "5")
	configureWorkerProcess(cmd)
	if base := filepath.Base(workerSrc); base == "src" {
		cmd.Dir = filepath.Dir(workerSrc)
	} else {
		cmd.Dir = workerSrc
	}
	cmd.Env = buildWorkerEnvironment(os.Environ(), r.cfg, req, workerSrc, listen)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	cmd.Stderr = cmd.Stdout
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start python worker: %w", err)
	}
	listenAddr, err := WaitWorkerListen(stdout, workerStartTimeout)
	if err != nil {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
		return nil, err
	}
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
			client:          client,
			closeConn:       conn.Close,
			killProcess:     cmd.Process.Kill,
			waitProcess:     func() error {
				_, err := cmd.Process.Wait()
				return err
			},
			workspaceKey:     req.SessionKey,
			runnerGeneration: 1,
		}.run(workerShutdownTimeout)
	}
	return &Instance{Client: client, InstID: instID, Cleanup: cleanup, RunnerGeneration: 1}, nil
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
	return &Instance{Client: client, InstID: "static-test-worker", Cleanup: cleanup, RunnerGeneration: 1}, nil
}

// ResolveGeneration returns 1: static runtime has no persistent lease.
func (s *StaticRuntime) ResolveGeneration(context.Context, string) (uint64, error) {
	return 1, nil
}

// ReleaseRunnerLease is a no-op for the static test runtime.
func (s *StaticRuntime) ReleaseRunnerLease(context.Context, string, uint64) error {
	return nil
}

// --- helpers moved from scheduler.go ---

const workerShutdownTimeout = 5 * time.Second

const workerStartTimeout = 30 * time.Second

type processCleaner struct {
	client           workerclient.WorkerClient
	closeConn        func() error
	killProcess       func() error
	waitProcess       func() error
	workspaceKey      string
	runnerGeneration  uint64
}

func (c processCleaner) run(timeout time.Duration) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	_ = c.client.Shutdown(ctx, c.workspaceKey, "scheduler-stop", c.runnerGeneration)
	cancel()
	_ = c.closeConn()
	_ = c.killProcess()
	_ = c.waitProcess()
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

var workerInheritedEnvironmentAllowlist = [...]string{
	"APPDATA",
	"COMSPEC",
	"GA_LANG",
	"HOME",
	"HOMEDRIVE",
	"HOMEPATH",
	"LANG",
	"LC_ALL",
	"LC_CTYPE",
	"LOCALAPPDATA",
	"PATH",
	"PATHEXT",
	"PYTHONIOENCODING",
	"PYTHONUTF8",
	"REQUESTS_CA_BUNDLE",
	"SSL_CERT_DIR",
	"SSL_CERT_FILE",
	"SYSTEMROOT",
	"TEMP",
	"TMP",
	"TZ",
	"USERPROFILE",
	"WINDIR",
}

func buildWorkerEnvironment(
	inherited []string,
	cfg LoopbackConfig,
	req StartRequest,
	workerSrc string,
	listen string,
) []string {
	env := make([]string, 0, len(workerInheritedEnvironmentAllowlist)+6)
	for _, key := range workerInheritedEnvironmentAllowlist {
		if value, present := lookupEnv(inherited, key); present {
			env = append(env, key+"="+value)
		}
	}
	env = setEnv(env, "GA_CONFIG_ROOT", req.ConfigDir)
	env = setEnv(env, "GA_LEGACY_ROOT", cfg.LegacyRoot)
	env = setEnv(env, "GA_RUNTIME_DIR", req.RuntimeDir)
	env = setEnv(env, "GA_WORKER_LISTEN", listen)
	if cfg.PolicyFile != "" {
		env = setEnv(env, "GA_POLICY_FILE", cfg.PolicyFile)
	}
	return setEnv(env, "PYTHONPATH", workerSrc)
}

func lookupEnv(env []string, key string) (string, bool) {
	for _, entry := range env {
		name, value, found := strings.Cut(entry, "=")
		if found && strings.EqualFold(name, key) {
			return value, true
		}
	}
	return "", false
}

func setEnv(env []string, key, value string) []string {
	out := env[:0]
	for _, entry := range env {
		name, _, found := strings.Cut(entry, "=")
		if !found || !strings.EqualFold(name, key) {
			out = append(out, entry)
		}
	}
	return append(out, key+"="+value)
}

func unsetEnv(env []string, key string) []string {
	out := env[:0]
	for _, entry := range env {
		name, _, found := strings.Cut(entry, "=")
		if !found || !strings.EqualFold(name, key) {
			out = append(out, entry)
		}
	}
	return out
}

func getEnv(env []string, key string) string {
	value, _ := lookupEnv(env, key)
	return value
}
