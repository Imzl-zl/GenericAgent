package worker

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	workerv1 "github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/gen/worker/v1"
	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/infrastructure/workerclient"
)

type blockedShutdownClient struct{}

func (blockedShutdownClient) StartSession(context.Context, *workerv1.StartSessionRequest) (*workerv1.StartSessionResponse, error) {
	return nil, nil
}

func (blockedShutdownClient) ReloadCredentials(context.Context, *workerv1.ReloadCredentialsRequest) (*workerv1.ReloadCredentialsResponse, error) {
	return nil, nil
}

func (blockedShutdownClient) ExecuteTask(context.Context, *workerv1.ExecuteTaskRequest) (<-chan workerclient.WorkerEvent, <-chan error) {
	return nil, nil
}

func (blockedShutdownClient) BeginCheckpoint(context.Context, *workerv1.BeginCheckpointRequest) (*workerv1.CheckpointReady, error) {
	return nil, nil
}

func (blockedShutdownClient) CancelTask(context.Context, string) error { return nil }

func (blockedShutdownClient) Health(context.Context) (*workerv1.HealthResponse, error) {
	return nil, nil
}

func (blockedShutdownClient) Shutdown(ctx context.Context, _ string) error {
	<-ctx.Done()
	return ctx.Err()
}

func TestProcessCleanerBoundsShutdownAndContinuesCleanup(t *testing.T) {
	worker := blockedShutdownClient{}
	var closeCalled, killCalled, waitCalled atomic.Bool
	cleanup := processCleaner{
		client:      worker,
		closeConn:   func() error { closeCalled.Store(true); return nil },
		killProcess: func() error { killCalled.Store(true); return nil },
		waitProcess: func() error { waitCalled.Store(true); return nil },
	}
	started := time.Now()
	cleanup.run(50 * time.Millisecond)
	if elapsed := time.Since(started); elapsed >= 500*time.Millisecond {
		t.Fatalf("cleanup exceeded outer budget: %s", elapsed)
	}
	if !closeCalled.Load() || !killCalled.Load() || !waitCalled.Load() {
		t.Fatalf("cleanup did not continue: close=%v kill=%v wait=%v", closeCalled.Load(), killCalled.Load(), waitCalled.Load())
	}
}

func TestBuildWorkerEnvironmentExcludesInheritedPlatformSecrets(t *testing.T) {
	inherited := []string{
		"PATH=C:\\tools",
		"LANG=en_US.UTF-8",
		"LLM_PROVIDER_API_KEY=real-upstream-key",
		"LLM_PROXY_CAPABILITY_SIGNING_KEY=signing-secret",
		"DATABASE_URL=postgres://secret",
		"BOT_TOKEN_KEY=bot-secret",
		"PLATFORM_DEV_TOKEN=dev-secret",
		"OPENAI_API_KEY=openai-secret",
		"ANTHROPIC_API_KEY=anthropic-secret",
		"UNRELATED_SECRET=must-not-cross-boundary",
		"PYTHONPATH=untrusted-parent-path",
	}
	env := buildWorkerEnvironment(
		inherited,
		LoopbackConfig{LegacyRoot: "C:\\ga", PolicyFile: "C:\\policy.json"},
		StartRequest{ConfigDir: "C:\\config", RuntimeDir: "C:\\runtime"},
		"C:\\worker-src",
		"127.0.0.1:0",
	)

	for _, secret := range []string{
		"LLM_PROVIDER_API_KEY", "LLM_PROXY_CAPABILITY_SIGNING_KEY", "DATABASE_URL",
		"BOT_TOKEN_KEY", "PLATFORM_DEV_TOKEN", "OPENAI_API_KEY", "ANTHROPIC_API_KEY", "UNRELATED_SECRET",
	} {
		if value := getEnv(env, secret); value != "" {
			t.Fatalf("Worker inherited %s=%q", secret, value)
		}
	}
	if got := getEnv(env, "PATH"); got != "C:\\tools" {
		t.Fatalf("PATH=%q", got)
	}
	if got := getEnv(env, "LANG"); got != "en_US.UTF-8" {
		t.Fatalf("LANG=%q", got)
	}
	for key, want := range map[string]string{
		"GA_CONFIG_ROOT":   "C:\\config",
		"GA_LEGACY_ROOT":   "C:\\ga",
		"GA_RUNTIME_DIR":   "C:\\runtime",
		"GA_WORKER_LISTEN": "127.0.0.1:0",
		"GA_POLICY_FILE":   "C:\\policy.json",
		"PYTHONPATH":       "C:\\worker-src",
	} {
		if got := getEnv(env, key); got != want {
			t.Fatalf("%s=%q, want %q", key, got, want)
		}
	}
}
