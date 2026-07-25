// Package api provides the loopback-only foundation HTTP surface.
package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/application"
	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/domain"
	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/policy"
	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/postgres"
	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/secret"
)

// Server is the loopback HTTP API.
type Server struct {
	svc        application.TaskService
	users      application.UserService
	binding    application.BindingService
	router     application.Router
	registry   policy.Registry
	policies   PolicyStore
	bots       BotStore
	cipher     secret.TokenCipher
	devToken   string
	devUserID  int64
	sessionKey string
	mux        *http.ServeMux
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
	CreateBot(ctx context.Context, botUUID string, ownerID int64, tokenCiphertext []byte) (domain.Bot, error)
	GetBotByUUID(ctx context.Context, botUUID string) (domain.Bot, error)
}

// ServerConfig configures the foundation API.
type ServerConfig struct {
	Service    application.TaskService
	Users      application.UserService
	Binding    application.BindingService
	Router     application.Router
	Registry   policy.Registry
	Policies   PolicyStore
	Bots       BotStore
	Cipher     secret.TokenCipher
	DevToken   string
	DevUserID  int64
	SessionKey string
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
		svc:        cfg.Service,
		users:      cfg.Users,
		binding:    cfg.Binding,
		router:     cfg.Router,
		registry:   cfg.Registry,
		policies:   cfg.Policies,
		bots:       cfg.Bots,
		cipher:     cfg.Cipher,
		devToken:   cfg.DevToken,
		devUserID:  cfg.DevUserID,
		sessionKey: cfg.SessionKey,
		mux:        http.NewServeMux(),
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

func (s *Server) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tok := r.Header.Get("X-Platform-Dev-Token")
		if tok == "" || tok != s.devToken {
			writeErr(w, http.StatusUnauthorized, "UNAUTHORIZED", "missing or invalid X-Platform-Dev-Token", traceID())
			return
		}
		next(w, r)
	}
}

func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

type createTaskBody struct {
	MessageID         string   `json:"message_id"`
	SourceInstanceID  string   `json:"source_instance_id"`
	Prompt            string   `json:"prompt"`
	Source            string   `json:"source"`
	PersonaSnapshot   []string `json:"persona_snapshot"`
	ToolPolicyVersion string   `json:"tool_policy_version"`
	RequesterUserID   int64    `json:"requester_user_id,omitempty"`
}

func (s *Server) handleCreateTask(w http.ResponseWriter, r *http.Request) {
	tid := traceID()
	sessionKey := r.PathValue("session_key")
	if strings.TrimSpace(sessionKey) == "" {
		writeErr(w, http.StatusBadRequest, "SESSION_REQUIRED", "session_key is required", tid)
		return
	}
	var body createTaskBody
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "INVALID_JSON", err.Error(), tid)
		return
	}
	if err := validateCreate(body); err != nil {
		writeErr(w, http.StatusBadRequest, "VALIDATION_ERROR", err.Error(), tid)
		return
	}
	if _, err := s.registry.Resolve(application.CapabilityVersion, body.ToolPolicyVersion); err != nil {
		writeErr(w, http.StatusBadRequest, "POLICY_REJECTED", err.Error(), tid)
		return
	}
	requester := s.devUserID
	if body.RequesterUserID > 0 {
		requester = body.RequesterUserID
	}
	task, err := s.svc.SubmitTask(r.Context(), domain.SubmitTaskCommand{
		SessionKey:        sessionKey,
		RequesterUserID:   requester,
		Source:            body.Source,
		SourceInstanceID:  body.SourceInstanceID,
		MessageID:         body.MessageID,
		Prompt:            body.Prompt,
		PersonaSnapshot:   body.PersonaSnapshot,
		ToolPolicyVersion: body.ToolPolicyVersion,
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "SUBMIT_FAILED", err.Error(), tid)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{
		"task_id": task.ID,
		"status":  string(task.Status),
	})
}

func (s *Server) handleGetTask(w http.ResponseWriter, r *http.Request) {
	tid := traceID()
	taskID := r.PathValue("task_id")
	task, err := s.svc.GetTask(r.Context(), taskID)
	if err != nil {
		writeErr(w, http.StatusNotFound, "NOT_FOUND", err.Error(), tid)
		return
	}
	out := map[string]any{
		"task_id":     task.ID,
		"session_key": task.SessionKey,
		"status":      string(task.Status),
	}
	if task.SnapshotID != "" {
		out["snapshot_id"] = task.SnapshotID
	}
	if task.SnapshotChecksum != "" {
		out["snapshot_checksum"] = task.SnapshotChecksum
	}
	if task.ResultRef != "" {
		out["result_ref"] = task.ResultRef
	}
	if task.ResultDigest != "" {
		out["result_digest"] = task.ResultDigest
	}
	if task.TerminalErrorCode != "" {
		out["terminal_error"] = map[string]string{
			"code":         task.TerminalErrorCode,
			"user_message": task.TerminalErrorMessage,
			"trace_id":     task.TerminalErrorTraceID,
		}
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleGetResult(w http.ResponseWriter, r *http.Request) {
	tid := traceID()
	taskID := r.PathValue("task_id")
	// Optional query result_ref must match opaque stored ref when provided; never a host path.
	if ref := r.URL.Query().Get("result_ref"); ref != "" {
		if strings.ContainsAny(ref, `/\`) || strings.Contains(ref, "..") {
			writeErr(w, http.StatusBadRequest, "INVALID_RESULT_REF", "path-like result_ref rejected", tid)
			return
		}
	}
	payload, err := s.svc.ReadResult(r.Context(), taskID)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "RESULT_UNAVAILABLE", err.Error(), tid)
		return
	}
	if q := r.URL.Query().Get("result_ref"); q != "" && q != payload.Ref {
		writeErr(w, http.StatusConflict, "RESULT_REF_MISMATCH", "result_ref does not match committed ref", tid)
		return
	}
	// Plan Task 3 Step 5: result body is text/plain UTF-8; result_digest is
	// sha256 over these exact bytes. OpenAPI declares payload as string.
	writeJSON(w, http.StatusOK, map[string]any{
		"result_ref":    payload.Ref,
		"result_digest": payload.Digest,
		"content_type":  "text/plain; charset=utf-8",
		"payload":       string(payload.Body),
	})
}

func (s *Server) handleCancel(w http.ResponseWriter, r *http.Request) {
	tid := traceID()
	taskID := r.PathValue("task_id")
	requester := s.devUserID
	if q := r.URL.Query().Get("requester_user_id"); q != "" {
		if v, err := strconv.ParseInt(q, 10, 64); err == nil && v > 0 {
			requester = v
		}
	}
	task, err := s.svc.CancelTask(r.Context(), taskID, requester)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "CANCEL_FAILED", err.Error(), tid)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"accepted": true,
		"status":   string(task.Status),
	})
}

func validateCreate(b createTaskBody) error {
	if strings.TrimSpace(b.MessageID) == "" || len(b.MessageID) > postgres.MaxMessageIDLen {
		return fmt.Errorf("message_id is required and must be <= %d bytes", postgres.MaxMessageIDLen)
	}
	if strings.TrimSpace(b.SourceInstanceID) == "" || len(b.SourceInstanceID) > postgres.MaxSourceInstanceLen {
		return fmt.Errorf("source_instance_id is required and must be <= %d bytes", postgres.MaxSourceInstanceLen)
	}
	if strings.TrimSpace(b.Prompt) == "" || len([]byte(b.Prompt)) > postgres.MaxPromptBytes {
		return fmt.Errorf("prompt is required and must be <= %d bytes", postgres.MaxPromptBytes)
	}
	if strings.TrimSpace(b.Source) == "" || !domain.IsValidSource(b.Source) {
		return fmt.Errorf("source must be one of %s|%s", domain.SourceWechat, domain.SourceWeb)
	}
	if b.PersonaSnapshot == nil {
		return fmt.Errorf("persona_snapshot is required")
	}
	if strings.TrimSpace(b.ToolPolicyVersion) == "" || len(b.ToolPolicyVersion) > postgres.MaxToolPolicyVersionLen {
		return fmt.Errorf("tool_policy_version is required and must be <= %d bytes", postgres.MaxToolPolicyVersionLen)
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, code, message, tid string) {
	writeJSON(w, status, map[string]any{
		"code":     code,
		"message":  message,
		"trace_id": tid,
	})
}

func traceID() string {
	return uuid.NewString()
}

// ServeContext starts the server and shuts down on ctx cancel.
func ServeContext(ctx context.Context, addr string, h http.Handler) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return err
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return fmt.Errorf("platform API must bind loopback, got %s", addr)
	}
	srv := &http.Server{Addr: addr, Handler: h}
	errCh := make(chan error, 1)
	go func() { errCh <- srv.ListenAndServe() }()
	select {
	case <-ctx.Done():
		shctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shctx)
		return ctx.Err()
	case err := <-errCh:
		return err
	}
}
