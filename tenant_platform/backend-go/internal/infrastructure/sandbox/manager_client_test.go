package sandbox

import (
	"context"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

// fakeCLI2 记录创建并返回固定 Runner(控制面测试用)。
type fakeCLI2 struct {
	specs []RunnerSpec
}

func (f *fakeCLI2) CreateAndStart(ctx context.Context, spec RunnerSpec) (Runner, error) {
	f.specs = append(f.specs, spec)
	return Runner{ContainerID: "cid-1", Name: "ga-runner-" + spec.WorkspaceHash[:12] + "-g" + strconv.FormatUint(spec.Generation, 10)}, nil
}

func (f *fakeCLI2) Destroy(ctx context.Context, name string) error { return nil }
func (f *fakeCLI2) Inspect(ctx context.Context, name string) error { return nil }

func TestManagerControlAPIRoundTrip(t *testing.T) {
	cli := &fakeCLI2{}
	manager := NewManager(ManagerConfig{CLI: cli, WorkspaceRoot: t.TempDir(), ContainerNamePrefix: "ga-runner"})
	server, err := NewManagerServer(manager, "0123456789abcdef-secret")
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(server.Handler())
	defer ts.Close()

	client, err := NewManagerClient(ts.URL, "0123456789abcdef-secret", "ga-runner")
	if err != nil {
		t.Fatal(err)
	}

	// ensure 创建并携带证书/环境
	runner, created, err := client.EnsureRunner(context.Background(), EnsureRunnerRequest{
		WorkspaceKey: "personal:7",
		Generation:   1,
		Env:          []string{"GA_LLM_PROXY_ADDR=http://llm-proxy:8081"},
		ConfigFiles:  map[string][]byte{"server.crt": []byte("cert"), "policy.json": []byte(`{}`)},
	})
	if err != nil {
		t.Fatalf("EnsureRunner: %v", err)
	}
	if !created || runner.Name == "" {
		t.Fatalf("runner = %+v created=%v", runner, created)
	}
	if len(cli.specs) != 1 {
		t.Fatalf("expected 1 create, got %d", len(cli.specs))
	}
	if got := cli.specs[0].Env; len(got) != 1 || got[0] != "GA_LLM_PROXY_ADDR=http://llm-proxy:8081" {
		t.Fatalf("env not forwarded: %v", got)
	}
	if cli.specs[0].ConfigFiles["server.crt"] == nil {
		t.Fatal("config files not forwarded")
	}

	// 同 generation 复用
	again, created, err := client.EnsureRunner(context.Background(), EnsureRunnerRequest{
		WorkspaceKey: "personal:7", Generation: 1,
	})
	if err != nil {
		t.Fatalf("EnsureRunner reuse: %v", err)
	}
	if created || again.Name != runner.Name {
		t.Fatalf("reuse = %+v created=%v", again, created)
	}

	// inspect + destroy + list
	if err := client.Inspect(context.Background(), runner.Name); err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if err := client.Destroy(context.Background(), runner.Name); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	names, err := client.ListRunners(context.Background())
	if err != nil {
		t.Fatalf("ListRunners: %v", err)
	}
	if len(names) != 0 {
		t.Fatalf("runners after destroy = %v", names)
	}
}

func TestManagerControlAPIRejectsBadSignature(t *testing.T) {
	cli := &fakeCLI2{}
	manager := NewManager(ManagerConfig{CLI: cli, WorkspaceRoot: t.TempDir(), ContainerNamePrefix: "ga-runner"})
	server, _ := NewManagerServer(manager, "0123456789abcdef-secret")
	ts := httptest.NewServer(server.Handler())
	defer ts.Close()

	// 错误 secret 的客户端必须被拒绝
	client, _ := NewManagerClient(ts.URL, "wrong-secret-0123456789", "ga-runner")
	if _, _, err := client.EnsureRunner(context.Background(), EnsureRunnerRequest{
		WorkspaceKey: "personal:7", Generation: 1,
	}); err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("bad secret must be rejected, got %v", err)
	}
}

func TestManagerControlAPIRequiresSecret(t *testing.T) {
	if _, err := NewManagerServer(NewManager(ManagerConfig{}), "short"); err == nil {
		t.Fatal("short secret must be rejected")
	}
	if _, err := NewManagerClient("http://x", "short", "ga-runner"); err == nil {
		t.Fatal("short secret must be rejected")
	}
}

func TestManagerControlAPIRejectsUnsafeDestroyName(t *testing.T) {
	cli := &fakeCLI2{}
	manager := NewManager(ManagerConfig{CLI: cli, WorkspaceRoot: t.TempDir(), ContainerNamePrefix: "ga-runner"})
	server, _ := NewManagerServer(manager, "0123456789abcdef-secret")
	ts := httptest.NewServer(server.Handler())
	defer ts.Close()
	client, _ := NewManagerClient(ts.URL, "0123456789abcdef-secret", "ga-runner")
	if err := client.Destroy(context.Background(), "../evil"); err == nil {
		t.Fatal("path traversal name must be rejected")
	}
}
