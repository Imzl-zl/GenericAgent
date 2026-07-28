// Package api admin user management handlers (spec §5: users lifecycle).
package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/domain"
)

// registerLifecycleRoutes wires admin/binding/router routes when the
// corresponding services are present. Missing services skip registration
// so the foundation task-only API still works in tests that don't need them.
func (s *Server) registerLifecycleRoutes() {
	if s.invite != nil {
		s.mux.HandleFunc("POST /v1/register", s.handleRegister)
		s.mux.HandleFunc("POST /v1/login", s.handleLogin)
	}
	if s.users != nil {
		s.mux.HandleFunc("POST /v1/admin/users", s.auth(s.handleAdminCreateUser))
		s.mux.HandleFunc("POST /v1/admin/users/{user_id}/approve", s.auth(s.handleAdminApproveUser))
		s.mux.HandleFunc("POST /v1/admin/users/{user_id}/block", s.auth(s.handleAdminBlockUser))
		s.mux.HandleFunc("GET /v1/admin/users/pending", s.auth(s.handleAdminListPending))
	}
	if s.invite != nil {
		s.mux.HandleFunc("POST /v1/admin/invite-codes", s.auth(s.handleAdminCreateInviteCode))
		s.mux.HandleFunc("GET /v1/admin/invite-codes", s.auth(s.handleAdminListInviteCodes))
		s.mux.HandleFunc("DELETE /v1/admin/invite-codes/{code}", s.auth(s.handleAdminRevokeInviteCode))
	}
	if s.botSvc != nil {
		s.mux.HandleFunc("GET /v1/users/me/bots", s.userAuth(s.handleGetOwnBot))
	}
	if s.wechatBinding != nil {
		// 普通用户的微信绑定接口
		s.mux.HandleFunc("POST /v1/users/me/wechat-qrcode", s.userAuth(s.handleCreateWechatQRCode))
		s.mux.HandleFunc("GET /v1/users/me/wechat-qrcode/status", s.userAuth(s.handleGetWechatQRCodeStatus))

		// 管理员的微信绑定接口（使用开发者 user_id）
		s.mux.HandleFunc("POST /v1/admin/me/wechat-qrcode", s.auth(s.handleAdminCreateWechatQRCode))
		s.mux.HandleFunc("GET /v1/admin/me/wechat-qrcode/status", s.auth(s.handleAdminGetWechatQRCodeStatus))
	}
	if s.personas != nil {
		s.mux.HandleFunc("GET /v1/personas", s.userAuth(s.handleListPersonas))
		s.mux.HandleFunc("POST /v1/personas", s.userAuth(s.handleCreatePersona))
		s.mux.HandleFunc("GET /v1/personas/{persona_id}", s.userAuth(s.handleGetPersona))
		s.mux.HandleFunc("PUT /v1/personas/{persona_id}", s.userAuth(s.handleUpdatePersona))
		s.mux.HandleFunc("DELETE /v1/personas/{persona_id}", s.userAuth(s.handleDeletePersona))
		s.mux.HandleFunc("POST /v1/personas/{persona_id}/submit", s.userAuth(s.handleSubmitPersona))
		s.mux.HandleFunc("POST /v1/users/me/default-persona", s.userAuth(s.handleSetDefaultPersona))
		s.mux.HandleFunc("GET /v1/admin/personas/pending", s.auth(s.handleAdminListPendingPersonas))
		s.mux.HandleFunc("POST /v1/admin/personas/{persona_id}/approve", s.auth(s.handleAdminApprovePersona))
		s.mux.HandleFunc("POST /v1/admin/personas/{persona_id}/reject", s.auth(s.handleAdminRejectPersona))
	}
	if s.router != nil {
		s.mux.HandleFunc("POST /v1/router/messages", s.auth(s.handleRouterMessage))
		s.mux.HandleFunc("POST /v1/im/webhook", s.handleIMWebhook)
	}
	if s.policies != nil {
		s.mux.HandleFunc("GET /v1/admin/commands", s.auth(s.handleListCommands))
		s.mux.HandleFunc("PUT /v1/admin/commands/{command_id}", s.auth(s.handleUpdateCommand))
		s.mux.HandleFunc("GET /v1/admin/tool-policies", s.auth(s.handleListToolPolicies))
		s.mux.HandleFunc("POST /v1/admin/tool-policies", s.auth(s.handleCreateToolPolicy))
		s.mux.HandleFunc("PUT /v1/admin/users/{user_id}/tool-policy", s.auth(s.handleUpdateUserToolPolicy))
	}
	if s.llmProviders != nil && s.cipher != nil {
		s.mux.HandleFunc("POST /v1/admin/llm-providers", s.auth(s.handleAdminCreateLLMProvider))
		s.mux.HandleFunc("GET /v1/admin/llm-providers", s.auth(s.handleAdminListLLMProviders))
		s.mux.HandleFunc("GET /v1/admin/llm-providers/{provider_id}", s.auth(s.handleAdminGetLLMProvider))
		s.mux.HandleFunc("PUT /v1/admin/llm-providers/{provider_id}", s.auth(s.handleAdminUpdateLLMProvider))
		s.mux.HandleFunc("DELETE /v1/admin/llm-providers/{provider_id}", s.auth(s.handleAdminDeleteLLMProvider))
		s.mux.HandleFunc("POST /v1/admin/llm-providers/{provider_id}/default", s.auth(s.handleAdminSetDefaultLLMProvider))
		s.mux.HandleFunc("POST /v1/admin/llm-providers/{provider_id}/disable", s.auth(s.handleAdminDisableLLMProvider))
		s.mux.HandleFunc("POST /v1/admin/llm-providers/{provider_id}/enable", s.auth(s.handleAdminEnableLLMProvider))

	}
	// Dashboard statistics endpoint
	s.mux.HandleFunc("GET /v1/admin/dashboard/stats", s.auth(s.handleAdminDashboardStats))
}

// TaskStoreStats is a minimal interface for fetching dashboard statistics.
type TaskStoreStats interface {
	CountRunningTasks(ctx context.Context) (int, error)
}

type dashboardStatsResponse struct {
	PendingUsers   int            `json:"pending_users"`
	ApprovedUsers  int            `json:"approved_users"`
	RunningTasks   int            `json:"running_tasks"`
	ActiveWorkers  int            `json:"active_workers"`
	RuntimeProfile RuntimeProfile `json:"runtime_profile"`
}

func (s *Server) handleAdminDashboardStats(w http.ResponseWriter, r *http.Request) {
	stats := dashboardStatsResponse{RuntimeProfile: s.runtimeProfile}

	// 查询待审批用户数
	if s.users != nil {
		if pending, err := s.users.CountPendingUsers(r.Context()); err == nil {
			stats.PendingUsers = pending
		}
		if approved, err := s.users.CountApprovedUsers(r.Context()); err == nil {
			stats.ApprovedUsers = approved
		}
	}

	// 查询运行中任务数
	if s.taskStats != nil {
		if running, err := s.taskStats.CountRunningTasks(r.Context()); err == nil {
			stats.RunningTasks = running
		}
	}

	// TODO: 活跃 Worker 数统计（需要实现 Worker 心跳机制）
	// 目前暂时返回 0
	stats.ActiveWorkers = 0

	writeJSON(w, http.StatusOK, stats)
}

type createUserBody struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

func (s *Server) handleAdminCreateUser(w http.ResponseWriter, r *http.Request) {
	tid := traceID()
	var body createUserBody
	if err := decodeStrict(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "INVALID_JSON", err.Error(), tid)
		return
	}
	user, err := s.users.CreateUser(r.Context(), body.Username, body.Password)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "USER_CREATE_FAILED", err.Error(), tid)
		return
	}
	writeJSON(w, http.StatusCreated, userReply(user))
}

func (s *Server) handleAdminApproveUser(w http.ResponseWriter, r *http.Request) {
	tid := traceID()
	uid, ok := parseUserID(w, r, tid)
	if !ok {
		return
	}
	user, err := s.users.ApproveUser(r.Context(), uid)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "USER_APPROVE_FAILED", err.Error(), tid)
		return
	}
	writeJSON(w, http.StatusOK, userReply(user))
}

func (s *Server) handleAdminBlockUser(w http.ResponseWriter, r *http.Request) {
	tid := traceID()
	uid, ok := parseUserID(w, r, tid)
	if !ok {
		return
	}
	user, err := s.users.BlockUser(r.Context(), uid)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "USER_BLOCK_FAILED", err.Error(), tid)
		return
	}
	writeJSON(w, http.StatusOK, userReply(user))
}

func (s *Server) handleAdminListPending(w http.ResponseWriter, r *http.Request) {
	tid := traceID()
	users, err := s.users.ListPendingUsers(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "LIST_PENDING_FAILED", err.Error(), tid)
		return
	}
	out := make([]map[string]any, 0, len(users))
	for _, u := range users {
		out = append(out, userReply(u))
	}
	writeJSON(w, http.StatusOK, map[string]any{"users": out})
}

func botReply(b domain.Bot) map[string]any {
	return map[string]any{
		"bot_id":        b.ID,
		"bot_uuid":      b.BotUUID,
		"ilink_bot_id":  b.IlinkBotID,
		"ilink_user_id": b.IlinkUserID,
		"baseurl":       b.BaseURL,
		"owner_id":      b.OwnerID,
		"state":         string(b.State),
		"created_at":    b.CreatedAt.UTC().Format(time.RFC3339),
	}
}

// userReply serializes a domain.User into a JSON-friendly map.
func userReply(u domain.User) map[string]any {
	reply := map[string]any{
		"user_id":    u.ID,
		"username":   u.Username,
		"status":     string(u.Status),
		"created_at": u.CreatedAt.UTC().Format(time.RFC3339),
	}
	if u.ApprovedAt != nil {
		reply["approved_at"] = u.ApprovedAt.UTC().Format(time.RFC3339)
	}
	return reply
}

// decodeStrict decodes JSON with DisallowUnknownFields.
func decodeStrict(r *http.Request, v any) error {
	return decodeStrictBytes(readBody(r), v)
}

// decodeStrictBytes decodes JSON with DisallowUnknownFields from a byte slice.
// Split out so webhook handlers can verify an HMAC over the raw bytes before
// parsing.
func decodeStrictBytes(body []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}

// readBody fully drains r.Body and replaces it so downstream handlers can
// still read it. Returns nil on error; the caller surfaces a 400.
func readBody(r *http.Request) []byte {
	if r.Body == nil {
		return nil
	}
	b, err := io.ReadAll(r.Body)
	if err != nil {
		return nil
	}
	_ = r.Body.Close()
	r.Body = io.NopCloser(bytes.NewReader(b))
	return b
}

// parseUserID extracts the {user_id} path value as int64.
func parseUserID(w http.ResponseWriter, r *http.Request, tid string) (int64, bool) {
	raw := r.PathValue("user_id")
	uid, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
	if err != nil || uid <= 0 {
		writeErr(w, http.StatusBadRequest, "INVALID_USER_ID", "user_id must be a positive integer", tid)
		return 0, false
	}
	return uid, true
}

// formatTimePtr formats a *time.Time as RFC3339 or empty string when nil.
func formatTimePtr(t *time.Time) string {
	if t == nil {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}
