// Package api provides the loopback-only foundation HTTP surface.
package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strings"

	"github.com/google/uuid"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/application"
	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/domain"
	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/infrastructure/policy"
	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/infrastructure/secret"
)

// MaxRequestBodyBytes caps JSON bodies on user-facing endpoints to prevent
// memory exhaustion from oversized payloads (Slowloris-style).
const MaxRequestBodyBytes int64 = 1 << 20 // 1 MiB

// Server is the loopback HTTP API.
type Server struct {
	svc            application.TaskService
	users          application.UserService
	wechatBinding  application.WechatQRBindingService
	botSvc         application.BotService
	invite         application.InviteService
	personas       application.PersonaService
	router         application.Router
	registry       policy.Registry
	policies       PolicyStore
	bots           BotStore
	llmProviders   LLMProviderStore
	botLifecycle   application.BotLifecycleService
	taskStats      TaskStoreStats
	runtimeProfile RuntimeProfile
	cipher         secret.TokenCipher
	devToken       string
	devUserID      int64
	sessionKey     string
	// webhookSecret is the HMAC-SHA256 key shared with the Bot Poller. When
	// non-empty, /v1/im/webhook rejects requests whose X-Webhook-Signature
	// header doesn't match. Empty = unauthenticated (dev/test only; logs once).
	webhookSecret string
	mux           *http.ServeMux
}

// PolicyStore is the admin-facing port for command/policy management.
type PolicyStore interface {
	ListAllCommands(ctx context.Context) ([]domain.PlatformCommand, error)
	UpdateCommand(ctx context.Context, id int64, action domain.CommandAction,
		helpText string, enabled bool, sortOrder int, updatedBy int64) (domain.PlatformCommand, error)
	ListToolPolicies(ctx context.Context) ([]domain.ToolPolicy, error)
	CreateToolPolicy(ctx context.Context, version, description string,
		allowedTools []string, createdBy int64) (domain.ToolPolicy, error)
	UpdateUserToolPolicy(ctx context.Context, userID int64, version string, updatedBy int64) error
}

// BotStore creates and resolves bot records with encrypted tokens.
type BotStore interface {
	CreateBot(ctx context.Context, ilinkBotID string, ownerID int64, tokenCiphertext []byte) (domain.Bot, error)
	GetBotByUUID(ctx context.Context, botUUID string) (domain.Bot, error)
	GetBotByIlinkBotID(ctx context.Context, ilinkBotID string) (domain.Bot, error)
}

// LLMProviderStore is the admin-facing port for configuring upstream LLMs.
type LLMProviderStore interface {
	CreateProvider(ctx context.Context, input domain.LLMProviderCreate) (domain.LLMProvider, error)
	GetProvider(ctx context.Context, id int64) (domain.LLMProvider, error)
	ListProviders(ctx context.Context) ([]domain.LLMProvider, error)
	UpdateProvider(ctx context.Context, id int64, input domain.LLMProviderUpdate) (domain.LLMProvider, error)
	SetProviderState(ctx context.Context, id int64, state domain.LLMProviderState) (domain.LLMProvider, error)
	SetDefaultProvider(ctx context.Context, id int64) error
	DeleteProvider(ctx context.Context, id int64) error
}

// Cipher is the interface for encrypting/decrypting sensitive data.
type Cipher interface {
	Encrypt(plaintext []byte) (ciphertext []byte, version int, err error)
	Decrypt(ciphertext []byte, version int) (plaintext []byte, err error)
}

// ServerConfig configures the foundation API.
type RuntimeProfile struct {
	ClaimLeaseSeconds       int `json:"claim_lease_seconds"`
	TokenTTLSeconds         int `json:"token_ttl_seconds"`
	TokenRefreshSkewSeconds int `json:"token_refresh_skew_seconds"`
	MaxTaskWallClockSeconds int `json:"max_task_wall_clock_seconds"`
	TaskTimeoutSeconds      int `json:"task_timeout_seconds"`
	TaskIdleTimeoutSeconds  int `json:"task_idle_timeout_seconds"`
	WorkerIdleTTLSeconds    int `json:"worker_idle_ttl_seconds"`
	MaxRunningTasks         int `json:"max_running_tasks"`
	PerTenantRunningLimit   int `json:"per_tenant_running_limit"`
	PerUserQueueLimit       int `json:"per_user_queue_limit"`
}

type ServerConfig struct {
	Service        application.TaskService
	Users          application.UserService
	WechatBinding  application.WechatQRBindingService
	BotService     application.BotService
	Invite         application.InviteService
	Personas       application.PersonaService
	Router         application.Router
	Registry       policy.Registry
	Policies       PolicyStore
	Bots           BotStore
	LLMProviders   LLMProviderStore
	BotLifecycle   application.BotLifecycleService
	TaskStats      TaskStoreStats
	RuntimeProfile RuntimeProfile
	Cipher         secret.TokenCipher
	DevToken       string
	DevUserID      int64
	SessionKey     string
	// WebhookSecret, when set, requires Bot Poller requests to /v1/im/webhook
	// to carry a valid X-Webhook-Signature header (HMAC-SHA256 over body).
	WebhookSecret string
}

// NewServer constructs handlers. Bind address enforcement is the caller's responsibility (127.0.0.1).
func NewServer(cfg ServerConfig) (*Server, error) {
	if cfg.Service == nil || cfg.Registry == nil {
		return nil, fmt.Errorf("service and registry required")
	}
	if strings.TrimSpace(cfg.DevToken) == "" {
		return nil, fmt.Errorf("dev token required")
	}
	if cfg.DevUserID <= 0 {
		return nil, fmt.Errorf("dev user id required")
	}
	if strings.TrimSpace(cfg.SessionKey) == "" {
		cfg.SessionKey = fmt.Sprintf("personal:%d", cfg.DevUserID)
	}
	s := &Server{
		svc:            cfg.Service,
		users:          cfg.Users,
		wechatBinding:  cfg.WechatBinding,
		botSvc:         cfg.BotService,
		invite:         cfg.Invite,
		personas:       cfg.Personas,
		router:         cfg.Router,
		registry:       cfg.Registry,
		policies:       cfg.Policies,
		bots:           cfg.Bots,
		llmProviders:   cfg.LLMProviders,
		botLifecycle:   cfg.BotLifecycle,
		taskStats:      cfg.TaskStats,
		runtimeProfile: cfg.RuntimeProfile,
		cipher:         cfg.Cipher,
		devToken:       cfg.DevToken,
		devUserID:      cfg.DevUserID,
		sessionKey:     cfg.SessionKey,
		webhookSecret:  cfg.WebhookSecret,
		mux:            http.NewServeMux(),
	}
	s.mux.HandleFunc("GET /healthz", s.handleHealthz)
	s.mux.HandleFunc("POST /v1/sessions/{session_key}/tasks", s.auth(s.handleCreateTask))
	s.mux.HandleFunc("GET /v1/tasks/{task_id}", s.auth(s.handleGetTask))
	s.mux.HandleFunc("GET /v1/tasks/{task_id}/result", s.auth(s.handleGetResult))
	s.mux.HandleFunc("POST /v1/tasks/{task_id}/cancel", s.auth(s.handleCancel))
	s.registerLifecycleRoutes()
	return s, nil
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
	return http.ListenAndServe(addr, s.mux)
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
