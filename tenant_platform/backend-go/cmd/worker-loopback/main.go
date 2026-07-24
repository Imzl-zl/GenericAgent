// Command worker-loopback is a development-only harness that launches the real
// Python Worker on loopback, runs one foundation task + BeginCheckpoint + Shutdown,
// and prints a bounded JSON summary.
//
// It is NOT a production Worker process and MUST NOT bind non-loopback addresses.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/workerclient"
	workerv1 "github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/gen/worker/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const (
	testToken            = "test-worker-token-not-a-real-key"
	capabilityVersion    = "foundation.v1"
	toolPolicyVersion    = "foundation.no-host-tools.v1"
	defaultTaskID        = "loopback-task-1"
	defaultSessionKey    = "personal:1"
	workerStartTimeout   = 30 * time.Second
	taskTimeout          = 90 * time.Second
	healthPollInterval   = 200 * time.Millisecond
	fixtureResponseText  = "loopback-fixture-reply"
)

var workerListenRE = regexp.MustCompile(`WORKER_LISTEN=(\S+)`)

type summary struct {
	TaskID           string `json:"task_id"`
	Status           string `json:"status"`
	UserMessage      string `json:"user_message,omitempty"`
	ResultDigest     string `json:"result_digest,omitempty"`
	CheckpointToken  string `json:"checkpoint_token,omitempty"`
	Checksum         string `json:"checksum,omitempty"`
	CheckpointDigest string `json:"checkpoint_result_digest,omitempty"`
	StagingRef       string `json:"staging_ref,omitempty"`
	WorkerInstanceID string `json:"worker_instance_id,omitempty"`
	PolicyDigest     string `json:"policy_digest"`
	OK               bool   `json:"ok"`
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "worker-loopback: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	configRoot, err := requireEnv("GA_CONFIG_ROOT")
	if err != nil {
		return err
	}
	legacyRoot, err := requireEnv("GA_LEGACY_ROOT")
	if err != nil {
		return err
	}
	runtimeDir, err := requireEnv("GA_RUNTIME_DIR")
	if err != nil {
		return err
	}
	policyFile, err := requireEnv("GA_POLICY_FILE")
	if err != nil {
		return err
	}

	if err := validatePaths(configRoot, legacyRoot, runtimeDir, policyFile); err != nil {
		return err
	}

	policyDigest, err := fileDigest(policyFile)
	if err != nil {
		return fmt.Errorf("policy digest: %w", err)
	}

	// Local OpenAI-compatible fixture (test credentials only).
	fixture, err := startOAIFixture()
	if err != nil {
		return fmt.Errorf("start OAI fixture: %w", err)
	}
	defer fixture.Close()

	if err := writeFixtureMyKey(configRoot, fixture.URL); err != nil {
		return fmt.Errorf("write fixture mykey.py: %w", err)
	}

	python := os.Getenv("GA_WORKER_PYTHON")
	if python == "" {
		python = os.Getenv("GA_TEST_PYTHON")
	}
	if python == "" {
		// Prefer project venv if present relative to legacy root / repo.
		candidates := []string{
			filepath.Join(legacyRoot, ".venv", "Scripts", "python.exe"),
			filepath.Join(legacyRoot, ".venv", "bin", "python"),
			"python3",
			"python",
		}
		for _, c := range candidates {
			if c == "python3" || c == "python" {
				python = c
				break
			}
			if st, err := os.Stat(c); err == nil && !st.IsDir() {
				python = c
				break
			}
		}
	}

	workerSrc := os.Getenv("GA_WORKER_SRC")
	if workerSrc == "" {
		// Default: tenant_platform/worker-python/src next to legacy root if repo layout.
		workerSrc = filepath.Join(legacyRoot, "tenant_platform", "worker-python", "src")
		if _, err := os.Stat(filepath.Join(workerSrc, "ga_worker")); err != nil {
			// Fallback: sibling of backend-go when launched from backend-go cwd.
			if wd, werr := os.Getwd(); werr == nil {
				alt := filepath.Clean(filepath.Join(wd, "..", "worker-python", "src"))
				if _, err2 := os.Stat(filepath.Join(alt, "ga_worker")); err2 == nil {
					workerSrc = alt
				}
			}
		}
	}

	proc, listenAddr, err := startPythonWorker(python, workerSrc, configRoot, legacyRoot, runtimeDir, policyFile)
	if err != nil {
		return err
	}
	defer func() {
		_ = proc.Process.Kill()
		_, _ = proc.Process.Wait()
	}()

	if !isLoopbackAddr(listenAddr) {
		return fmt.Errorf("worker listen address is not loopback: %s", listenAddr)
	}

	ctx, cancel := context.WithTimeout(context.Background(), taskTimeout)
	defer cancel()

	conn, err := grpc.DialContext(ctx, listenAddr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithBlock(),
	)
	if err != nil {
		return fmt.Errorf("dial worker %s: %w", listenAddr, err)
	}
	defer conn.Close()

	client, err := workerclient.New(conn)
	if err != nil {
		return err
	}

	// Health before session should eventually become ready after StartSession.
	if _, err := waitHealth(ctx, client, false); err != nil {
		// Non-fatal if worker is up but not ready yet; continue to StartSession.
		_ = err
	}

	startResp, err := client.StartSession(ctx, &workerv1.StartSessionRequest{
		SessionKey: defaultSessionKey,
		RuntimePolicy: &workerv1.RuntimePolicy{
			MaxTurns:           6,
			MaxHistoryBytes:    256 * 1024,
			MaxWorkingBytes:    64 * 1024,
			MaxOutputBytes:     256 * 1024,
			TaskTimeoutSeconds: 60,
			CapabilityVersion:  capabilityVersion,
			PolicyDigest:       policyDigest,
		},
	})
	if err != nil {
		return fmt.Errorf("StartSession: %w", err)
	}

	if _, err := waitHealth(ctx, client, true); err != nil {
		return fmt.Errorf("Health.ready: %w", err)
	}

	taskID := defaultTaskID
	events, errs := client.ExecuteTask(ctx, &workerv1.ExecuteTaskRequest{
		Task: &workerv1.TaskEnvelope{
			TaskId:            taskID,
			SessionKey:        defaultSessionKey,
			RequesterUserId:   1,
			Source:            "worker-loopback",
			SourceInstanceId:  "loopback-dev",
			MessageId:         "m-" + taskID,
			Prompt:            "Reply with a short greeting for the foundation loopback smoke.",
			PersonaSnapshot:   []string{"You are a concise foundation smoke agent."},
			ToolPolicyVersion: toolPolicyVersion,
			CreatedAt:         timestamppb.Now(),
		},
	})

	var terminal *workerv1.Terminal
	var streamErr error
	eventsOpen, errsOpen := true, true
	for eventsOpen || errsOpen {
		select {
		case <-ctx.Done():
			return fmt.Errorf("task timed out: %w", ctx.Err())
		case ev, ok := <-events:
			if !ok {
				eventsOpen = false
				events = nil
				continue
			}
			if ev.IsCheckpoint() {
				return errors.New("display stream illegally carried checkpoint payload")
			}
			if ev.IsTerminal() {
				terminal = ev.Terminal
			}
		case err, ok := <-errs:
			if !ok {
				errsOpen = false
				errs = nil
				continue
			}
			if err != nil && streamErr == nil {
				streamErr = err
			}
		}
	}
	if streamErr != nil {
		return fmt.Errorf("ExecuteTask stream: %w", streamErr)
	}
	if terminal == nil {
		return errors.New("ExecuteTask completed without terminal event")
	}
	if terminal.GetStatus() != workerv1.TerminalStatus_TASK_SUCCEEDED {
		return fmt.Errorf("task did not succeed: status=%s message=%q", terminal.GetStatus(), terminal.GetUserMessage())
	}

	stagingDir := filepath.Join(runtimeDir, "staging")
	if err := os.MkdirAll(stagingDir, 0o755); err != nil {
		return err
	}
	// Token-scoped staging ref (temporary path under runtime dir).
	checkpointToken := "loopback-tok-" + taskID
	stagingRef := filepath.Join(stagingDir, checkpointToken+".bundle.json")

	ready, err := client.BeginCheckpoint(ctx, &workerv1.BeginCheckpointRequest{
		TaskId:          taskID,
		CheckpointToken: checkpointToken,
		StagingRef:      stagingRef,
		MaxBundleBytes:  2 * 1024 * 1024,
	})
	if err != nil {
		return fmt.Errorf("BeginCheckpoint: %w", err)
	}
	if ready.GetCheckpointToken() != checkpointToken {
		return fmt.Errorf("checkpoint token not preserved: got %q want %q", ready.GetCheckpointToken(), checkpointToken)
	}
	if ready.GetChecksum() == "" {
		return errors.New("checkpoint ready missing checksum")
	}

	if err := client.Shutdown(ctx, "loopback-complete"); err != nil {
		return fmt.Errorf("Shutdown: %w", err)
	}

	out := summary{
		TaskID:           taskID,
		Status:           terminal.GetStatus().String(),
		UserMessage:      boundString(terminal.GetUserMessage(), 512),
		ResultDigest:     terminal.GetResultDigest(),
		CheckpointToken:  ready.GetCheckpointToken(),
		Checksum:         ready.GetChecksum(),
		CheckpointDigest: ready.GetResultDigest(),
		StagingRef:       ready.GetStagingRef(),
		WorkerInstanceID: startResp.GetWorkerInstanceId(),
		PolicyDigest:     policyDigest,
		OK:               true,
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(out); err != nil {
		return err
	}
	return nil
}

func requireEnv(name string) (string, error) {
	v := strings.TrimSpace(os.Getenv(name))
	if v == "" {
		return "", fmt.Errorf("missing required environment variable: %s", name)
	}
	return v, nil
}

func validatePaths(configRoot, legacyRoot, runtimeDir, policyFile string) error {
	if st, err := os.Stat(configRoot); err != nil || !st.IsDir() {
		return fmt.Errorf("GA_CONFIG_ROOT is not a directory: %s", configRoot)
	}
	if st, err := os.Stat(legacyRoot); err != nil || !st.IsDir() {
		return fmt.Errorf("GA_LEGACY_ROOT is not a directory: %s", legacyRoot)
	}
	if _, err := os.Stat(filepath.Join(legacyRoot, "agentmain.py")); err != nil {
		return fmt.Errorf("agentmain.py missing under GA_LEGACY_ROOT: %s", legacyRoot)
	}
	if err := os.MkdirAll(runtimeDir, 0o755); err != nil {
		return fmt.Errorf("GA_RUNTIME_DIR: %w", err)
	}
	if st, err := os.Stat(policyFile); err != nil || st.IsDir() {
		return fmt.Errorf("GA_POLICY_FILE is not a file: %s", policyFile)
	}
	return nil
}

func fileDigest(path string) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

func writeFixtureMyKey(configRoot, apibase string) error {
	// Development smoke only: fixture credentials, never a real key.
	// Refuse to overwrite an existing mykey.py so a mis-pointed GA_CONFIG_ROOT
	// cannot destroy real secrets. No overwrite flag / backup fallback.
	content := fmt.Sprintf(
		"native_oai_config = {\n"+
			"    'name': 'loopback-fixture-gpt',\n"+
			"    'apikey': %q,\n"+
			"    'apibase': %q,\n"+
			"    'model': 'gpt-test',\n"+
			"    'api_mode': 'chat_completions',\n"+
			"    'stream': False,\n"+
			"    'read_timeout': 30,\n"+
			"}\n",
		testToken, apibase,
	)
	path := filepath.Join(configRoot, "mykey.py")
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return fmt.Errorf("refusing to overwrite existing mykey.py at %s: %w", path, err)
		}
		return fmt.Errorf("create mykey.py at %s: %w", path, err)
	}
	defer f.Close()
	if _, err := f.Write([]byte(content)); err != nil {
		return fmt.Errorf("write mykey.py at %s: %w", path, err)
	}
	return nil
}

type oaiFixture struct {
	URL    string
	server *http.Server
	ln     net.Listener
}

func (f *oaiFixture) Close() {
	if f == nil || f.server == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = f.server.Shutdown(ctx)
	if f.ln != nil {
		_ = f.ln.Close()
	}
}

func startOAIFixture() (*oaiFixture, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		if !strings.Contains(auth, testToken) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		body := map[string]any{
			"id":     "chatcmpl-loopback",
			"object": "chat.completion",
			"choices": []map[string]any{
				{
					"index": 0,
					"message": map[string]string{
						"role":    "assistant",
						"content": fixtureResponseText,
					},
					"finish_reason": "stop",
				},
			},
			"usage": map[string]int{"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2},
		}
		raw, _ := json.Marshal(body)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(raw)))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(raw)
	})
	mux := http.NewServeMux()
	mux.Handle("/v1/chat/completions", handler)
	mux.Handle("/chat/completions", handler)

	srv := &http.Server{Handler: mux}
	go func() { _ = srv.Serve(ln) }()
	return &oaiFixture{
		URL:    "http://" + ln.Addr().String(),
		server: srv,
		ln:     ln,
	}, nil
}

func startPythonWorker(python, workerSrc, configRoot, legacyRoot, runtimeDir, policyFile string) (*exec.Cmd, string, error) {
	// Force loopback-only bind.
	listen := "127.0.0.1:0"

	cmd := exec.Command(python, "-m", "ga_worker.entrypoint", "--listen", listen, "--grace-seconds", "5")
	cmd.Dir = filepath.Dir(workerSrc) // worker-python root if src is .../src
	if base := filepath.Base(workerSrc); base == "src" {
		cmd.Dir = filepath.Dir(workerSrc)
	}

	env := os.Environ()
	env = setEnv(env, "GA_CONFIG_ROOT", configRoot)
	env = setEnv(env, "GA_LEGACY_ROOT", legacyRoot)
	env = setEnv(env, "GA_RUNTIME_DIR", runtimeDir)
	env = setEnv(env, "GA_POLICY_FILE", policyFile)
	env = setEnv(env, "GA_WORKER_LISTEN", listen)
	// Strip real cloud keys from the worker process environment.
	env = unsetEnv(env, "OPENAI_API_KEY")
	env = unsetEnv(env, "ANTHROPIC_API_KEY")
	// Ensure ga_worker is importable.
	pp := workerSrc
	if existing := getEnv(env, "PYTHONPATH"); existing != "" {
		pp = workerSrc + string(os.PathListSeparator) + existing
	}
	env = setEnv(env, "PYTHONPATH", pp)
	cmd.Env = env

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, "", err
	}
	cmd.Stderr = cmd.Stdout // merge

	if err := cmd.Start(); err != nil {
		return nil, "", fmt.Errorf("start python worker: %w", err)
	}

	listenAddr, err := waitWorkerListen(stdout, workerStartTimeout)
	if err != nil {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
		// Drain remaining for diagnostics.
		rest, _ := io.ReadAll(stdout)
		return nil, "", fmt.Errorf("%w\nworker output:\n%s", err, string(rest))
	}

	// Continue draining stdout in background so the pipe does not block.
	go func() { _, _ = io.Copy(io.Discard, stdout) }()

	return cmd, listenAddr, nil
}

func waitWorkerListen(r io.Reader, timeout time.Duration) (string, error) {
	deadline := time.Now().Add(timeout)
	buf := make([]byte, 0, 4096)
	tmp := make([]byte, 512)
	for time.Now().Before(deadline) {
		// Non-blocking-ish read with short deadline via SetReadDeadline not available on pipe;
		// read with small chunks; process may block until line arrives.
		n, err := r.Read(tmp)
		if n > 0 {
			buf = append(buf, tmp[:n]...)
			if m := workerListenRE.FindSubmatch(buf); m != nil {
				return string(m[1]), nil
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return "", fmt.Errorf("worker exited before publishing WORKER_LISTEN; output:\n%s", string(buf))
			}
			return "", err
		}
	}
	return "", fmt.Errorf("timeout waiting for WORKER_LISTEN; output so far:\n%s", string(buf))
}

func waitHealth(ctx context.Context, client workerclient.WorkerClient, wantReady bool) (*workerv1.HealthResponse, error) {
	var last error
	for {
		if err := ctx.Err(); err != nil {
			if last != nil {
				return nil, fmt.Errorf("%w (last health error: %v)", err, last)
			}
			return nil, err
		}
		h, err := client.Health(ctx)
		if err != nil {
			last = err
		} else if h.GetReady() == wantReady || !wantReady {
			// When wantReady=false, any successful health is fine.
			if wantReady && !h.GetReady() {
				last = errors.New("not ready")
			} else {
				return h, nil
			}
		} else {
			last = errors.New("not ready")
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(healthPollInterval):
		}
	}
}

func isLoopbackAddr(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		// bare host?
		host = addr
	}
	host = strings.Trim(host, "[]")
	ip := net.ParseIP(host)
	if ip != nil {
		return ip.IsLoopback()
	}
	return host == "localhost"
}

func setEnv(env []string, key, value string) []string {
	prefix := key + "="
	for i, e := range env {
		if strings.HasPrefix(e, prefix) {
			env[i] = prefix + value
			return env
		}
	}
	return append(env, prefix+value)
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

func boundString(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max]
}
