package sandbox

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// ManagerClient 是 Platform 侧的 Manager 控制面客户端(方案 §7: Platform
// 不持有 Docker socket, 所有容器操作经认证的 Manager HTTP API)。
type ManagerClient struct {
	baseURL string
	secret  []byte
	http    *http.Client
	prefix  string // Runner 容器名前缀(推导证书 SAN 与拨号地址)
	now     func() time.Time
}

// NewManagerClient 构建 Manager 控制面客户端。
func NewManagerClient(baseURL, secret, containerPrefix string) (*ManagerClient, error) {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if baseURL == "" {
		return nil, fmt.Errorf("manager base URL is required")
	}
	if _, err := url.Parse(baseURL); err != nil {
		return nil, fmt.Errorf("invalid manager base URL: %w", err)
	}
	if len(secret) < 16 {
		return nil, fmt.Errorf("manager control secret must be at least 16 bytes")
	}
	if strings.TrimSpace(containerPrefix) == "" {
		containerPrefix = "ga-runner"
	}
	return &ManagerClient{
		baseURL: baseURL,
		secret:  []byte(secret),
		http:    &http.Client{Timeout: 60 * time.Second},
		prefix:  containerPrefix,
		now:     time.Now,
	}, nil
}

// RunnerName 推导容器名(与 Manager 的 DockerCLI.RunnerName 同构)。
func (c *ManagerClient) RunnerName(workspaceHash string, generation uint64) string {
	return fmt.Sprintf("%s-%s-g%d", c.prefix, workspaceHash[:12], generation)
}

func (c *ManagerClient) EnsureRunner(ctx context.Context, req EnsureRunnerRequest) (Runner, bool, error) {
	payload, err := json.Marshal(ensureRunnerRequest{
		WorkspaceKey: req.WorkspaceKey,
		Generation:   req.Generation,
		Env:          req.Env,
		ConfigFiles:  req.ConfigFiles,
	})
	if err != nil {
		return Runner{}, false, fmt.Errorf("marshal ensure runner request: %w", err)
	}
	var out ensureRunnerResponse
	if err := c.do(ctx, http.MethodPost, "/v1/runners/ensure", payload, &out); err != nil {
		return Runner{}, false, err
	}
	return Runner{ContainerID: out.ContainerID, Name: out.Name}, out.Created, nil
}

func (c *ManagerClient) Destroy(ctx context.Context, name string) error {
	if name == "" || strings.ContainsAny(name, "/\x00") {
		return fmt.Errorf("invalid runner name %q", name)
	}
	return c.do(ctx, http.MethodDelete, "/v1/runners/"+name, nil, nil)
}

func (c *ManagerClient) Inspect(ctx context.Context, name string) error {
	if name == "" || strings.ContainsAny(name, "/\x00") {
		return fmt.Errorf("invalid runner name %q", name)
	}
	return c.do(ctx, http.MethodGet, "/v1/runners/"+name, nil, nil)
}

// CreateAndStart 不允许经远程控制面直接创建(必须走 EnsureRunner 的
// workspace/generation 推导), 返回明确错误而不是静默绕过。
func (c *ManagerClient) CreateAndStart(ctx context.Context, spec RunnerSpec) (Runner, error) {
	return Runner{}, fmt.Errorf("direct CreateAndStart is not allowed through the manager control API; use EnsureRunner")
}

// ListRunners 列出 Manager 当前管理的 Runner 容器名。
func (c *ManagerClient) ListRunners(ctx context.Context) ([]string, error) {
	var out struct {
		Runners []string `json:"runners"`
	}
	if err := c.do(ctx, http.MethodGet, "/v1/runners", nil, &out); err != nil {
		return nil, err
	}
	return out.Runners, nil
}

// do 发送带 HMAC 签名的控制面请求并解析 JSON 响应。
func (c *ManagerClient) do(ctx context.Context, method, path string, body []byte, out any) error {
	nonce := make([]byte, 16)
	if _, err := rand.Read(nonce); err != nil {
		return fmt.Errorf("generate nonce: %w", err)
	}
	timestamp := strconv.FormatInt(c.now().Unix(), 10)
	canonical := strings.Join([]string{method, path, timestamp, hex.EncodeToString(nonce), string(body)}, "\n")
	mac := hmac.New(sha256.New, c.secret)
	_, _ = mac.Write([]byte(canonical))

	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build manager request: %w", err)
	}
	req.Header.Set("X-GA-Timestamp", timestamp)
	req.Header.Set("X-GA-Nonce", hex.EncodeToString(nonce))
	req.Header.Set("X-GA-Signature", hex.EncodeToString(mac.Sum(nil)))
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("manager request %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("read manager response: %w", err)
	}
	if resp.StatusCode == http.StatusNotFound {
		return fmt.Errorf("%w: %s", errRunnerNotFound, path)
	}
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
		var apiErr struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		}
		_ = json.Unmarshal(respBody, &apiErr)
		if apiErr.Message == "" {
			apiErr.Message = strings.TrimSpace(string(respBody))
		}
		return fmt.Errorf("manager %s %s failed (%d %s): %s", method, path, resp.StatusCode, apiErr.Code, apiErr.Message)
	}
	if out != nil && len(respBody) > 0 {
		if err := json.Unmarshal(respBody, out); err != nil {
			return fmt.Errorf("parse manager response: %w", err)
		}
	}
	return nil
}
