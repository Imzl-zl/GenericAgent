package application

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/domain"
	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/infrastructure/policy"
)

var (
	ErrDocumentToolForbidden = errors.New("document tool is not allowed by task policy")
	ErrDocumentToolInvalid   = errors.New("document tool request is invalid")
)

type DocumentToolPrincipal struct {
	SessionKey  string
	WorkspaceID string
}

type DocumentToolCommandRequest struct {
	TaskID    string
	ToolName  string
	RequestID string
	Operation domain.DocumentOperationRequest
}

type DocumentToolStore interface {
	GetTask(ctx context.Context, taskID string) (domain.Task, error)
	SubmitDocumentToolCommand(ctx context.Context, cmd domain.SubmitDocumentToolCommand) (domain.DocumentToolSubmission, error)
	CloseDocumentToolJob(ctx context.Context, scope domain.DocumentToolTaskScope) (domain.DocumentJob, error)
	GetDocumentToolStatus(ctx context.Context, scope domain.DocumentToolTaskScope, requestID string) (domain.DocumentToolStatus, error)
	GetDocumentToolArtifact(ctx context.Context, scope domain.DocumentToolTaskScope, requestID string) (domain.DocumentArtifact, error)
}

type DocumentToolService interface {
	SubmitCommand(ctx context.Context, principal DocumentToolPrincipal, request DocumentToolCommandRequest) (domain.DocumentToolSubmission, error)
	Close(ctx context.Context, principal DocumentToolPrincipal, taskID, toolName string) (domain.DocumentJob, error)
	Status(ctx context.Context, principal DocumentToolPrincipal, taskID, toolName, requestID string) (domain.DocumentToolStatus, error)
	DownloadArtifact(ctx context.Context, principal DocumentToolPrincipal, taskID, toolName, requestID string) (domain.DocumentArtifact, error)
}

type DocumentToolServiceConfig struct {
	Store    DocumentToolStore
	Registry policy.Registry
}

type documentToolService struct {
	store    DocumentToolStore
	registry policy.Registry
}

func NewDocumentToolService(cfg DocumentToolServiceConfig) (DocumentToolService, error) {
	if cfg.Store == nil {
		return nil, fmt.Errorf("document tool store is required")
	}
	if cfg.Registry == nil {
		return nil, fmt.Errorf("document tool policy registry is required")
	}
	return &documentToolService{store: cfg.Store, registry: cfg.Registry}, nil
}

func (s *documentToolService) SubmitCommand(
	ctx context.Context,
	principal DocumentToolPrincipal,
	request DocumentToolCommandRequest,
) (domain.DocumentToolSubmission, error) {
	request.TaskID = strings.TrimSpace(request.TaskID)
	request.ToolName = strings.TrimSpace(request.ToolName)
	request.RequestID = strings.TrimSpace(request.RequestID)
	if request.RequestID == "" || len(request.RequestID) > 256 {
		return domain.DocumentToolSubmission{}, fmt.Errorf("%w: request_id is required and must be <= 256 bytes", ErrDocumentToolInvalid)
	}
	if err := validateDocumentToolOperation(request.Operation); err != nil {
		return domain.DocumentToolSubmission{}, err
	}
	if !documentToolAllowsOperation(request.ToolName, request.Operation.Operation) {
		return domain.DocumentToolSubmission{}, ErrDocumentToolForbidden
	}
	if _, err := decodeExportDOCXParameters(request.Operation.Parameters); err != nil {
		return domain.DocumentToolSubmission{}, fmt.Errorf("%w: %v", ErrDocumentToolInvalid, err)
	}
	scope, err := s.authorize(ctx, principal, request.TaskID, request.ToolName, true)
	if err != nil {
		return domain.DocumentToolSubmission{}, err
	}
	return s.store.SubmitDocumentToolCommand(ctx, domain.SubmitDocumentToolCommand{
		Scope: scope, RequestID: request.RequestID, Operation: request.Operation,
	})
}

func (s *documentToolService) Close(
	ctx context.Context,
	principal DocumentToolPrincipal,
	taskID, toolName string,
) (domain.DocumentJob, error) {
	if strings.TrimSpace(toolName) != "document_job_close" {
		return domain.DocumentJob{}, ErrDocumentToolForbidden
	}
	scope, err := s.authorize(ctx, principal, taskID, toolName, false)
	if err != nil {
		return domain.DocumentJob{}, err
	}
	return s.store.CloseDocumentToolJob(ctx, scope)
}

func (s *documentToolService) Status(
	ctx context.Context,
	principal DocumentToolPrincipal,
	taskID, toolName, requestID string,
) (domain.DocumentToolStatus, error) {
	scope, err := s.authorize(ctx, principal, taskID, toolName, true)
	if err != nil {
		return domain.DocumentToolStatus{}, err
	}
	requestID = strings.TrimSpace(requestID)
	if len(requestID) > 256 {
		return domain.DocumentToolStatus{}, fmt.Errorf("%w: request_id must be <= 256 bytes", ErrDocumentToolInvalid)
	}
	return s.store.GetDocumentToolStatus(ctx, scope, requestID)
}

func (s *documentToolService) DownloadArtifact(
	ctx context.Context,
	principal DocumentToolPrincipal,
	taskID, toolName, requestID string,
) (domain.DocumentArtifact, error) {
	toolName = strings.TrimSpace(toolName)
	if toolName != "export_docx" && toolName != "document_job_submit" {
		return domain.DocumentArtifact{}, ErrDocumentToolForbidden
	}
	requestID = strings.TrimSpace(requestID)
	if requestID == "" || len(requestID) > 256 {
		return domain.DocumentArtifact{}, fmt.Errorf("%w: request_id is required and must be <= 256 bytes", ErrDocumentToolInvalid)
	}
	scope, err := s.authorize(ctx, principal, taskID, toolName, true)
	if err != nil {
		return domain.DocumentArtifact{}, err
	}
	return s.store.GetDocumentToolArtifact(ctx, scope, requestID)
}

func (s *documentToolService) authorize(
	ctx context.Context,
	principal DocumentToolPrincipal,
	taskID, toolName string,
	requireActive bool,
) (domain.DocumentToolTaskScope, error) {
	taskID = strings.TrimSpace(taskID)
	toolName = strings.TrimSpace(toolName)
	principal.SessionKey = strings.TrimSpace(principal.SessionKey)
	principal.WorkspaceID = strings.TrimSpace(principal.WorkspaceID)
	if taskID == "" || principal.SessionKey == "" || principal.WorkspaceID == "" {
		return domain.DocumentToolTaskScope{}, domain.ErrDocumentUnauthorized
	}
	task, err := s.store.GetTask(ctx, taskID)
	if err != nil {
		return domain.DocumentToolTaskScope{}, err
	}
	if task.SessionKey != principal.SessionKey || task.WorkspaceID != principal.WorkspaceID {
		return domain.DocumentToolTaskScope{}, domain.ErrDocumentUnauthorized
	}
	if requireActive && task.Status != domain.TaskRunning {
		return domain.DocumentToolTaskScope{}, domain.ErrDocumentTaskInactive
	}
	if !isDocumentToolName(toolName) {
		return domain.DocumentToolTaskScope{}, ErrDocumentToolForbidden
	}
	resolved, err := s.registry.Resolve(CapabilityVersion, task.ToolPolicyVersion)
	if err != nil {
		return domain.DocumentToolTaskScope{}, fmt.Errorf("resolve task tool policy: %w", err)
	}
	allowed := false
	for _, candidate := range resolved.AllowedTools {
		if candidate == toolName {
			allowed = true
			break
		}
	}
	if !allowed {
		return domain.DocumentToolTaskScope{}, ErrDocumentToolForbidden
	}
	return domain.DocumentToolTaskScope{
		TaskID: task.ID, SessionKey: task.SessionKey, WorkspaceID: task.WorkspaceID,
	}, nil
}

func isDocumentToolName(name string) bool {
	switch name {
	case "export_docx", "document_job_submit", "document_job_close":
		return true
	default:
		return false
	}
}

func documentToolAllowsOperation(toolName, operation string) bool {
	switch strings.TrimSpace(toolName) {
	case "export_docx", "document_job_submit":
		return strings.TrimSpace(operation) == "export_docx"
	default:
		return false
	}
}

func validateDocumentToolOperation(operation domain.DocumentOperationRequest) error {
	operation.Operation = strings.TrimSpace(operation.Operation)
	if operation.SchemaVersion != 1 || operation.Operation == "" || len(operation.Operation) > 128 {
		return fmt.Errorf("%w: operation requires schema_version 1 and a name <= 128 bytes", ErrDocumentToolInvalid)
	}
	if len(operation.Parameters) == 0 || !json.Valid(operation.Parameters) {
		return fmt.Errorf("%w: operation parameters must be valid JSON", ErrDocumentToolInvalid)
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(operation.Parameters, &object); err != nil || object == nil {
		return fmt.Errorf("%w: operation parameters must be a JSON object", ErrDocumentToolInvalid)
	}
	return nil
}
