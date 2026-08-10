package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/application"
	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/domain"
)

// handleHealthz is the unauthenticated liveness probe.
func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// createTaskBody is the JSON body for POST /v1/sessions/{session_key}/tasks.
type createTaskBody struct {
	MessageID        string   `json:"message_id"`
	SourceInstanceID string   `json:"source_instance_id"`
	Prompt           string   `json:"prompt"`
	Source           string   `json:"source"`
	PersonaSnapshot  []string `json:"persona_snapshot"`
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
	// RequesterUserID is taken from the authenticated principal, never from the
	// request body. The endpoint runs under userAuth (审查 I-4), so the
	// principal is always present; allowing the body to override it would let
	// any caller impersonate arbitrary users.
	requester, ok := userIDFromContext(r.Context())
	if !ok {
		writeErr(w, http.StatusInternalServerError, "NO_PRINCIPAL", "authenticated user missing from context", tid)
		return
	}
	task, err := s.svc.SubmitTask(r.Context(), domain.SubmitTaskCommand{
		SessionKey:        sessionKey,
		RequesterUserID:   requester,
		Source:            body.Source,
		SourceInstanceID:  body.SourceInstanceID,
		MessageID:         body.MessageID,
		Prompt:            body.Prompt,
		PersonaSnapshot:   body.PersonaSnapshot,
		ToolPolicyVersion: application.DefaultToolPolicyVersion,
	})
	if err != nil {
		// Round16-P1: 错误语义分级——队列满 429 / 越权或非法会话 403 /
		// 会话不存在 404; 旧实现全 500, 客户端无法区分可恢复与配置错误。
		switch {
		case errors.Is(err, domain.ErrPerUserQueueFull):
			writeErr(w, http.StatusTooManyRequests, "QUEUE_FULL", err.Error(), tid)
		case errors.Is(err, domain.ErrSessionAccessDenied):
			writeErr(w, http.StatusForbidden, "ACCESS_DENIED", err.Error(), tid)
		case errors.Is(err, domain.ErrWorkspaceNotFound):
			writeErr(w, http.StatusNotFound, "WORKSPACE_NOT_FOUND", err.Error(), tid)
		default:
			writeErr(w, http.StatusInternalServerError, "SUBMIT_FAILED", err.Error(), tid)
		}
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{
		"task_id": task.ID,
		"status":  string(task.Status),
	})
}

// requesterID 返回认证主体 userID(审查 I-4: 任务端点运行于 userAuth,
// principal 必存在; 缺失时返回 0, 由 service 层归属校验统一拒绝)。
func requesterID(r *http.Request) int64 {
	uid, _ := userIDFromContext(r.Context())
	return uid
}

func (s *Server) handleGetTask(w http.ResponseWriter, r *http.Request) {
	tid := traceID()
	taskID := r.PathValue("task_id")
	task, err := s.svc.GetTask(r.Context(), taskID, requesterID(r))
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
	payload, err := s.svc.ReadResult(r.Context(), taskID, requesterID(r))
	if err != nil {
		// 审查 I-4: 归属校验失败统一 404(ErrTaskNotFound), 与 get/cancel 一致。
		if errors.Is(err, application.ErrTaskNotFound) {
			writeErr(w, http.StatusNotFound, "NOT_FOUND", err.Error(), tid)
			return
		}
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
	// 审查 I-2: 请求者身份只取认证主体, 绝不允许 query/body 覆盖——
	// 否则任意调用方可传他人 user_id 取消他人任务(IDOR)。
	// 审查 I-4: 端点运行于 userAuth, requester 恒为认证用户;
	// 归属校验失败统一 404(ErrTaskNotFound), 与 get/result 一致。
	requester := requesterID(r)
	task, err := s.svc.CancelTask(r.Context(), taskID, requester)
	if err != nil {
		if errors.Is(err, application.ErrTaskNotFound) {
			writeErr(w, http.StatusNotFound, "NOT_FOUND", err.Error(), tid)
			return
		}
		writeErr(w, http.StatusBadRequest, "CANCEL_FAILED", err.Error(), tid)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"accepted": true,
		"status":   string(task.Status),
	})
}

// validateCreate enforces non-empty + length limits on createTaskBody fields.
// Limits match the postgres constants used by the store layer.
func validateCreate(b createTaskBody) error {
	if strings.TrimSpace(b.MessageID) == "" || len(b.MessageID) > domain.MaxMessageIDLen {
		return fmt.Errorf("message_id is required and must be <= %d bytes", domain.MaxMessageIDLen)
	}
	if strings.TrimSpace(b.SourceInstanceID) == "" || len(b.SourceInstanceID) > domain.MaxSourceInstanceLen {
		return fmt.Errorf("source_instance_id is required and must be <= %d bytes", domain.MaxSourceInstanceLen)
	}
	if strings.TrimSpace(b.Prompt) == "" || len([]byte(b.Prompt)) > domain.MaxPromptBytes {
		return fmt.Errorf("prompt is required and must be <= %d bytes", domain.MaxPromptBytes)
	}
	if strings.TrimSpace(b.Source) == "" || !domain.IsValidSource(b.Source) {
		return fmt.Errorf("source must be one of %s|%s", domain.SourceWechat, domain.SourceWeb)
	}
	if b.PersonaSnapshot == nil {
		return fmt.Errorf("persona_snapshot is required")
	}
	return nil
}
