package application

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/domain"
	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/infrastructure/policy"
)

func TestDocumentToolServiceUsesDurableTaskIdentityAndReusesJob(t *testing.T) {
	store := newDocumentToolFakeStore(domain.Task{
		ID: "task-1", SessionKey: "team:docs",
		WorkspaceID: "11111111-1111-1111-1111-111111111111",
		RequesterID: 42, ToolPolicyVersion: "docs.v1", Status: domain.TaskRunning,
	})
	service := newTestDocumentToolService(t, store, []string{"export_docx"})
	principal := DocumentToolPrincipal{
		SessionKey: "team:docs", WorkspaceID: "11111111-1111-1111-1111-111111111111",
	}

	first, err := service.SubmitCommand(context.Background(), principal, DocumentToolCommandRequest{
		TaskID: "task-1", ToolName: "export_docx", RequestID: "tool-call-1",
		Operation: domain.DocumentOperationRequest{
			SchemaVersion: 1, Operation: "export_docx", Parameters: json.RawMessage(`{"output_name":"report.docx","content":"report"}`),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	second, err := service.SubmitCommand(context.Background(), principal, DocumentToolCommandRequest{
		TaskID: "task-1", ToolName: "export_docx", RequestID: "tool-call-2",
		Operation: domain.DocumentOperationRequest{
			SchemaVersion: 1, Operation: "export_docx", Parameters: json.RawMessage(`{"output_name":"appendix.docx","content":"appendix"}`),
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	if first.Job.ID == "" || first.Job.ID != second.Job.ID {
		t.Fatalf("jobs first=%q second=%q", first.Job.ID, second.Job.ID)
	}
	if first.Command.CommandID == second.Command.CommandID || first.Command.CommandID == "" {
		t.Fatalf("command IDs first=%q second=%q", first.Command.CommandID, second.Command.CommandID)
	}
	if store.lastScope.TaskID != "task-1" || store.lastRequester != 42 {
		t.Fatalf("store scope=%+v requester=%d", store.lastScope, store.lastRequester)
	}
}

func TestDocumentToolServiceCommandRequestIsIdempotentAndConflictsOnPayloadChange(t *testing.T) {
	store := newDocumentToolFakeStore(domain.Task{
		ID: "task-1", SessionKey: "personal:42",
		WorkspaceID: "11111111-1111-1111-1111-111111111111",
		RequesterID: 42, ToolPolicyVersion: "docs.v1", Status: domain.TaskRunning,
	})
	service := newTestDocumentToolService(t, store, []string{"document_job_submit"})
	principal := DocumentToolPrincipal{SessionKey: "personal:42", WorkspaceID: store.task.WorkspaceID}
	request := DocumentToolCommandRequest{
		TaskID: "task-1", ToolName: "document_job_submit", RequestID: "stable-call",
		Operation: domain.DocumentOperationRequest{SchemaVersion: 1, Operation: "export_docx", Parameters: json.RawMessage(`{"output_name":"report.docx","content":"hello"}`)},
	}

	first, err := service.SubmitCommand(context.Background(), principal, request)
	if err != nil {
		t.Fatal(err)
	}
	second, err := service.SubmitCommand(context.Background(), principal, request)
	if err != nil {
		t.Fatal(err)
	}
	if first.Command.ID != second.Command.ID || first.Command.CommandID != second.Command.CommandID {
		t.Fatalf("idempotent commands first=%+v second=%+v", first.Command, second.Command)
	}

	request.Operation.Parameters = json.RawMessage(`{"output_name":"changed.docx","content":"hello"}`)
	if _, err := service.SubmitCommand(context.Background(), principal, request); !errors.Is(err, domain.ErrDocumentIdempotencyConflict) {
		t.Fatalf("changed payload err=%v", err)
	}
}

func TestDocumentToolServiceRejectsMismatchedOrInactiveTask(t *testing.T) {
	base := domain.Task{
		ID: "task-1", SessionKey: "team:docs",
		WorkspaceID: "11111111-1111-1111-1111-111111111111",
		RequesterID: 42, ToolPolicyVersion: "docs.v1", Status: domain.TaskRunning,
	}
	request := DocumentToolCommandRequest{
		TaskID: "task-1", ToolName: "export_docx", RequestID: "call-1",
		Operation: domain.DocumentOperationRequest{SchemaVersion: 1, Operation: "export_docx", Parameters: json.RawMessage(`{"content":"hello"}`)},
	}

	for name, tc := range map[string]struct {
		edit func(*domain.Task, *DocumentToolPrincipal)
		want error
	}{
		"session mismatch":   {func(_ *domain.Task, p *DocumentToolPrincipal) { p.SessionKey = "team:other" }, domain.ErrDocumentUnauthorized},
		"workspace mismatch": {func(_ *domain.Task, p *DocumentToolPrincipal) { p.WorkspaceID = "22222222-2222-2222-2222-222222222222" }, domain.ErrDocumentUnauthorized},
		"queued task":        {func(task *domain.Task, _ *DocumentToolPrincipal) { task.Status = domain.TaskQueued }, domain.ErrDocumentTaskInactive},
		"terminal task":      {func(task *domain.Task, _ *DocumentToolPrincipal) { task.Status = domain.TaskSucceeded }, domain.ErrDocumentTaskInactive},
	} {
		t.Run(name, func(t *testing.T) {
			task := base
			principal := DocumentToolPrincipal{SessionKey: task.SessionKey, WorkspaceID: task.WorkspaceID}
			tc.edit(&task, &principal)
			store := newDocumentToolFakeStore(task)
			service := newTestDocumentToolService(t, store, []string{"export_docx"})
			if _, err := service.SubmitCommand(context.Background(), principal, request); !errors.Is(err, tc.want) {
				t.Fatalf("err=%v want=%v", err, tc.want)
			}
			if store.submitCalls != 0 {
				t.Fatalf("submit calls=%d", store.submitCalls)
			}
		})
	}
}

func TestDocumentToolServiceRechecksExactTaskToolPolicy(t *testing.T) {
	store := newDocumentToolFakeStore(domain.Task{
		ID: "task-1", SessionKey: "personal:42",
		WorkspaceID: "11111111-1111-1111-1111-111111111111",
		RequesterID: 42, ToolPolicyVersion: "docs.v1", Status: domain.TaskRunning,
	})
	principal := DocumentToolPrincipal{SessionKey: store.task.SessionKey, WorkspaceID: store.task.WorkspaceID}
	request := DocumentToolCommandRequest{
		TaskID: "task-1", ToolName: "export_docx", RequestID: "call-1",
		Operation: domain.DocumentOperationRequest{SchemaVersion: 1, Operation: "export_docx", Parameters: json.RawMessage(`{"content":"hello"}`)},
	}

	service := newTestDocumentToolService(t, store, []string{"file_read"})
	if _, err := service.SubmitCommand(context.Background(), principal, request); !errors.Is(err, ErrDocumentToolForbidden) {
		t.Fatalf("missing policy err=%v", err)
	}
	request.ToolName = "file_read"
	if _, err := service.SubmitCommand(context.Background(), principal, request); !errors.Is(err, ErrDocumentToolForbidden) {
		t.Fatalf("non-document tool err=%v", err)
	}
	if store.submitCalls != 0 {
		t.Fatalf("submit calls=%d", store.submitCalls)
	}
}

func TestDocumentToolServiceBindsToolToAllowedOperation(t *testing.T) {
	store := newDocumentToolFakeStore(domain.Task{
		ID: "task-1", SessionKey: "personal:42",
		WorkspaceID: "11111111-1111-1111-1111-111111111111",
		RequesterID: 42, ToolPolicyVersion: "docs.v1", Status: domain.TaskRunning,
	})
	principal := DocumentToolPrincipal{SessionKey: store.task.SessionKey, WorkspaceID: store.task.WorkspaceID}
	request := DocumentToolCommandRequest{
		TaskID: "task-1", ToolName: "export_docx", RequestID: "call-1",
		Operation: domain.DocumentOperationRequest{SchemaVersion: 1, Operation: "convert_pdf", Parameters: json.RawMessage(`{}`)},
	}
	service := newTestDocumentToolService(t, store, []string{"export_docx", "document_job_close"})
	if _, err := service.SubmitCommand(context.Background(), principal, request); !errors.Is(err, ErrDocumentToolForbidden) {
		t.Fatalf("export_docx operation err=%v", err)
	}
	request.ToolName = "document_job_close"
	request.Operation.Operation = "export_docx"
	if _, err := service.SubmitCommand(context.Background(), principal, request); !errors.Is(err, ErrDocumentToolForbidden) {
		t.Fatalf("close command err=%v", err)
	}
	if store.submitCalls != 0 {
		t.Fatalf("submit calls=%d", store.submitCalls)
	}
}

func TestDocumentToolServiceRejectsUnknownOrMalformedOperationBeforeStore(t *testing.T) {
	store := newDocumentToolFakeStore(domain.Task{
		ID: "task-1", SessionKey: "personal:42",
		WorkspaceID: "11111111-1111-1111-1111-111111111111",
		RequesterID: 42, ToolPolicyVersion: "docs.v1", Status: domain.TaskRunning,
	})
	service := newTestDocumentToolService(t, store, []string{"document_job_submit"})
	principal := DocumentToolPrincipal{SessionKey: store.task.SessionKey, WorkspaceID: store.task.WorkspaceID}
	for name, operation := range map[string]domain.DocumentOperationRequest{
		"unknown operation": {SchemaVersion: 1, Operation: "convert_pdf", Parameters: json.RawMessage(`{}`)},
		"unknown field":     {SchemaVersion: 1, Operation: "export_docx", Parameters: json.RawMessage(`{"content":"hello","shell":"id"}`)},
		"oversize content":  {SchemaVersion: 1, Operation: "export_docx", Parameters: json.RawMessage(`{"content":"` + strings.Repeat("x", MaxDocumentOperationContentBytes+1) + `"}`)},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := service.SubmitCommand(context.Background(), principal, DocumentToolCommandRequest{
				TaskID: "task-1", ToolName: "document_job_submit", RequestID: "call-1", Operation: operation,
			})
			if err == nil {
				t.Fatal("expected operation rejection")
			}
		})
	}
	if store.submitCalls != 0 {
		t.Fatalf("submit calls=%d", store.submitCalls)
	}
}

func TestDocumentToolServiceCloseAndStatusUseSameAuthorizedTaskJob(t *testing.T) {
	store := newDocumentToolFakeStore(domain.Task{
		ID: "task-1", SessionKey: "personal:42",
		WorkspaceID: "11111111-1111-1111-1111-111111111111",
		RequesterID: 42, ToolPolicyVersion: "docs.v1", Status: domain.TaskRunning,
	})
	service := newTestDocumentToolService(t, store, []string{"export_docx"})
	principal := DocumentToolPrincipal{SessionKey: store.task.SessionKey, WorkspaceID: store.task.WorkspaceID}
	_, err := service.SubmitCommand(context.Background(), principal, DocumentToolCommandRequest{
		TaskID: "task-1", ToolName: "export_docx", RequestID: "call-1",
		Operation: domain.DocumentOperationRequest{SchemaVersion: 1, Operation: "export_docx", Parameters: json.RawMessage(`{"content":"hello"}`)},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Close(context.Background(), principal, "task-1", "export_docx"); !errors.Is(err, ErrDocumentToolForbidden) {
		t.Fatalf("export_docx close err=%v", err)
	}
	service = newTestDocumentToolService(t, store, []string{"export_docx", "document_job_close"})
	closed, err := service.Close(context.Background(), principal, "task-1", "document_job_close")
	if err != nil {
		t.Fatal(err)
	}
	status, err := service.Status(context.Background(), principal, "task-1", "export_docx", "call-1")
	if err != nil {
		t.Fatal(err)
	}
	if closed.CommandsClosedAt == nil || status.Job.ID != closed.ID || status.Command == nil || status.Command.CommandID == "" || store.closeCalls != 1 || store.statusCalls != 1 {
		t.Fatalf("closed=%+v status=%+v close_calls=%d status_calls=%d", closed, status, store.closeCalls, store.statusCalls)
	}
}

func TestDocumentToolServiceCloseAllowsTerminalTaskButOtherOperationsRemainInactive(t *testing.T) {
	store := newDocumentToolFakeStore(domain.Task{
		ID: "task-1", SessionKey: "personal:42",
		WorkspaceID: "11111111-1111-1111-1111-111111111111",
		RequesterID: 42, ToolPolicyVersion: "docs.v1", Status: domain.TaskFailed,
	})
	service := newTestDocumentToolService(t, store, []string{"export_docx", "document_job_close"})
	principal := DocumentToolPrincipal{SessionKey: store.task.SessionKey, WorkspaceID: store.task.WorkspaceID}

	if _, err := service.Close(context.Background(), principal, "task-1", "document_job_close"); err != nil {
		t.Fatalf("terminal close err=%v", err)
	}
	if _, err := service.Status(context.Background(), principal, "task-1", "export_docx", "call-1"); !errors.Is(err, domain.ErrDocumentTaskInactive) {
		t.Fatalf("terminal status err=%v", err)
	}
	if store.closeCalls != 1 || store.statusCalls != 0 {
		t.Fatalf("close_calls=%d status_calls=%d", store.closeCalls, store.statusCalls)
	}
}

func TestDocumentToolServiceArtifactDownloadReauthorizesExactTool(t *testing.T) {
	store := newDocumentToolFakeStore(domain.Task{
		ID: "task-1", SessionKey: "personal:42",
		WorkspaceID: "11111111-1111-1111-1111-111111111111",
		RequesterID: 42, ToolPolicyVersion: "docs.v1", Status: domain.TaskRunning,
	})
	store.artifact = domain.DocumentArtifact{FileName: "report.docx", Content: []byte("docx")}
	principal := DocumentToolPrincipal{SessionKey: store.task.SessionKey, WorkspaceID: store.task.WorkspaceID}
	service := newTestDocumentToolService(t, store, []string{"export_docx"})
	artifact, err := service.DownloadArtifact(context.Background(), principal, "task-1", "export_docx", "call-1")
	if err != nil || artifact.FileName != "report.docx" || store.artifactRequestID != "call-1" {
		t.Fatalf("artifact=%+v err=%v request=%q", artifact, err, store.artifactRequestID)
	}
	if _, err := service.DownloadArtifact(context.Background(), principal, "task-1", "document_job_close", "call-1"); !errors.Is(err, ErrDocumentToolForbidden) {
		t.Fatalf("close artifact err=%v", err)
	}
	if _, err := service.DownloadArtifact(context.Background(), principal, "task-1", "export_docx", ""); !errors.Is(err, ErrDocumentToolInvalid) {
		t.Fatalf("empty request err=%v", err)
	}
	if store.artifactCalls != 1 {
		t.Fatalf("artifact calls=%d", store.artifactCalls)
	}
}

func newTestDocumentToolService(t *testing.T, store *documentToolFakeStore, allowed []string) DocumentToolService {
	t.Helper()
	service, err := NewDocumentToolService(DocumentToolServiceConfig{
		Store: store, Registry: documentToolFakeRegistry{allowed: allowed},
	})
	if err != nil {
		t.Fatal(err)
	}
	return service
}

type documentToolFakeRegistry struct{ allowed []string }

func (documentToolFakeRegistry) Digest() string { return "sha256:test" }
func (r documentToolFakeRegistry) Resolve(capabilityVersion, toolPolicyVersion string) (policy.ToolPolicy, error) {
	if capabilityVersion != CapabilityVersion || toolPolicyVersion != "docs.v1" {
		return policy.ToolPolicy{}, errors.New("policy not found")
	}
	return policy.ToolPolicy{Version: toolPolicyVersion, AllowedTools: append([]string(nil), r.allowed...)}, nil
}

type documentToolFakeStore struct {
	task              domain.Task
	job               domain.DocumentJob
	commands          map[string]domain.DocumentCommand
	payloads          map[string]string
	lastScope         domain.DocumentToolTaskScope
	lastRequester     int64
	submitCalls       int
	closeCalls        int
	statusCalls       int
	artifactCalls     int
	artifactRequestID string
	artifact          domain.DocumentArtifact
}

func newDocumentToolFakeStore(task domain.Task) *documentToolFakeStore {
	return &documentToolFakeStore{task: task, commands: map[string]domain.DocumentCommand{}, payloads: map[string]string{}}
}

func (s *documentToolFakeStore) GetTask(_ context.Context, taskID string) (domain.Task, error) {
	if taskID != s.task.ID {
		return domain.Task{}, errors.New("task not found")
	}
	return s.task, nil
}

func (s *documentToolFakeStore) SubmitDocumentToolCommand(_ context.Context, cmd domain.SubmitDocumentToolCommand) (domain.DocumentToolSubmission, error) {
	s.submitCalls++
	if err := s.authorize(cmd.Scope); err != nil {
		return domain.DocumentToolSubmission{}, err
	}
	if s.job.ID == "" {
		s.job = domain.DocumentJob{ID: "job-task-1", WorkspaceID: s.task.WorkspaceID, RequesterUserID: s.task.RequesterID, Status: domain.DocumentJobQueued}
	}
	encoded, _ := json.Marshal(cmd.Operation)
	if prior, ok := s.payloads[cmd.RequestID]; ok {
		if prior != string(encoded) {
			return domain.DocumentToolSubmission{}, domain.ErrDocumentIdempotencyConflict
		}
		return domain.DocumentToolSubmission{Job: s.job, Command: s.commands[cmd.RequestID]}, nil
	}
	command := domain.DocumentCommand{ID: "command-" + cmd.RequestID, JobID: s.job.ID, CommandID: "stable-" + cmd.RequestID, Status: domain.DocumentCommandPending}
	s.payloads[cmd.RequestID] = string(encoded)
	s.commands[cmd.RequestID] = command
	s.lastRequester = s.task.RequesterID
	return domain.DocumentToolSubmission{Job: s.job, Command: command}, nil
}

func (s *documentToolFakeStore) CloseDocumentToolJob(_ context.Context, scope domain.DocumentToolTaskScope) (domain.DocumentJob, error) {
	s.closeCalls++
	if err := s.authorizeScope(scope); err != nil {
		return domain.DocumentJob{}, err
	}
	now := s.task.UpdatedAt
	s.job.CommandsClosedAt = &now
	return s.job, nil
}

func (s *documentToolFakeStore) GetDocumentToolStatus(_ context.Context, scope domain.DocumentToolTaskScope, requestID string) (domain.DocumentToolStatus, error) {
	s.statusCalls++
	if err := s.authorize(scope); err != nil {
		return domain.DocumentToolStatus{}, err
	}
	if s.job.ID == "" {
		return domain.DocumentToolStatus{}, domain.ErrDocumentJobNotFound
	}
	status := domain.DocumentToolStatus{Job: s.job}
	if requestID != "" {
		command, ok := s.commands[requestID]
		if !ok {
			return domain.DocumentToolStatus{}, domain.ErrDocumentCommandNotFound
		}
		status.Command = &command
	}
	return status, nil
}

func (s *documentToolFakeStore) GetDocumentToolArtifact(_ context.Context, scope domain.DocumentToolTaskScope, requestID string) (domain.DocumentArtifact, error) {
	s.artifactCalls++
	if err := s.authorize(scope); err != nil {
		return domain.DocumentArtifact{}, err
	}
	s.artifactRequestID = requestID
	return s.artifact, nil
}

func (s *documentToolFakeStore) authorize(scope domain.DocumentToolTaskScope) error {
	if err := s.authorizeScope(scope); err != nil {
		return err
	}
	if s.task.Status != domain.TaskRunning {
		return domain.ErrDocumentTaskInactive
	}
	return nil
}

func (s *documentToolFakeStore) authorizeScope(scope domain.DocumentToolTaskScope) error {
	s.lastScope = scope
	if scope.TaskID != s.task.ID || scope.SessionKey != s.task.SessionKey || scope.WorkspaceID != s.task.WorkspaceID {
		return domain.ErrDocumentUnauthorized
	}
	return nil
}
