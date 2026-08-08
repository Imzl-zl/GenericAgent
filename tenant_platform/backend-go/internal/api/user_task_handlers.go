package api

import (
	"net/http"
	"strconv"
	"time"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/domain"
)

// registerUserTaskRoutes wires the user self-service task endpoints. They
// live behind userAuth (Bearer user session) and only ever operate on the
// authenticated user's own tasks — requester identity comes from the session
// context, never from request parameters (tenant isolation invariant).
func (s *Server) registerUserTaskRoutes() {
	if s.users == nil {
		return
	}
	s.mux.HandleFunc("GET /v1/users/me/tasks", s.userAuth(s.handleListMyTasks))
	s.mux.HandleFunc("GET /v1/users/me/task-stats", s.userAuth(s.handleMyTaskStats))
}

func (s *Server) handleListMyTasks(w http.ResponseWriter, r *http.Request) {
	tid := traceID()
	userID, ok := userIDFromContext(r.Context())
	if !ok {
		writeErr(w, http.StatusUnauthorized, "UNAUTHORIZED", "missing user context", tid)
		return
	}
	limit := 20
	if raw := r.URL.Query().Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > 100 {
			writeErr(w, http.StatusBadRequest, "INVALID_LIMIT", "limit must be an integer in [1,100]", tid)
			return
		}
		limit = n
	}
	tasks, err := s.users.ListMyTasks(r.Context(), userID, limit)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "LIST_MY_TASKS_FAILED", err.Error(), tid)
		return
	}
	out := make([]map[string]any, 0, len(tasks))
	for _, t := range tasks {
		out = append(out, userTaskReply(t))
	}
	writeJSON(w, http.StatusOK, map[string]any{"tasks": out})
}

func (s *Server) handleMyTaskStats(w http.ResponseWriter, r *http.Request) {
	tid := traceID()
	userID, ok := userIDFromContext(r.Context())
	if !ok {
		writeErr(w, http.StatusUnauthorized, "UNAUTHORIZED", "missing user context", tid)
		return
	}
	stats, err := s.users.CountMyTaskStats(r.Context(), userID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "MY_TASK_STATS_FAILED", err.Error(), tid)
		return
	}
	// 填零保证响应结构稳定(契约声明全部字段); running 为
	// starting+running 合计(用户视角"正在处理中")。
	reply := map[string]any{
		"queued":      stats[domain.TaskQueued],
		"running":     stats[domain.TaskStarting] + stats[domain.TaskRunning],
		"succeeded":   stats[domain.TaskSucceeded],
		"failed":      stats[domain.TaskFailed],
		"cancelled":   stats[domain.TaskCancelled],
		"interrupted": stats[domain.TaskInterrupted],
	}
	total := 0
	for _, n := range stats {
		total += n
	}
	reply["total"] = total
	writeJSON(w, http.StatusOK, reply)
}

// userTaskReply serializes a domain.Task into the compact self-service
// envelope declared in the OpenAPI contract (UserTask schema).
func userTaskReply(t domain.Task) map[string]any {
	reply := map[string]any{
		"task_id":     t.ID,
		"session_key": t.SessionKey,
		"status":      string(t.Status),
		"source":      t.Source,
		"created_at":  t.CreatedAt.UTC().Format(time.RFC3339),
	}
	if t.StartedAt != nil {
		reply["started_at"] = t.StartedAt.UTC().Format(time.RFC3339)
	}
	if t.TerminalAt != nil {
		reply["terminal_at"] = t.TerminalAt.UTC().Format(time.RFC3339)
	}
	if t.TerminalErrorCode != "" {
		reply["terminal_error_code"] = t.TerminalErrorCode
	}
	return reply
}
