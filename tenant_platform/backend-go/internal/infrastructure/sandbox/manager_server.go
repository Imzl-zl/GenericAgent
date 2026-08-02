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
	"strconv"
	"strings"
	"time"
)

// ManagerServer 是 sandbox-manager 暴露给 Platform 控制面的 HTTP API
// (方案 §7: Platform 不持有 Docker socket)。所有请求都必须携带
// HMAC-SHA256 签名(共享 secret + 时间戳 + nonce), 防重放窗口 ±5 分钟。
//
// 端点:
//
//	POST /v1/runners/ensure  创建/复用 Runner(携带 mTLS 材料与控制面环境)
//	DELETE /v1/runners/{name} 销毁 Runner
//	GET   /v1/runners/{name}  校验 Runner 固定 profile
//	GET   /v1/runners         列出本 Manager 的 Runner
type ManagerServer struct {
	manager *Manager
	secret  []byte
	now     func() time.Time
}

const managerAuthWindow = 5 * time.Minute

// NewManagerServer 构建 Manager 控制面。
func NewManagerServer(manager *Manager, secret string) (*ManagerServer, error) {
	if manager == nil {
		return nil, fmt.Errorf("manager is required")
	}
	if len(secret) < 16 {
		return nil, fmt.Errorf("manager control secret must be at least 16 bytes")
	}
	return &ManagerServer{manager: manager, secret: []byte(secret), now: time.Now}, nil
}

// Handler 返回带认证中间件的路由。
func (s *ManagerServer) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/runners/ensure", s.handleEnsure)
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

func (s *ManagerServer) handleDestroy(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if name == "" {
		writeManagerError(w, http.StatusBadRequest, "NAME_REQUIRED", "runner name is required")
		return
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
		r.Body = io.NopCloser(strings.NewReader(string(body)))
		next.ServeHTTP(w, r)
	})
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
