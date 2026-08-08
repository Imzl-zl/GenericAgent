package mcpgateway

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeCatalog 是内存 catalog。
type fakeCatalog struct {
	servers []Server
}

func (c *fakeCatalog) EnabledServers(ctx context.Context) ([]Server, error) {
	return c.servers, nil
}

// crashServerDef 构造一个处理 n 个请求后崩溃的 server 定义。
func crashServerDef(binary string, crashAfter int) Server {
	def := fakeServerDef(binary)
	def.Args = []string{fmt.Sprintf("-crash-after=%d", crashAfter)}
	return def
}

func fakeServerDef(binary string) Server {
	return Server{
		ServerID:    "fake",
		Name:        "fake mcp",
		Command:     binary,
		Timeout:     5 * time.Second,
		MaxInstance: 1,
	}
}

// buildFakeServer 编译 mock stdio server 到 t.TempDir()。
func buildFakeServer(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	binary := filepath.Join(dir, "fakeserver")
	cmd := exec.Command("go", "build", "-o", binary, "./testdata/fakeserver")
	cmd.Dir = "."
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build fakeserver: %v\n%s", err, out)
	}
	return binary
}

func newTestGateway(t *testing.T, servers ...Server) (*Gateway, *httptest.Server) {
	t.Helper()
	gateway := New(Config{
		Catalog:  &fakeCatalog{servers: servers},
		WorkRoot: filepath.Join(t.TempDir(), "work"),
		IdleTTL:  50 * time.Millisecond,
	})
	ts := httptest.NewServer(gateway.Handler())
	t.Cleanup(func() {
		ts.Close()
		gateway.Close()
	})
	return gateway, ts
}

func postJSON(t *testing.T, base, serverID string, payload map[string]any) (*http.Response, []byte) {
	t.Helper()
	body, _ := json.Marshal(payload)
	req, err := http.NewRequest("POST", base+"/v1/mcp/"+serverID, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var buf bytes.Buffer
	_, _ = buf.ReadFrom(resp.Body)
	return resp, buf.Bytes()
}

func parseResp(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var msg map[string]any
	if err := json.Unmarshal(raw, &msg); err != nil {
		t.Fatalf("unmarshal response %q: %v", raw, err)
	}
	return msg
}

func initializeReq(id any) map[string]any {
	return map[string]any{
		"jsonrpc": "2.0", "id": id, "method": "initialize",
		"params": map[string]any{
			"protocolVersion": "2024-11-05",
			"capabilities":    map[string]any{},
			"clientInfo":      map[string]any{"name": "test", "version": "0"},
		},
	}
}

func initGateway(t *testing.T, base, serverID string, clientID any) {
	t.Helper()
	resp, raw := postJSON(t, base, serverID, initializeReq(clientID))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("initialize code = %d body=%s", resp.StatusCode, raw)
	}
	msg := parseResp(t, raw)
	if msg["result"] == nil {
		t.Fatalf("initialize missing result: %s", raw)
	}
	// 关键协议不变量: 响应 id 必须回显客户端 id(worker 严格校验)。
	// (JSON 数字解析为 float64, 用字符串比较跨类型安全。)
	if fmt.Sprint(msg["id"]) != fmt.Sprint(clientID) {
		t.Fatalf("initialize response id = %v, want %v", msg["id"], clientID)
	}
}

// TestGatewayFullFlow: initialize → tools/list → tools/call 全链路,
// 每个响应 id 回显客户端 id(JSON-RPC 协议不变量)。
func TestGatewayFullFlow(t *testing.T) {
	binary := buildFakeServer(t)
	_, ts := newTestGateway(t, fakeServerDef(binary))

	initGateway(t, ts.URL, "fake", 1)

	resp, raw := postJSON(t, ts.URL, "fake", map[string]any{"jsonrpc": "2.0", "id": 2, "method": "tools/list"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("tools/list code = %d body=%s", resp.StatusCode, raw)
	}
	tools := parseResp(t, raw)
	if tools["result"] == nil {
		t.Fatalf("tools/list missing result: %s", raw)
	}
	if tools["id"] != float64(2) {
		t.Fatalf("tools/list response id = %v, want 2", tools["id"])
	}

	resp, raw = postJSON(t, ts.URL, "fake", map[string]any{
		"jsonrpc": "2.0", "id": 3, "method": "tools/call",
		"params": map[string]any{"name": "echo", "arguments": map[string]any{"text": "hello"}},
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("tools/call code = %d body=%s", resp.StatusCode, raw)
	}
	call := parseResp(t, raw)
	if call["result"] == nil {
		t.Fatalf("tools/call missing result: %s", raw)
	}
	if call["id"] != float64(3) {
		t.Fatalf("tools/call response id = %v, want 3", call["id"])
	}
}

// TestGatewayNoSessionHeader: gateway 无状态——不带任何会话头的调用
// 直接转发到进程(进程按协议拒绝未初始化调用); 已初始化则正常服务。
func TestGatewayNoSessionHeader(t *testing.T) {
	binary := buildFakeServer(t)
	_, ts := newTestGateway(t, fakeServerDef(binary))

	initGateway(t, ts.URL, "fake", 1)
	// 无会话头(也没有任何头)的 tools/list 必须成功。
	resp, raw := postJSON(t, ts.URL, "fake", map[string]any{"jsonrpc": "2.0", "id": 2, "method": "tools/list"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("sessionless tools/list code = %d body=%s", resp.StatusCode, raw)
	}
}

// TestGatewayUnknownServer: 白名单外的 server_id 一律 404(fail-closed)。
func TestGatewayUnknownServer(t *testing.T) {
	binary := buildFakeServer(t)
	_, ts := newTestGateway(t, fakeServerDef(binary))
	resp, raw := postJSON(t, ts.URL, "nope", initializeReq(1))
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("code = %d, want 404 (body=%s)", resp.StatusCode, raw)
	}
}

// TestGatewaySecondClientInitialize: 第二个客户端 initialize 复用首个
// 客户端的握手缓存, 但响应 id 回显第二个客户端的 id。
func TestGatewaySecondClientInitialize(t *testing.T) {
	binary := buildFakeServer(t)
	gateway, ts := newTestGateway(t, fakeServerDef(binary))

	initGateway(t, ts.URL, "fake", 1)

	// 第二个客户端用不同的 id 与 params。
	req := initializeReq("second-client-id")
	req["params"].(map[string]any)["clientInfo"] = map[string]any{"name": "other", "version": "9"}
	resp, raw := postJSON(t, ts.URL, "fake", req)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("second initialize code = %d body=%s", resp.StatusCode, raw)
	}
	msg := parseResp(t, raw)
	if msg["id"] != "second-client-id" {
		t.Fatalf("second initialize response id = %v, want %q", msg["id"], "second-client-id")
	}
	// 进程只被握手一次。
	gateway.mu.Lock()
	pool := gateway.pools["fake"]
	gateway.mu.Unlock()
	if pool == nil || len(pool.procs) == 0 {
		t.Fatal("expected a shared process")
	}
}

// TestGatewayProcessPool: max_instances=2 时并发请求分流到两个进程。
func TestGatewayProcessPool(t *testing.T) {
	binary := buildFakeServer(t)
	def := fakeServerDef(binary)
	def.MaxInstance = 2
	def.Args = []string{"-slow-ms=60"} // 让并发请求真正堆积, 触发扩容
	gateway, ts := newTestGateway(t, def)

	initGateway(t, ts.URL, "fake", 1)

	const calls = 6
	var wg sync.WaitGroup
	errs := make(chan error, calls)
	for i := 0; i < calls; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			resp, raw := postJSON(t, ts.URL, "fake", map[string]any{
				"jsonrpc": "2.0", "id": 100 + i, "method": "tools/call",
				"params": map[string]any{"name": "echo", "arguments": map[string]any{"text": "hi"}},
			})
			if resp.StatusCode != http.StatusOK {
				errs <- fmt.Errorf("call %d code = %d body=%s", i, resp.StatusCode, raw)
				return
			}
			msg := parseResp(t, raw)
			if msg["id"] != float64(100+i) {
				errs <- fmt.Errorf("call %d response id = %v, want %d", i, msg["id"], 100+i)
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}

	gateway.mu.Lock()
	pool := gateway.pools["fake"]
	gateway.mu.Unlock()
	pool.mu.Lock()
	alive := 0
	for _, proc := range pool.procs {
		if !proc.dead.Load() {
			alive++
		}
	}
	pool.mu.Unlock()
	if alive != 2 {
		t.Fatalf("expected 2 pooled processes, got %d", alive)
	}
}

// TestGatewayCrashRebuild: 进程崩溃后, 过退避窗口的新请求自动重建并重放握手。
func TestGatewayCrashRebuild(t *testing.T) {
	binary := buildFakeServer(t)
	// 处理 2 个请求(initialize + 一次 tools/list)后崩溃。
	_, ts := newTestGateway(t, crashServerDef(binary, 2))

	initGateway(t, ts.URL, "fake", 1)
	// 请求 1 成功返回, 随后进程崩溃。
	resp, raw := postJSON(t, ts.URL, "fake", map[string]any{"jsonrpc": "2.0", "id": 2, "method": "tools/list"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("pre-crash call code = %d body=%s", resp.StatusCode, raw)
	}
	// 进程已死 → 退避窗口内失败。
	resp, raw = postJSON(t, ts.URL, "fake", map[string]any{"jsonrpc": "2.0", "id": 3, "method": "tools/list"})
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("backoff window: code = %d, want 502 (body=%s)", resp.StatusCode, raw)
	}
	// 窗口外自动重建: 重放握手消耗 1 个请求额度, 本次调用成功。
	time.Sleep(2*DefaultCrashBackoff + 50*time.Millisecond)
	resp, raw = postJSON(t, ts.URL, "fake", map[string]any{"jsonrpc": "2.0", "id": 4, "method": "tools/list"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("after backoff: code = %d body=%s", resp.StatusCode, raw)
	}
}

// TestGatewayIdleReap: 空闲进程被回收后, 后续请求重建(重放握手)。
func TestGatewayIdleReap(t *testing.T) {
	binary := buildFakeServer(t)
	gateway, ts := newTestGateway(t, fakeServerDef(binary))

	initGateway(t, ts.URL, "fake", 1)

	gateway.mu.Lock()
	pool := gateway.pools["fake"]
	gateway.mu.Unlock()
	if pool == nil || len(pool.procs) == 0 {
		t.Fatal("expected process after initialize")
	}
	firstProc := pool.procs[0]
	time.Sleep(80 * time.Millisecond) // > idleTTL(50ms)
	gateway.reap(time.Now())

	pool.mu.Lock()
	reaped := len(pool.procs) == 0
	pool.mu.Unlock()
	if !reaped {
		t.Fatal("expected idle processes to be reaped")
	}
	// 新请求触发重建, 自动重放握手。
	resp, raw := postJSON(t, ts.URL, "fake", map[string]any{"jsonrpc": "2.0", "id": 2, "method": "tools/list"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("rebuilt call code = %d body=%s", resp.StatusCode, raw)
	}
	pool.mu.Lock()
	rebuilt := len(pool.procs) == 1 && pool.procs[0] != firstProc
	pool.mu.Unlock()
	if !rebuilt {
		t.Fatal("expected a rebuilt process")
	}
}

// TestGatewayConcurrentClients: 多客户端并发, 各自 id 序列互不干扰
// (原实现用 gateway 自造 id, 第二个客户端必然错位——回归测试)。
func TestGatewayConcurrentClients(t *testing.T) {
	binary := buildFakeServer(t)
	_, ts := newTestGateway(t, fakeServerDef(binary))

	const clients = 4
	const calls = 5
	var wg sync.WaitGroup
	errs := make(chan error, clients*calls)
	for c := 0; c < clients; c++ {
		wg.Add(1)
		go func(c int) {
			defer wg.Done()
			baseID := 1000 + c*100
			initGateway(t, ts.URL, "fake", baseID)
			for i := 1; i <= calls; i++ {
				reqID := baseID + i
				resp, raw := postJSON(t, ts.URL, "fake", map[string]any{
					"jsonrpc": "2.0", "id": reqID, "method": "tools/call",
					"params": map[string]any{"name": "echo", "arguments": map[string]any{"text": "hi"}},
				})
				if resp.StatusCode != http.StatusOK {
					errs <- fmt.Errorf("client %d call %d code = %d body=%s", c, i, resp.StatusCode, raw)
					return
				}
				msg := parseResp(t, raw)
				if msg["id"] != float64(reqID) {
					errs <- fmt.Errorf("client %d call %d response id = %v, want %d", c, i, msg["id"], reqID)
					return
				}
			}
		}(c)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
}

// TestGatewayConfigHotReload: revision 变化 → 旧进程排空, 新定义生效。
func TestGatewayConfigHotReload(t *testing.T) {
	binary := buildFakeServer(t)
	gateway, ts := newTestGateway(t, fakeServerDef(binary))

	initGateway(t, ts.URL, "fake", 1)
	gateway.mu.Lock()
	pool := gateway.pools["fake"]
	gateway.mu.Unlock()
	oldProc := pool.procs[0]

	// 管理员更新定义(revision+1): 下次请求触发排空重建。
	def := fakeServerDef(binary)
	def.Revision = 2
	gateway.catalog = &fakeCatalog{servers: []Server{def}}

	resp, raw := postJSON(t, ts.URL, "fake", map[string]any{"jsonrpc": "2.0", "id": 2, "method": "tools/list"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("hot reload call code = %d body=%s", resp.StatusCode, raw)
	}
	pool.mu.Lock()
	rebuilt := len(pool.procs) == 1 && pool.procs[0] != oldProc
	pool.mu.Unlock()
	if !rebuilt {
		t.Fatal("expected processes rebuilt after revision change")
	}
}

// TestGatewayCircuitBreak: 连续失败达到阈值 → 熔断(503), 按探活间隔
// 才允许重建; 不随每个请求反复 spawn。
func TestGatewayCircuitBreak(t *testing.T) {
	binary := buildFakeServer(t)
	// 无法启动的 server: spawn 即失败, 每次都计一次崩溃。
	def := fakeServerDef(binary)
	def.Command = filepath.Join(t.TempDir(), "does-not-exist")
	gateway, ts := newTestGateway(t, def)

	origThreshold := circuitBreakThreshold
	origProbe := circuitProbeInterval
	circuitBreakThreshold = 3
	circuitProbeInterval = 200 * time.Millisecond
	defer func() {
		circuitBreakThreshold = origThreshold
		circuitProbeInterval = origProbe
	}()

	for i := 0; i < 3; i++ {
		resp, raw := postJSON(t, ts.URL, "fake", initializeReq(1))
		if resp.StatusCode != http.StatusBadGateway {
			t.Fatalf("iteration %d: code = %d, want 502 (body=%s)", i, resp.StatusCode, raw)
		}
	}
	// 达到阈值后: 熔断 503, 而非无休止 spawn 失败。
	resp, _ := postJSON(t, ts.URL, "fake", initializeReq(2))
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("circuit open: code = %d, want 503", resp.StatusCode)
	}
	gateway.mu.Lock()
	pool := gateway.pools["fake"]
	gateway.mu.Unlock()
	pool.mu.Lock()
	spawnAttempts := pool.crashCount
	pool.mu.Unlock()
	if spawnAttempts < circuitBreakThreshold {
		t.Fatalf("crash count %d, want >= %d", spawnAttempts, circuitBreakThreshold)
	}
	// 探活间隔后允许再次尝试(spawn 失败 → 继续熔断)。
	time.Sleep(circuitProbeInterval + 50*time.Millisecond)
	resp, _ = postJSON(t, ts.URL, "fake", initializeReq(3))
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("after probe: code = %d, want 502 (probe attempt fails)", resp.StatusCode)
	}
}

// TestGatewayCleanSubprocessEnv: 子进程环境为白名单, 绝不继承
// gateway 的凭据(DATABASE_URL 等)。
func TestGatewayCleanSubprocessEnv(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://secret:secret@db:5432/ga")
	t.Setenv("PLATFORM_ADMIN_TOKEN", "super-secret")
	binary := buildFakeServer(t)
	proc, err := spawnProcess(fakeServerDef(binary), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer proc.close()
	if proc.cmd.Env == nil {
		t.Fatal("expected explicit whitelist env, got nil")
	}
	joined := strings.Join(proc.cmd.Env, "\n")
	for _, forbidden := range []string{"DATABASE_URL", "PLATFORM_ADMIN_TOKEN", "GA_", "SECRET", "TOKEN"} {
		if strings.Contains(joined, forbidden) {
			t.Fatalf("subprocess env leaks %q: %v", forbidden, proc.cmd.Env)
		}
	}
	for _, required := range []string{"PATH=", "HOME=", "TMPDIR="} {
		found := false
		for _, kv := range proc.cmd.Env {
			if strings.HasPrefix(kv, required) {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("subprocess env missing %s: %v", required, proc.cmd.Env)
		}
	}
}

// TestCatalogFailureFailsClosed: 目录加载失败(DB 不可用)时请求必须失败。
func TestCatalogFailureFailsClosed(t *testing.T) {
	gateway := New(Config{
		Catalog:  &failCatalog{},
		WorkRoot: t.TempDir(),
	})
	ts := httptest.NewServer(gateway.Handler())
	defer ts.Close()
	defer gateway.Close()

	resp, raw := postJSON(t, ts.URL, "fake", initializeReq(1))
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("code = %d, want 503 (body=%s)", resp.StatusCode, raw)
	}
}

// TestGatewayMetrics: 指标端点暴露进程/请求/失败计数。
func TestGatewayMetrics(t *testing.T) {
	binary := buildFakeServer(t)
	gateway, ts := newTestGateway(t, fakeServerDef(binary))
	initGateway(t, ts.URL, "fake", 1)

	resp, err := http.Get(ts.URL + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var metrics map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&metrics); err != nil {
		t.Fatal(err)
	}
	if metrics["requests"].(float64) < 1 {
		t.Fatalf("metrics requests = %v, want >= 1", metrics["requests"])
	}
	if metrics["processes"].(float64) < 1 {
		t.Fatalf("metrics processes = %v, want >= 1", metrics["processes"])
	}
	_ = gateway
}

type failCatalog struct{}

func (c *failCatalog) EnabledServers(ctx context.Context) ([]Server, error) {
	return nil, os.ErrPermission
}
