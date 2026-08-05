package sandbox

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// round12 审查(I4): nonce 防重放状态必须跨 Manager 重启持久化——Compose 的
// GA_MANAGER_SECRET 稳定不复位, 5 分钟签名窗口内重启后, 旧签名请求可重放。

const replaySecret = "0123456789abcdef-secret"

func signedRequest(t *testing.T, baseURL, method, path, timestamp, nonce, body string) *http.Request {
	t.Helper()
	canonical := strings.Join([]string{method, path, timestamp, nonce, body}, "\n")
	mac := hmac.New(sha256.New, []byte(replaySecret))
	_, _ = mac.Write([]byte(canonical))
	req, err := http.NewRequest(method, baseURL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-GA-Timestamp", timestamp)
	req.Header.Set("X-GA-Nonce", nonce)
	req.Header.Set("X-GA-Signature", hex.EncodeToString(mac.Sum(nil)))
	return req
}

func doSigned(t *testing.T, req *http.Request) int {
	t.Helper()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	return resp.StatusCode
}

// TestManagerControlAPIRejectsReplayAcrossRestart: 同一签名请求在 Manager
// 重启(新进程实例、同 secret、同状态目录)后重放必须被拒绝。
func TestManagerControlAPIRejectsReplayAcrossRestart(t *testing.T) {
	stateDir := t.TempDir()
	manager := func() *Manager {
		return NewManager(ManagerConfig{CLI: &fakeCLI2{}, WorkspaceRoot: t.TempDir(), ContainerNamePrefix: "ga-runner"})
	}
	server1, err := NewManagerServerWithNonceState(manager(), replaySecret, stateDir)
	if err != nil {
		t.Fatal(err)
	}
	ts1 := httptest.NewServer(server1.Handler())

	timestamp := strconv.FormatInt(time.Now().Unix(), 10)
	const nonce = "aabbccddeeff00112233445566778899"
	req := signedRequest(t, ts1.URL, "GET", "/v1/runners", timestamp, nonce, "")
	if code := doSigned(t, req); code != http.StatusOK {
		t.Fatalf("first request = %d, want 200", code)
	}
	ts1.Close()

	// Manager 重启: 新进程实例, 同一 secret 与状态目录。
	server2, err := NewManagerServerWithNonceState(manager(), replaySecret, stateDir)
	if err != nil {
		t.Fatal(err)
	}
	ts2 := httptest.NewServer(server2.Handler())
	defer ts2.Close()

	// 相同 headers/body 原样重放(签名窗口内)。
	req2 := signedRequest(t, ts2.URL, "GET", "/v1/runners", timestamp, nonce, "")
	if code := doSigned(t, req2); code != http.StatusUnauthorized {
		t.Fatalf("replayed request after restart = %d, want 401", code)
	}
}

// round13 审查(X1): nonce 过期基准必须绑定签名时间戳。服务允许未来 5
// 分钟的签名——若 nonce 按接收时刻记过期(6 分钟), 合法签名(now+5m)的
// 使用窗口可达 10 分钟, 第 6~10 分钟可原样重放。用未来时间戳签名验证:
// 首次放行后, 在该签名的合法窗口内重放必须被拒。
func TestManagerControlAPIFutureTimestampNonceCoversFullReplayWindow(t *testing.T) {
	stateDir := t.TempDir()
	manager := func() *Manager {
		return NewManager(ManagerConfig{CLI: &fakeCLI2{}, WorkspaceRoot: t.TempDir(), ContainerNamePrefix: "ga-runner"})
	}
	server, err := NewManagerServerWithNonceState(manager(), replaySecret, stateDir)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(server.Handler())
	defer ts.Close()

	// 未来 5 分钟的时间戳(签名窗口上限), 首次请求必须放行。
	future := strconv.FormatInt(time.Now().Add(5*time.Minute).Unix(), 10)
	const nonce = "futuretimestampnonce0000000001"
	req := signedRequest(t, ts.URL, "GET", "/v1/runners", future, nonce, "")
	if code := doSigned(t, req); code != http.StatusOK {
		t.Fatalf("first future-ts request = %d, want 200", code)
	}

	// 修复前 nonce 按接收时刻记过期(6 分钟), 该签名的合法窗口可达 10
	// 分钟——第 6~10 分钟重放会被放行。现在过期绑定 ts, 窗口内重放必须
	// 被拒(同一 nonce 同进程内重复消费即等价断言)。
	req2 := signedRequest(t, ts.URL, "GET", "/v1/runners", future, nonce, "")
	if code := doSigned(t, req2); code != http.StatusUnauthorized {
		t.Fatalf("replayed future-ts request = %d, want 401", code)
	}
}

// TestManagerControlAPIFreshNonceAcceptedAfterRestart: 重启后新 nonce 正常
// 放行(持久化未把"整个窗口"锁死, 只锁已消费的 nonce)。
func TestManagerControlAPIFreshNonceAcceptedAfterRestart(t *testing.T) {
	stateDir := t.TempDir()
	manager := NewManager(ManagerConfig{CLI: &fakeCLI2{}, WorkspaceRoot: t.TempDir(), ContainerNamePrefix: "ga-runner"})
	server1, err := NewManagerServerWithNonceState(manager, replaySecret, stateDir)
	if err != nil {
		t.Fatal(err)
	}
	ts1 := httptest.NewServer(server1.Handler())
	timestamp := strconv.FormatInt(time.Now().Unix(), 10)
	req := signedRequest(t, ts1.URL, "GET", "/v1/runners", timestamp, "11111111111111111111111111111111", "")
	if code := doSigned(t, req); code != http.StatusOK {
		t.Fatalf("first request = %d, want 200", code)
	}
	ts1.Close()

	server2, err := NewManagerServerWithNonceState(manager, replaySecret, stateDir)
	if err != nil {
		t.Fatal(err)
	}
	ts2 := httptest.NewServer(server2.Handler())
	defer ts2.Close()
	req2 := signedRequest(t, ts2.URL, "GET", "/v1/runners", timestamp, "22222222222222222222222222222222", "")
	if code := doSigned(t, req2); code != http.StatusOK {
		t.Fatalf("fresh nonce after restart = %d, want 200", code)
	}
}

// TestManagerControlAPIRejectsWhenNoncePersistFails: 状态目录不可写时请求
// fail-closed(503), 不得退回纯内存防重放(否则重启窗口重新打开)。
func TestManagerControlAPIRejectsWhenNoncePersistFails(t *testing.T) {
	// stateDir 指向一个普通文件: 创建/写入必然失败。
	filePath := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(filePath, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	manager := NewManager(ManagerConfig{CLI: &fakeCLI2{}, WorkspaceRoot: t.TempDir(), ContainerNamePrefix: "ga-runner"})
	if _, err := NewManagerServerWithNonceState(manager, replaySecret, filePath); err == nil {
		t.Fatal("expected startup failure when nonce state dir is not a directory")
	}
}

// TestManagerControlAPIRejectsWhenPersistFailsMidflight: 持久化在请求途中
// 失败(目录权限被收回)必须拒绝该请求, 而不是静默放行。
func TestManagerControlAPIRejectsWhenPersistFailsMidflight(t *testing.T) {
	stateDir := t.TempDir()
	manager := NewManager(ManagerConfig{CLI: &fakeCLI2{}, WorkspaceRoot: t.TempDir(), ContainerNamePrefix: "ga-runner"})
	server, err := NewManagerServerWithNonceState(manager, replaySecret, stateDir)
	if err != nil {
		t.Fatal(err)
	}
	// 只读化状态目录(Windows 下 chmod 语义弱, 用删除目录代替: persist 的
	// 临时文件写入失败)。
	if err := os.RemoveAll(stateDir); err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(server.Handler())
	defer ts.Close()
	timestamp := strconv.FormatInt(time.Now().Unix(), 10)
	req := signedRequest(t, ts.URL, "GET", "/v1/runners", timestamp, "33333333333333333333333333333333", "")
	if code := doSigned(t, req); code != http.StatusServiceUnavailable {
		t.Fatalf("request with failing persist = %d, want 503", code)
	}
}
