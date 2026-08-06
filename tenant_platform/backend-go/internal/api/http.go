// Package api provides the loopback-only foundation HTTP surface.
package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"

	"github.com/google/uuid"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/application"
	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/infrastructure/llmproxy"
	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/infrastructure/policy"
	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/infrastructure/secret"
)

// DefaultMaxRequestBodyBytes caps JSON bodies on user-facing endpoints to prevent
// memory exhaustion from oversized payloads (Slowloris-style).
// 审查: 部署可经 ServerConfig.MaxBodyBytes / PLATFORM_MAX_BODY_BYTES 覆盖。
const DefaultMaxRequestBodyBytes int64 = 1 << 20 // 1 MiB

// Server is the loopback HTTP API.
type Server struct {
	svc                             application.TaskService
	users                           application.UserService
	wechatBinding                   application.WechatQRBindingService
	botSvc                          application.BotService
	invite                          application.InviteService
	personas                        application.PersonaService
	router                          application.Router
	registry                        policy.Registry
	policies                        application.AdminCommandPort
	runtimeSettings                 application.RuntimeSettingsPort
	llmProviders                    application.LLMProviderPort
	mcpServers                      application.MCPServerPort
	botLifecycle                    application.BotLifecycleService
	taskStats                       application.TaskStatsPort
	maxBodyBytes                    int64
	runtimeProfileMu                sync.RWMutex
	runtimeProfile                  RuntimeProfile
	imAggregationRuntime            IMAggregationRuntime
	sophub                          application.SophubService
	sophubProxy                     *WorkerSophubProxy
	cipher                          secret.TokenCipher
	adminToken                        string
	adminUserID                       int64
	sessionKey                      string
	// webhookSecret is the HMAC-SHA256 key shared with the Bot Poller. When
	// non-empty, /v1/im/webhook rejects requests whose X-Webhook-Signature
	// header doesn't match. Empty = unauthenticated (dev/test only; logs once).
	webhookSecret string
	mux           *http.ServeMux
}

// IMAggregationRuntime applies persisted aggregation settings to the live bot
// transport. The Python Poller implements this port.
type IMAggregationRuntime interface {
	ConfigureInboundCoalescing(ctx context.Context, windowMS int) error
}

// Cipher is the interface for encrypting/decrypting sensitive data.
type Cipher interface {
	Encrypt(plaintext []byte) (ciphertext []byte, version int, err error)
	Decrypt(ciphertext []byte, version int) (plaintext []byte, err error)
}

// ServerConfig configures the foundation API.
type RuntimeProfile struct {
	ClaimLeaseSeconds         int `json:"claim_lease_seconds"`
	TokenTTLSeconds           int `json:"token_ttl_seconds"`
	TokenRefreshSkewSeconds   int `json:"token_refresh_skew_seconds"`
	MaxTaskWallClockSeconds   int `json:"max_task_wall_clock_seconds"`
	TaskTimeoutSeconds        int `json:"task_timeout_seconds"`
	TaskIdleTimeoutSeconds    int `json:"task_idle_timeout_seconds"`
	MaxRunningTasks           int `json:"max_running_tasks"`
	PerTenantRunningLimit     int `json:"per_tenant_running_limit"`
	PerUserQueueLimit         int `json:"per_user_queue_limit"`
	IMInboundCoalesceWindowMS int `json:"im_inbound_coalesce_window_ms"`
	AgentMaxTurns             int `json:"agent_max_turns"`
}

type ServerConfig struct {
	Service                         application.TaskService
	Users                           application.UserService
	WechatBinding                   application.WechatQRBindingService
	BotService                      application.BotService
	Invite                          application.InviteService
	Personas                        application.PersonaService
	Router                          application.Router
	Registry                        policy.Registry
	Policies                        application.AdminCommandPort
	RuntimeSettings                 application.RuntimeSettingsPort
	LLMProviders                    application.LLMProviderPort
	MCPServers                      application.MCPServerPort
	BotLifecycle                    application.BotLifecycleService
	TaskStats                       application.TaskStatsPort
	RuntimeProfile                  RuntimeProfile
	IMAggregationRuntime            IMAggregationRuntime
	Sophub                          application.SophubService
	// SophubValidator 校验 Worker → Platform Sophub proxy 的 capability JWT。
	SophubValidator func(ctx context.Context, token string) (llmproxy.CapabilityClaims, error)
	// SophubUsageCounter 按 JTI 原子计量 sophub 代理调用(审查 F10);
	// 为 nil 时代理跳过计量(仅测试), 生产接线 postgres.Store。
	SophubUsageCounter llmproxy.CapabilityUsageCounter
	// SophubProxy 由 NewServer 依据 Sophub+SophubValidator 构造; 非 nil 时注册
	// /v1/worker/sophub/* 端点。
	SophubProxy                     *WorkerSophubProxy
	Cipher                          secret.TokenCipher
	AdminToken                        string
	AdminUserID                       int64
	SessionKey                      string
	// WebhookSecret, when set, requires Bot Poller requests to /v1/im/webhook
	// to carry a valid X-Webhook-Signature header (HMAC-SHA256 over body).
	WebhookSecret string
	// MaxBodyBytes caps request bodies on user-facing endpoints; 0 uses
	// DefaultMaxRequestBodyBytes (1 MiB).
	MaxBodyBytes int64
}

// NewServer constructs handlers. Bind address enforcement is the caller's responsibility (127.0.0.1).
func NewServer(cfg ServerConfig) (*Server, error) {
	if cfg.Service == nil || cfg.Registry == nil {
		return nil, fmt.Errorf("service and registry required")
	}
	if strings.TrimSpace(cfg.AdminToken) == "" {
		return nil, fmt.Errorf("admin token required")
	}
	if cfg.AdminUserID <= 0 {
		return nil, fmt.Errorf("dev user id required")
	}
	if strings.TrimSpace(cfg.SessionKey) == "" {
		cfg.SessionKey = fmt.Sprintf("personal:%d", cfg.AdminUserID)
	}
	// Worker Sophub proxy: 提供 SophubService + capability 校验器时自动接线。
	if cfg.Sophub != nil && cfg.SophubValidator != nil && cfg.SophubProxy == nil {
		var consume func(ctx context.Context, jtiHash [32]byte, maxCalls int64) (bool, error)
		if cfg.SophubUsageCounter != nil {
			consume = cfg.SophubUsageCounter.ConsumeCapabilityCall
		}
		cfg.SophubProxy = NewWorkerSophubProxy(
			cfg.Sophub.Search,
			cfg.Sophub.FetchRemoteSOP,
			cfg.SophubValidator,
			consume,
		)
	}
	s := &Server{
		svc:                             cfg.Service,
		users:                           cfg.Users,
		wechatBinding:                   cfg.WechatBinding,
		botSvc:                          cfg.BotService,
		invite:                          cfg.Invite,
		personas:                        cfg.Personas,
		router:                          cfg.Router,
		registry:                        cfg.Registry,
		policies:                        cfg.Policies,
		runtimeSettings:                 cfg.RuntimeSettings,
		llmProviders:                    cfg.LLMProviders,
		mcpServers:                      cfg.MCPServers,
		botLifecycle:                    cfg.BotLifecycle,
		taskStats:                       cfg.TaskStats,
		runtimeProfile:                  cfg.RuntimeProfile,
		imAggregationRuntime:            cfg.IMAggregationRuntime,
		sophub:                          cfg.Sophub,
		sophubProxy:                     cfg.SophubProxy,
		cipher:                          cfg.Cipher,
		adminToken:                        cfg.AdminToken,
		adminUserID:                       cfg.AdminUserID,
		sessionKey:                      cfg.SessionKey,
		webhookSecret:                   cfg.WebhookSecret,
		maxBodyBytes:                    cfg.MaxBodyBytes,
		mux:                             http.NewServeMux(),
	}
	s.mux.HandleFunc("GET /healthz", s.handleHealthz)
	s.mux.HandleFunc("POST /v1/sessions/{session_key}/tasks", s.userAuth(s.handleCreateTask))
	s.mux.HandleFunc("GET /v1/tasks/{task_id}", s.userAuth(s.handleGetTask))
	s.mux.HandleFunc("GET /v1/tasks/{task_id}/result", s.userAuth(s.handleGetResult))
	s.mux.HandleFunc("POST /v1/tasks/{task_id}/cancel", s.userAuth(s.handleCancel))
	s.registerLifecycleRoutes()
	return s, nil
}

func (s *Server) runtimeProfileSnapshot() RuntimeProfile {
	s.runtimeProfileMu.RLock()
	defer s.runtimeProfileMu.RUnlock()
	return s.runtimeProfile
}

func (s *Server) updateRuntimeProfile(update func(*RuntimeProfile)) {
	s.runtimeProfileMu.Lock()
	defer s.runtimeProfileMu.Unlock()
	update(&s.runtimeProfile)
}

// SessionKey returns the default bootstrapped session (used by smoke tooling).
func (s *Server) SessionKey() string { return s.sessionKey }

// Handler returns the root handler.
func (s *Server) Handler() http.Handler { return s.mux }

// ListenAndServe binds only loopback addresses.
func (s *Server) ListenAndServe(addr string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return err
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return fmt.Errorf("platform API must bind loopback, got %s", addr)
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	return serveListener(context.Background(), ln, s.mux, s.maxBodyBytes)
}

// writeJSON encodes v as JSON and writes it with the given status.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// writeErr is the standard error envelope: {code, message, trace_id}.
func writeErr(w http.ResponseWriter, status int, code, message, tid string) {
	writeJSON(w, status, map[string]any{
		"code":     code,
		"message":  message,
		"trace_id": tid,
	})
}

// traceID returns a fresh UUIDv4 for request tracing.
func traceID() string {
	return uuid.NewString()
}
