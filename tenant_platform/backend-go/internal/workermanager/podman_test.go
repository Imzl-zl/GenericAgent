package workermanager

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type fakeExecutor struct {
	calls   [][]string
	nextID  string
	failRun bool
}

func (f *fakeExecutor) Run(_ context.Context, args []string) ([]byte, error) {
	f.calls = append(f.calls, args)
	if f.failRun {
		return []byte("error"), fmt.Errorf("podman failed")
	}
	return []byte(f.nextID + "\n"), nil
}

func TestNewRuntimeRejectsMissingConfig(t *testing.T) {
	cases := []RuntimeConfig{
		{Image: "", Executor: &fakeExecutor{}},
		{Image: "img"},
	}
	for i, cfg := range cases {
		_, err := NewRuntime(cfg)
		if err == nil {
			t.Fatalf("case %d: expected error", i)
		}
	}
}

func TestNewRuntimeAcceptsValidConfig(t *testing.T) {
	_, err := NewRuntime(RuntimeConfig{Image: "img", Executor: &fakeExecutor{}})
	if err != nil {
		t.Fatalf("expected valid config: %v", err)
	}
}

func TestRuntimeAllocateAndRelease(t *testing.T) {
	exec := &fakeExecutor{nextID: "container-123"}
	runtimeRoot := t.TempDir()
	runtime, err := NewRuntime(RuntimeConfig{Image: "ga-worker:latest", Executor: exec})
	if err != nil {
		t.Fatalf("new runtime: %v", err)
	}

	configDir := t.TempDir()
	ctx := context.Background()
	c, err := runtime.Allocate(ctx, "session-1", configDir, runtimeRoot)
	if err != nil {
		t.Fatalf("allocate: %v", err)
	}
	if c.ID != "container-123" {
		t.Fatalf("unexpected id: %s", c.ID)
	}
	if c.SessionKey != "session-1" {
		t.Fatalf("unexpected session: %s", c.SessionKey)
	}
	expectedSocket := runtimeRoot + "/session-1/rpc/worker.sock"
	if c.SocketPath != expectedSocket {
		t.Fatalf("socket path: got %s want %s", c.SocketPath, expectedSocket)
	}

	if len(exec.calls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(exec.calls))
	}
	args := exec.calls[0]
	if args[0] != "run" || args[1] != "-d" {
		t.Fatalf("unexpected podman args: %v", args)
	}
	if !containsArg(args, "--name", "ga-worker-session-1") {
		t.Fatalf("missing container name: %v", args)
	}
	if !dirExists(t, filepath.Join(runtimeRoot, "session-1", "rpc")) {
		t.Fatal("socket dir was not created")
	}

	if err := runtime.Release(ctx, c.ID); err != nil {
		t.Fatalf("release: %v", err)
	}
	if len(exec.calls) != 3 {
		t.Fatalf("expected 3 calls (run, stop, rm), got %d", len(exec.calls))
	}
	if exec.calls[1][0] != "stop" || exec.calls[2][0] != "rm" {
		t.Fatalf("expected stop then rm, got %v", exec.calls[1:])
	}
}

func TestRuntimeAllocatePodmanFailure(t *testing.T) {
	exec := &fakeExecutor{failRun: true}
	runtime, err := NewRuntime(RuntimeConfig{Image: "ga-worker:latest", Executor: exec})
	if err != nil {
		t.Fatalf("new runtime: %v", err)
	}
	_, err = runtime.Allocate(context.Background(), "session-1", t.TempDir(), t.TempDir())
	if err == nil {
		t.Fatal("expected error when podman fails")
	}
}

func TestRuntimeList(t *testing.T) {
	exec := &fakeExecutor{nextID: "container-456"}
	runtime, err := NewRuntime(RuntimeConfig{Image: "ga-worker:latest", Executor: exec})
	if err != nil {
		t.Fatalf("new runtime: %v", err)
	}
	ctx := context.Background()
	_, _ = runtime.Allocate(ctx, "session-a", t.TempDir(), t.TempDir())
	list := runtime.List()
	if len(list) != 1 {
		t.Fatalf("expected 1 container, got %d", len(list))
	}
	_ = runtime.Release(ctx, "container-456")
	if len(runtime.List()) != 0 {
		t.Fatal("expected empty list after release")
	}
}

func containsArg(args []string, key, value string) bool {
	for i := 0; i < len(args)-1; i++ {
		if args[i] == key && args[i+1] == value {
			return true
		}
	}
	return false
}

func dirExists(t *testing.T, path string) bool {
	t.Helper()
	st, err := os.Stat(path)
	return err == nil && st.IsDir()
}

func TestContainerNameUsesSessionKey(t *testing.T) {
	if got := containerName("abc"); !strings.HasPrefix(got, "ga-worker-") {
		t.Fatalf("unexpected name: %s", got)
	}
}
