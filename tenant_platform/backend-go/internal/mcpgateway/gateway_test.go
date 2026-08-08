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

func fakeServerDef(binary string) Server {
	return Server{
		ServerID: "fake",
		Name:     "fake mcp",
		Command:  binary,
		Args:     nil,
		Timeout:  5 * time.Second,
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

func postJSON(t *testing.T, base, path, sessionID string, payload map[string]any) (*http.Response, []byte, string) {
	t.Helper()
	body, _ := json.Marshal(payload)
	req, err := http.NewRequest("POST", base+path, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if sessionID != "" {
		req.Header.Set("Mcp-Session-Id", sessionID)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var buf bytes.Buffer
	_, _ = buf.ReadFrom(resp.Body)
	return resp, buf.Bytes(), resp.Header.Get("Mcp-Session-Id")
}

func parseResp(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var msg map[string]any
	if err := json.Unmarshal(raw, &msg); err != nil {
		t.Fatalf("unmarshal response %q: %v", raw, err)
	}
	return msg
}

// TestGatewayFullFlow: initialize → session → tools/list → tools/call 全链路。
func TestGatewayFullFlow(t *testing.T) {
	binary := buildFakeServer(t)
	_, ts := newTestGateway(t, fakeServerDef(binary))

	resp, raw, sid := postJSON(t, ts.URL, "/v1/mcp/fake", "", map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "initialize",
		"params": map[string]any{"protocolVersion": "2024-11-05", "capabilities": map[string]any{}, "clientInfo": map[string]any{"name": "test", "version": "0"}},
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("initialize code = %d body=%s", resp.StatusCode, raw)
	}
	if sid == "" {
		t.Fatal("initialize response missing Mcp-Session-Id")
	}
	msg := parseResp(t, raw)
	if msg["result"] == nil {
		t.Fatalf("initialize missing result: %s", raw)
	}

	// tools/list
	resp, raw, _ = postJSON(t, ts.URL, "/v1/mcp/fake", sid, map[string]any{
		"jsonrpc": "2.0", "id": 2, "method": "tools/list",
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("tools/list code = %d body=%s", resp.StatusCode, raw)
	}
	tools := parseResp(t, raw)
	if tools["result"] == nil {
		t.Fatalf("tools/list missing result: %s", raw)
	}

	// tools/call
	resp, raw, _ = postJSON(t, ts.URL, "/v1/mcp/fake", sid, map[string]any{
		"jsonrpc": "2.0", "id": 3, "method": "tools/call",
		"params": map[string]any{"name": "echo", "arguments": map[string]any{"text": "hello"}},
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("tools/call code = %d body=%s", resp.StatusCode, raw)
	}
	call := parseResp(t, raw)
	content, _ := call["result"].(map[string]any)
	if content == nil {
		t.Fatalf("tools/call missing result: %s", raw)
	}
}

// TestGatewayUnknownServer: 白名单外的 server_id 一律 404(fail-closed)。
func TestGatewayUnknownServer(t *testing.T) {
	binary := buildFakeServer(t)
	_, ts := newTestGateway(t, fakeServerDef(binary))
	resp, raw, _ := postJSON(t, ts.URL, "/v1/mcp/nope", "", map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "initialize",
	})
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("code = %d, want 404 (body=%s)", resp.StatusCode, raw)
	}
}

// TestGatewayInvalidSession: 未 initialize 直接调用工具必须拒绝。
func TestGatewayInvalidSession(t *testing.T) {
	binary := buildFakeServer(t)
	_, ts := newTestGateway(t, fakeServerDef(binary))
	resp, raw, _ := postJSON(t, ts.URL, "/v1/mcp/fake", "", map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/list",
	})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("code = %d, want 400 (body=%s)", resp.StatusCode, raw)
	}
}

// TestGatewaySharedInitCache: 第二个会话 initialize 复用首个会话初始化的
// 进程与缓存响应(进程只被 initialize 一次)。
func TestGatewaySharedInitCache(t *testing.T) {
	binary := buildFakeServer(t)
	gateway, ts := newTestGateway(t, fakeServerDef(binary))

	_, raw1, sid1 := postJSON(t, ts.URL, "/v1/mcp/fake", "", map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "initialize",
	})
	_, raw2, sid2 := postJSON(t, ts.URL, "/v1/mcp/fake", "", map[string]any{
		"jsonrpc": "2.0", "id": 2, "method": "initialize",
	})
	if sid1 == "" || sid2 == "" || sid1 == sid2 {
		t.Fatalf("sessions: %q %q, want distinct non-empty", sid1, sid2)
	}
	if string(raw1) != string(raw2) {
		t.Fatalf("second initialize should reuse cached response:\n%s\n%s", raw1, raw2)
	}
	// 同一进程被两个 session 复用: 两个 session 都能调工具。
	resp, raw, _ := postJSON(t, ts.URL, "/v1/mcp/fake", sid2, map[string]any{
		"jsonrpc": "2.0", "id": 3, "method": "tools/list",
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("second session tools/list code = %d body=%s", resp.StatusCode, raw)
	}
	gateway.mu.Lock()
	pool := gateway.pools["fake"]
	gateway.mu.Unlock()
	if pool == nil || pool.proc == nil {
		t.Fatal("expected a shared process")
	}
}

// TestGatewayCrashRebuild: 进程崩溃后, 过退避窗口的新请求自动重建进程。
func TestGatewayCrashRebuild(t *testing.T) {
	binary := buildFakeServer(t)
	gateway, ts := newTestGateway(t, fakeServerDef(binary))
	// 覆盖退避窗口(测试加速)。
	origBackoff := DefaultCrashBackoff
	DefaultCrashBackoff = 30 * time.Millisecond
	defer func() { DefaultCrashBackoff = origBackoff }()

	_, raw, sid := postJSON(t, ts.URL, "/v1/mcp/fake", "", map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "initialize",
	})
	if sid == "" {
		t.Fatalf("initialize failed: %s", raw)
	}
	// 杀掉进程模拟崩溃(生产路径: 请求遇死进程 → markDead + noteCrash)。
	gateway.mu.Lock()
	pool := gateway.pools["fake"]
	gateway.mu.Unlock()
	if pool == nil || pool.proc == nil {
		t.Fatal("no process to kill")
	}
	pool.proc.markDead()
	pool.noteCrash()

	// 退避窗口内: 请求失败。
	resp, _, _ := postJSON(t, ts.URL, "/v1/mcp/fake", sid, map[string]any{
		"jsonrpc": "2.0", "id": 2, "method": "tools/list",
	})
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("backoff window: code = %d, want 502", resp.StatusCode)
	}
	time.Sleep(60 * time.Millisecond)
	// 窗口过后: 自动重建。
	resp, raw, _ = postJSON(t, ts.URL, "/v1/mcp/fake", sid, map[string]any{
		"jsonrpc": "2.0", "id": 3, "method": "tools/list",
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("after backoff: code = %d body=%s", resp.StatusCode, raw)
	}
}

// TestGatewayIdleReap: 空闲进程被回收后, 后续请求重建。
func TestGatewayIdleReap(t *testing.T) {
	binary := buildFakeServer(t)
	gateway, ts := newTestGateway(t, fakeServerDef(binary))

	_, _, sid := postJSON(t, ts.URL, "/v1/mcp/fake", "", map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "initialize",
	})
	gateway.mu.Lock()
	pool := gateway.pools["fake"]
	gateway.mu.Unlock()
	if pool == nil || pool.proc == nil {
		t.Fatal("expected process after initialize")
	}
	firstProc := pool.proc
	time.Sleep(80 * time.Millisecond) // > idleTTL(50ms)
	gateway.reap(time.Now())
	gateway.mu.Lock()
	reaped := pool.proc == nil
	gateway.mu.Unlock()
	if !reaped {
		t.Fatal("expected idle process to be reaped")
	}
	// 新请求触发重建(自动重放握手)。session 不应被 reap 误删: 会话 TTL
	// 远长于进程 TTL, 进程可重建而会话由 worker 长期持有。
	resp, raw, _ := postJSON(t, ts.URL, "/v1/mcp/fake", sid, map[string]any{
		"jsonrpc": "2.0", "id": 2, "method": "tools/list",
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("rebuilt call code = %d body=%s", resp.StatusCode, raw)
	}
	gateway.mu.Lock()
	rebuilt := pool.proc != nil && pool.proc != firstProc
	gateway.mu.Unlock()
	if !rebuilt {
		t.Fatal("expected a rebuilt process")
	}
}

// TestGatewayConcurrentCalls: 多会话并发调用串行化到共享进程, 全部成功。
func TestGatewayConcurrentCalls(t *testing.T) {
	binary := buildFakeServer(t)
	_, ts := newTestGateway(t, fakeServerDef(binary))

	const sessions = 4
	const calls = 5
	var wg sync.WaitGroup
	errs := make(chan error, sessions*calls)
	for s := 0; s < sessions; s++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _, sid := postJSON(t, ts.URL, "/v1/mcp/fake", "", map[string]any{
				"jsonrpc": "2.0", "id": s + 1, "method": "initialize",
			})
			if sid == "" {
				errs <- fmt.Errorf("initialize missing session")
				return
			}
			for c := 0; c < calls; c++ {
				resp, raw, _ := postJSON(t, ts.URL, "/v1/mcp/fake", sid, map[string]any{
					"jsonrpc": "2.0", "id": c + 1, "method": "tools/call",
					"params": map[string]any{"name": "echo", "arguments": map[string]any{"text": "hi"}},
				})
				if resp.StatusCode != http.StatusOK {
					errs <- fmt.Errorf("call %d/%d code = %d body=%s", s, c, resp.StatusCode, raw)
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
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

	resp, raw, _ := postJSON(t, ts.URL, "/v1/mcp/fake", "", map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "initialize",
	})
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("code = %d, want 503 (body=%s)", resp.StatusCode, raw)
	}
}

type failCatalog struct{}

func (c *failCatalog) EnabledServers(ctx context.Context) ([]Server, error) {
	return nil, os.ErrPermission
}
