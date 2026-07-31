package api

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"mime"
	"net/http"
	"strconv"
	"strings"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/application"
	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/domain"
)

const maxDocumentCapabilityTokenBytes = 4096

type documentCommandBody struct {
	TaskID    string                          `json:"task_id"`
	ToolName  string                          `json:"tool_name"`
	RequestID string                          `json:"request_id"`
	Operation domain.DocumentOperationRequest `json:"operation"`
}

type documentTaskBody struct {
	TaskID   string `json:"task_id"`
	ToolName string `json:"tool_name"`
}

func (s *Server) registerDocumentToolRoutes() {
	s.mux.HandleFunc("POST /v1/document/commands", s.documentCapabilityAuth(s.handleDocumentCommand))
	s.mux.HandleFunc("POST /v1/document/close", s.documentCapabilityAuth(s.handleDocumentClose))
	s.mux.HandleFunc("GET /v1/document/status", s.documentCapabilityAuth(s.handleDocumentStatus))
	s.mux.HandleFunc("GET /v1/document/artifact", s.documentCapabilityAuth(s.handleDocumentArtifact))
}

func (s *Server) documentCapabilityAuth(next func(http.ResponseWriter, *http.Request, application.DocumentToolPrincipal)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		header := r.Header.Get("Authorization")
		const prefix = "Bearer "
		if !strings.HasPrefix(header, prefix) {
			writeErr(w, http.StatusUnauthorized, "DOCUMENT_CAPABILITY_REQUIRED", "missing document capability", traceID())
			return
		}
		token := strings.TrimSpace(header[len(prefix):])
		if token == "" || len(token) > maxDocumentCapabilityTokenBytes {
			writeErr(w, http.StatusUnauthorized, "DOCUMENT_CAPABILITY_INVALID", "invalid document capability", traceID())
			return
		}
		claims, err := s.documentCapabilityValidator.Validate(token)
		if err != nil {
			writeErr(w, http.StatusUnauthorized, "DOCUMENT_CAPABILITY_INVALID", "invalid document capability", traceID())
			return
		}
		next(w, r, application.DocumentToolPrincipal{
			SessionKey: claims.Subject, WorkspaceID: claims.WorkspaceID,
		})
	}
}

func (s *Server) handleDocumentCommand(
	w http.ResponseWriter,
	r *http.Request,
	principal application.DocumentToolPrincipal,
) {
	tid := traceID()
	var body documentCommandBody
	if err := decodeStrict(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "DOCUMENT_REQUEST_INVALID", "invalid document command request", tid)
		return
	}
	submission, err := s.documentTools.SubmitCommand(r.Context(), principal, application.DocumentToolCommandRequest{
		TaskID: body.TaskID, ToolName: body.ToolName, RequestID: body.RequestID, Operation: body.Operation,
	})
	if err != nil {
		writeDocumentToolError(w, err, tid)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{
		"job":     documentJobResponse(submission.Job),
		"command": documentCommandResponse(submission.Command),
	})
}

func (s *Server) handleDocumentClose(
	w http.ResponseWriter,
	r *http.Request,
	principal application.DocumentToolPrincipal,
) {
	tid := traceID()
	var body documentTaskBody
	if err := decodeStrict(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "DOCUMENT_REQUEST_INVALID", "invalid document close request", tid)
		return
	}
	job, err := s.documentTools.Close(r.Context(), principal, body.TaskID, body.ToolName)
	if err != nil {
		writeDocumentToolError(w, err, tid)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"job": documentJobResponse(job)})
}

func (s *Server) handleDocumentStatus(
	w http.ResponseWriter,
	r *http.Request,
	principal application.DocumentToolPrincipal,
) {
	tid := traceID()
	status, err := s.documentTools.Status(
		r.Context(), principal,
		strings.TrimSpace(r.URL.Query().Get("task_id")),
		strings.TrimSpace(r.URL.Query().Get("tool_name")),
		strings.TrimSpace(r.URL.Query().Get("request_id")),
	)
	if err != nil {
		writeDocumentToolError(w, err, tid)
		return
	}
	response := map[string]any{"job": documentJobResponse(status.Job)}
	if status.Command != nil {
		response["command"] = documentCommandResponse(*status.Command)
	}
	writeJSON(w, http.StatusOK, response)
}

func (s *Server) handleDocumentArtifact(
	w http.ResponseWriter,
	r *http.Request,
	principal application.DocumentToolPrincipal,
) {
	tid := traceID()
	artifact, err := s.documentTools.DownloadArtifact(
		r.Context(), principal,
		strings.TrimSpace(r.URL.Query().Get("task_id")),
		strings.TrimSpace(r.URL.Query().Get("tool_name")),
		strings.TrimSpace(r.URL.Query().Get("request_id")),
	)
	if err != nil {
		writeDocumentToolError(w, err, tid)
		return
	}
	if err := validateDocumentArtifactResponse(artifact); err != nil {
		writeDocumentToolError(w, err, tid)
		return
	}
	w.Header().Set("Content-Type", artifact.MediaType)
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": artifact.FileName}))
	w.Header().Set("Content-Length", strconv.Itoa(len(artifact.Content)))
	w.Header().Set("X-Content-SHA256", artifact.SHA256)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(artifact.Content)
}

func validateDocumentArtifactResponse(artifact domain.DocumentArtifact) error {
	if len(artifact.Content) == 0 || len(artifact.Content) > domain.MaxDocumentArtifactBytes || artifact.SizeBytes != int64(len(artifact.Content)) {
		return errors.New("stored document artifact size is invalid")
	}
	digest := sha256.Sum256(artifact.Content)
	if artifact.SHA256 != hex.EncodeToString(digest[:]) {
		return errors.New("stored document artifact digest is invalid")
	}
	if err := domain.ValidateDocumentArtifactMetadata(artifact.FileName, artifact.MediaType); err != nil {
		return fmt.Errorf("stored document artifact metadata is invalid: %w", err)
	}
	return nil
}

func writeDocumentToolError(w http.ResponseWriter, err error, tid string) {
	status, code, message := http.StatusInternalServerError, "DOCUMENT_INTERNAL", "document gateway request failed"
	switch {
	case errors.Is(err, domain.ErrDocumentUnauthorized), errors.Is(err, application.ErrDocumentToolForbidden):
		status, code, message = http.StatusForbidden, "DOCUMENT_FORBIDDEN", "document tool is not authorized"
	case errors.Is(err, application.ErrDocumentToolInvalid):
		status, code, message = http.StatusBadRequest, "DOCUMENT_REQUEST_INVALID", "invalid document request"
	case errors.Is(err, domain.ErrDocumentTaskInactive), errors.Is(err, domain.ErrDocumentCommandsClosed), errors.Is(err, domain.ErrDocumentJobState):
		status, code, message = http.StatusConflict, "DOCUMENT_STATE_CONFLICT", "document request conflicts with current state"
	case errors.Is(err, domain.ErrDocumentJobNotFound), errors.Is(err, domain.ErrDocumentCommandNotFound), errors.Is(err, domain.ErrDocumentArtifactNotFound):
		status, code, message = http.StatusNotFound, "DOCUMENT_JOB_NOT_FOUND", "document job or command not found"
	case errors.Is(err, domain.ErrDocumentGlobalQueueFull), errors.Is(err, domain.ErrDocumentWorkspaceQueueFull):
		status, code, message = http.StatusTooManyRequests, "DOCUMENT_QUEUE_FULL", "document queue is full"
	case errors.Is(err, domain.ErrDocumentPoolDisabled):
		status, code, message = http.StatusServiceUnavailable, "DOCUMENT_POOL_DISABLED", "document processing is disabled"
	case errors.Is(err, domain.ErrDocumentIdempotencyConflict):
		status, code, message = http.StatusConflict, "DOCUMENT_IDEMPOTENCY_CONFLICT", "document request id conflicts with prior payload"
	}
	writeErr(w, status, code, message, tid)
}

func documentJobResponse(job domain.DocumentJob) map[string]any {
	response := map[string]any{
		"status": string(job.Status), "commands_closed": job.CommandsClosedAt != nil,
	}
	if job.TerminalErrorCode != "" {
		response["terminal_error"] = map[string]string{"code": job.TerminalErrorCode}
	}
	return response
}

func documentCommandResponse(command domain.DocumentCommand) map[string]any {
	return map[string]any{"status": string(command.Status)}
}
