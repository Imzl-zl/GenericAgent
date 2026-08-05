package sandbox

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ManagerServer 是 sandbox-manager 暴露给 Platform 控制面的 HTTP API
// (方案 §7: Platform 不持有 Docker socket)。所有请求都必须携带
// HMAC-SHA256 签名(共享 secret + 时间戳 + nonce), 防重放窗口 ±5 分钟;
// nonce 一次性消费, 同一签名请求不能重放两次。
//
// round12 审查(I4): nonce 状态默认仅进程内(测试), 生产必须经
// NewManagerServerWithNonceState 持久化到 Manager 自有状态卷——Compose 的
// GA_MANAGER_SECRET 稳定不复位, 进程内实现无法覆盖"窗口内重启后重放"。
// 持久化开启时每次消费先落盘(fsync + 原子 rename)再放行, 落盘失败
// fail-closed 拒绝请求。
//
// 端点:
//
//	POST /v1/runners/ensure  创建/复用 Runner(携带 mTLS 材料与控制面环境)
//	DELETE /v1/runners/{name} 销毁 Runner(仅限本 Manager 的 Runner 命名)
//	GET   /v1/runners/{name}  校验 Runner 固定 profile
//	GET   /v1/runners         列出本 Manager 的 Runner
type ManagerServer struct {
	manager *Manager
	secret  []byte
	now     func() time.Time

	// seenNonces 是已消费 nonce 的一次性集合(nonce -> 过期时间)。
	// stateDir 非空时经 nonces.json 持久化(round12 审查 I4)。
	seenNoncesMu sync.Mutex
	seenNonces   map[string]time.Time
	stateDir     string
	nonceFile    string
}

const managerAuthWindow = 5 * time.Minute

// NewManagerServer 构建 Manager 控制面(nonce 状态仅进程内, 测试/开发用;
// 生产必须使用 NewManagerServerWithNonceState 持久化, round12 审查 I4)。
func NewManagerServer(manager *Manager, secret string) (*ManagerServer, error) {
	return NewManagerServerWithNonceState(manager, secret, "")
}

// NewManagerServerWithNonceState 构建 Manager 控制面并持久化已消费 nonce:
// 每次消费先原子落盘(fsync + rename)再放行, 落盘失败拒绝请求(fail-closed);
// 启动时加载既有 nonces.json 并丢弃过期项。stateDir 为空时退化为进程内
// 实现(仅测试/开发, 不覆盖重启窗口)。
func NewManagerServerWithNonceState(manager *Manager, secret, stateDir string) (*ManagerServer, error) {
	if manager == nil {
		return nil, fmt.Errorf("manager is required")
	}
	if len(secret) < 16 {
		return nil, fmt.Errorf("manager control secret must be at least 16 bytes")
	}
	s := &ManagerServer{
		manager:    manager,
		secret:     []byte(secret),
		now:        time.Now,
		seenNonces: make(map[string]time.Time),
	}
	if strings.TrimSpace(stateDir) == "" {
		return s, nil
	}
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return nil, fmt.Errorf("create manager nonce state dir %q: %w", stateDir, err)
	}
	s.stateDir = stateDir
	s.nonceFile = filepath.Join(stateDir, "nonces.json")
	if err := s.loadNoncesLocked(); err != nil {
		return nil, err
	}
	return s, nil
}

// Handler 返回带认证中间件的路由。
func (s *ManagerServer) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/runners/ensure", s.handleEnsure)
	mux.HandleFunc("POST /v1/workspaces/ensure", s.handleEnsureWorkspace)
	mux.HandleFunc("DELETE /v1/runners/{name}", s.handleDestroy)
	mux.HandleFunc("GET /v1/runners/{name}", s.handleInspect)
	mux.HandleFunc("GET /v1/runners", s.handleList)
	return s.authenticate(mux)
}

type ensureRunnerRequest struct {
	WorkspaceKey string            `json:"workspace_key"`
	Generation   uint64            `json:"generation"`
	Env          []string          `json:"env,omitempty"`
	ConfigFiles  map[string][]byte `json:"config_files,omitempty"`
}

type ensureRunnerResponse struct {
	Name        string `json:"name"`
	ContainerID string `json:"container_id"`
	Created     bool   `json:"created"`
}

func (s *ManagerServer) handleEnsure(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 8<<20))
	if err != nil {
		writeManagerError(w, http.StatusBadRequest, "BODY_READ_FAILED", "request body could not be read")
		return
	}
	var req ensureRunnerRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeManagerError(w, http.StatusBadRequest, "INVALID_JSON", "invalid request body")
		return
	}
	runner, created, err := s.manager.EnsureRunner(r.Context(), EnsureRunnerRequest{
		WorkspaceKey: req.WorkspaceKey,
		Generation:   req.Generation,
		Env:          req.Env,
		ConfigFiles:  req.ConfigFiles,
	})
	if err != nil {
		writeManagerError(w, http.StatusConflict, "RUNNER_ENSURE_FAILED", err.Error())
		return
	}
	writeManagerJSON(w, http.StatusOK, ensureRunnerResponse{
		Name: runner.Name, ContainerID: runner.ContainerID, Created: created,
	})
}

func (s *ManagerServer) handleEnsureWorkspace(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeManagerError(w, http.StatusBadRequest, "BODY_READ_FAILED", "request body could not be read")
		return
	}
	var req struct {
		WorkspaceKey string `json:"workspace_key"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		writeManagerError(w, http.StatusBadRequest, "INVALID_JSON", "invalid request body")
		return
	}
	if strings.TrimSpace(req.WorkspaceKey) == "" {
		writeManagerError(w, http.StatusBadRequest, "WORKSPACE_KEY_REQUIRED", "workspace_key is required")
		return
	}
	if err := s.manager.EnsureWorkspace(r.Context(), req.WorkspaceKey); err != nil {
		writeManagerError(w, http.StatusConflict, "WORKSPACE_ENSURE_FAILED", err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *ManagerServer) handleDestroy(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if name == "" {
		writeManagerError(w, http.StatusBadRequest, "NAME_REQUIRED", "runner name is required")
		return
	}
	// 仅允许销毁本 Manager 的 Runner(命名模式或容器 ID + label 校验),
	// 防止控制面凭据泄露后任意 docker rm -f 宿主任意容器(方案 §7)。
	// 接管清理(stale_container_id)以容器 ID 调用本接口, 必须走 label 归属校验。
	if !s.manager.IsRunnerName(name) {
		ok, err := s.manager.IsRunnerContainer(r.Context(), name)
		if err != nil || !ok {
			// round10 审查(B1): 容器已不存在时幂等成功(lease 记录的容器 ID
			// 可能已被其他路径删除)——把"不存在"当作拒绝会让 stale_container_id
			// 清理永久失败。存在但非本 Manager Runner 才拒绝。
			exists, existsErr := s.manager.ContainerExists(r.Context(), name)
			if existsErr == nil && !exists {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			writeManagerError(w, http.StatusBadRequest, "NAME_REJECTED", "not a managed runner")
			return
		}
	}
	if err := s.manager.DestroyRunner(r.Context(), name); err != nil {
		writeManagerError(w, http.StatusConflict, "RUNNER_DESTROY_FAILED", err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *ManagerServer) handleInspect(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if name == "" {
		writeManagerError(w, http.StatusBadRequest, "NAME_REQUIRED", "runner name is required")
		return
	}
	if err := s.manager.Inspect(r.Context(), name); err != nil {
		writeManagerError(w, http.StatusConflict, "RUNNER_INSPECT_FAILED", err.Error())
		return
	}
	// 审查 R5-C6: inspect 同时返回容器 label 中的 workspace hash, 供
	// Platform 销毁路径(重启后按容器 ID)定位 config/ 清理目标。
	if hash, ok, err := s.manager.RunnerWorkspaceHash(r.Context(), name); err == nil && ok {
		writeManagerJSON(w, http.StatusOK, map[string]string{"name": name, "ok": "true", "workspace_hash": hash})
		return
	}
	writeManagerJSON(w, http.StatusOK, map[string]string{"name": name, "ok": "true"})
}

func (s *ManagerServer) handleList(w http.ResponseWriter, r *http.Request) {
	names, err := s.manager.ListRunners(r.Context())
	if err != nil {
		writeManagerError(w, http.StatusConflict, "RUNNER_LIST_FAILED", err.Error())
		return
	}
	writeManagerJSON(w, http.StatusOK, map[string][]string{"runners": names})
}

// authenticate 校验 HMAC 签名: canonical = method + "\n" + path + "\n" +
// timestamp + "\n" + nonce + "\n" + body。
func (s *ManagerServer) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		timestamp := strings.TrimSpace(r.Header.Get("X-GA-Timestamp"))
		nonce := strings.TrimSpace(r.Header.Get("X-GA-Nonce"))
		signature := strings.TrimSpace(r.Header.Get("X-GA-Signature"))
		if timestamp == "" || nonce == "" || signature == "" {
			writeManagerError(w, http.StatusUnauthorized, "AUTH_REQUIRED", "missing manager control headers")
			return
		}
		ts, err := strconv.ParseInt(timestamp, 10, 64)
		if err != nil {
			writeManagerError(w, http.StatusUnauthorized, "AUTH_INVALID", "invalid timestamp")
			return
		}
		now := s.now().Unix()
		if diff := now - ts; diff > int64(managerAuthWindow.Seconds()) || diff < -int64(managerAuthWindow.Seconds()) {
			writeManagerError(w, http.StatusUnauthorized, "AUTH_EXPIRED", "request timestamp outside replay window")
			return
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, 8<<20))
		if err != nil {
			writeManagerError(w, http.StatusBadRequest, "BODY_READ_FAILED", "request body could not be read")
			return
		}
		canonical := strings.Join([]string{r.Method, r.URL.Path, timestamp, nonce, string(body)}, "\n")
		mac := hmac.New(sha256.New, s.secret)
		_, _ = mac.Write([]byte(canonical))
		if !hmac.Equal([]byte(signature), []byte(hex.EncodeToString(mac.Sum(nil)))) {
			writeManagerError(w, http.StatusUnauthorized, "AUTH_MISMATCH", "invalid manager control signature")
			return
		}
		// nonce 一次性消费: 同一 nonce 在窗口内只能使用一次(防重放)。
		// round12 审查(I4): 持久化失败 fail-closed——已消费 nonce 未落盘时
		// 放行会让重启窗口重新打开。
		ok, err := s.consumeNonce(nonce)
		if err != nil {
			writeManagerError(w, http.StatusServiceUnavailable, "NONCE_PERSIST_FAILED", "could not persist consumed nonce")
			return
		}
		if !ok {
			writeManagerError(w, http.StatusUnauthorized, "AUTH_REPLAY", "nonce already used")
			return
		}
		r.Body = io.NopCloser(strings.NewReader(string(body)))
		next.ServeHTTP(w, r)
	})
}

// consumeNonce 记录并消费 nonce; 重复使用返回 false。顺带惰性清理过期项。
// stateDir 配置时先原子持久化再返回 true, 持久化失败返回错误(调用方
// fail-closed 拒绝请求)。
func (s *ManagerServer) consumeNonce(nonce string) (bool, error) {
	s.seenNoncesMu.Lock()
	defer s.seenNoncesMu.Unlock()
	now := s.now()
	for n, expires := range s.seenNonces {
		if now.After(expires) {
			delete(s.seenNonces, n)
		}
	}
	if _, seen := s.seenNonces[nonce]; seen {
		return false, nil
	}
	s.seenNonces[nonce] = now.Add(managerAuthWindow + time.Minute)
	if s.nonceFile != "" {
		if err := s.persistNoncesLocked(); err != nil {
			// 未落盘前不放行: 该 nonce 仍可被重启后的实例接受。
			delete(s.seenNonces, nonce)
			return false, err
		}
	}
	return true, nil
}

// persistNoncesLocked 把已消费 nonce 集合原子写入状态文件
// (临时文件 + fsync + rename, 调用方必须持有 seenNoncesMu)。
func (s *ManagerServer) persistNoncesLocked() error {
	data, err := json.Marshal(s.seenNonces)
	if err != nil {
		return fmt.Errorf("marshal nonces: %w", err)
	}
	tmp := filepath.Join(s.stateDir, ".nonces.tmp")
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("open nonce state tmp: %w", err)
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return fmt.Errorf("write nonce state: %w", err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return fmt.Errorf("sync nonce state: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close nonce state: %w", err)
	}
	if err := os.Rename(tmp, s.nonceFile); err != nil {
		return fmt.Errorf("commit nonce state: %w", err)
	}
	return nil
}

// loadNoncesLocked 启动时加载已持久化的 nonce 集合, 丢弃过期项
// (调用方必须持有 seenNoncesMu)。文件不存在视为空状态; 文件损坏 fail-closed
// 拒绝启动(部署需修复或删除状态文件)。
func (s *ManagerServer) loadNoncesLocked() error {
	data, err := os.ReadFile(s.nonceFile)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("read nonce state %q: %w", s.nonceFile, err)
	}
	var loaded map[string]time.Time
	if err := json.Unmarshal(data, &loaded); err != nil {
		return fmt.Errorf("parse nonce state %q: %w", s.nonceFile, err)
	}
	now := s.now()
	for n, expires := range loaded {
		if now.After(expires) {
			delete(loaded, n)
		}
	}
	s.seenNonces = loaded
	return nil
}

func writeManagerJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeManagerError(w http.ResponseWriter, status int, code, message string) {
	writeManagerJSON(w, status, map[string]string{"code": code, "message": message})
}

// IsNotFound 用于调用方判断容器不存在(与 404/409 区分)。
func IsManagerNotFound(err error) bool {
	return errors.Is(err, errRunnerNotFound)
}

var errRunnerNotFound = errors.New("runner not found")
