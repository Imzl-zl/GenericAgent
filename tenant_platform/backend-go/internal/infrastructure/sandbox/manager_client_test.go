package sandbox

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

// fakeCLI2 记录创建并返回固定 Runner(控制面测试用)。
type fakeCLI2 struct {
	runnerContainer bool
	containerExists bool
	specs           []RunnerSpec
	ensureCalls     []string
}

func (f *fakeCLI2) CreateAndStart(ctx context.Context, spec RunnerSpec) (Runner, error) {
	f.specs = append(f.specs, spec)
	return Runner{ContainerID: "cid-1", Name: "ga-runner-" + spec.WorkspaceHash[:12] + "-g" + strconv.FormatUint(spec.Generation, 10)}, nil
}

func (f *fakeCLI2) Destroy(ctx context.Context, name string) error { return nil }
func (f *fakeCLI2) Inspect(ctx context.Context, name string) error { return nil }
func (f *fakeCLI2) IsRunnerContainer(ctx context.Context, idOrName string) (bool, error) {
	return f.runnerContainer, nil
}

func (f *fakeCLI2) ContainerExists(ctx context.Context, idOrName string) (bool, error) {
	return f.containerExists, nil
}

func (f *fakeCLI2) RunnerWorkspaceHash(ctx context.Context, idOrName string) (string, bool, error) {
	return "", false, nil
}
func (f *fakeCLI2) EnsureWorkspace(ctx context.Context, workspaceHash string) error {
	f.ensureCalls = append(f.ensureCalls, workspaceHash)
	return nil
}

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

func TestManagerControlAPIEnsureWorkspaceRoundTrip(t *testing.T) {
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
	if err := client.EnsureWorkspace(context.Background(), "personal:9"); err != nil {
		t.Fatalf("EnsureWorkspace: %v", err)
	}
	if len(cli.ensureCalls) != 1 || cli.ensureCalls[0] != mustWorkspaceHash("personal:9") {
		t.Fatalf("ensure calls = %v", cli.ensureCalls)
	}
	if err := client.EnsureWorkspace(context.Background(), ""); err == nil {
		t.Fatal("EnsureWorkspace with empty key must fail client-side")
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

// TestManagerControlAPIRejectsReplayedNonce 验证同一签名请求(nonce 相同)
// 重放会被一次性 nonce 集合拒绝(防重放, 方案 §7)。
func TestManagerControlAPIRejectsReplayedNonce(t *testing.T) {
	cli := &fakeCLI2{}
	manager := NewManager(ManagerConfig{CLI: cli, WorkspaceRoot: t.TempDir(), ContainerNamePrefix: "ga-runner"})
	server, err := NewManagerServer(manager, "0123456789abcdef-secret")
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(server.Handler())
	defer ts.Close()

	sign := func(method, path, timestamp, nonce, body string) string {
		canonical := strings.Join([]string{method, path, timestamp, nonce, body}, "\n")
		mac := hmac.New(sha256.New, []byte("0123456789abcdef-secret"))
		_, _ = mac.Write([]byte(canonical))
		return hex.EncodeToString(mac.Sum(nil))
	}

	do := func() int {
		timestamp := strconv.FormatInt(time.Now().Unix(), 10)
		const nonce = "deadbeefdeadbeefdeadbeefdeadbeef"
		req, err := http.NewRequest("GET", ts.URL+"/v1/runners", nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("X-GA-Timestamp", timestamp)
		req.Header.Set("X-GA-Nonce", nonce)
		req.Header.Set("X-GA-Signature", sign("GET", "/v1/runners", timestamp, nonce, ""))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		return resp.StatusCode
	}
	if code := do(); code != http.StatusOK {
		t.Fatalf("first request = %d, want 200", code)
	}
	if code := do(); code != http.StatusUnauthorized {
		t.Fatalf("replayed request = %d, want 401", code)
	}
}

// TestManagerControlAPIRejectsArbitraryDestroyName 验证控制面不能销毁
// 非 Runner 命名模式且无 Runner label 的容器(任意 rm 防护, 方案 §7);
// 容器 ID 但 label 校验通过(接管清理 stale_container_id 的场景)必须允许。
func TestManagerControlAPIRejectsArbitraryDestroyName(t *testing.T) {
	cli := &fakeCLI2{}
	manager := NewManager(ManagerConfig{CLI: cli, WorkspaceRoot: t.TempDir(), ContainerNamePrefix: "ga-runner"})
	server, _ := NewManagerServer(manager, "0123456789abcdef-secret")
	ts := httptest.NewServer(server.Handler())
	defer ts.Close()
	client, _ := NewManagerClient(ts.URL, "0123456789abcdef-secret", "ga-runner")
	// 存在但非 Runner 的容器名(如 compose 里的 postgres-1)必须拒绝。
	cli.runnerContainer = false
	cli.containerExists = true
	if err := client.Destroy(context.Background(), "postgres-1"); err == nil {
		t.Fatal("destroying a non-runner container name must be rejected")
	}
	// 名字不匹配模式但容器存在且非 Runner: 必须拒绝(防任意 rm)。
	cli.runnerContainer = false
	cli.containerExists = true
	if err := client.Destroy(context.Background(), "ga-runner-1234567890xz-g1"); err == nil {
		t.Fatal("runner name with non-hex hash must be rejected")
	}
	// 容器 ID(非命名模式)但带 Runner label: 接管清理路径, 必须允许(审查)。
	cli.runnerContainer = true
	if err := client.Destroy(context.Background(), "a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b"); err != nil {
		t.Fatalf("destroying a labeled runner container id must be allowed: %v", err)
	}
	// label 校验失败(存在但非 Runner)必须拒绝。
	cli.runnerContainer = false
	cli.containerExists = true
	if err := client.Destroy(context.Background(), "deadbeefdeadbeef"); err == nil {
		t.Fatal("destroying a non-runner container id must be rejected")
	}
	// round10 审查(B1): 容器已不存在时销毁必须幂等成功(lease 记录的
	// container_id 可能已被 idle 回收删除; 拒绝会让 stale_container_id
	// 清理永久失败, 该工作区无法重建 Runner)。
	cli.runnerContainer = false
	cli.containerExists = false
	if err := client.Destroy(context.Background(), "a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b"); err != nil {
		t.Fatalf("destroying a missing container id must be idempotent success: %v", err)
	}
}
