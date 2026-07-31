package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/application"
	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/domain"
	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/infrastructure/documentgateway"
	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/infrastructure/llmproxy"
)

const documentGatewayTestKey = "test-document-gateway-signing-key-32-bytes"

func TestDocumentGatewayCommandUsesBearerClaimsAndRejectsRequesterOverride(t *testing.T) {
	service := &documentToolFakeService{submission: domain.DocumentToolSubmission{
		Job:     domain.DocumentJob{ID: "job-1", Status: domain.DocumentJobQueued},
		Command: domain.DocumentCommand{ID: "command-1", CommandID: "stable-command", Status: domain.DocumentCommandPending},
	}}
	srv, token := newDocumentGatewayTestServer(t, service)
	body := []byte(`{"task_id":"task-1","tool_name":"export_docx","request_id":"call-1","operation":{"schema_version":1,"operation":"export_docx","parameters":{"output_name":"report.docx"}}}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/document/commands", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusAccepted {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	if bytes.Contains(rr.Body.Bytes(), []byte("job_id")) || bytes.Contains(rr.Body.Bytes(), []byte("command_id")) {
		t.Fatalf("command response leaked internal identifiers: %s", rr.Body.String())
	}
	if service.principal.SessionKey != "team:docs" || service.principal.WorkspaceID != "11111111-1111-1111-1111-111111111111" {
		t.Fatalf("principal=%+v", service.principal)
	}
	if service.command.TaskID != "task-1" || service.command.RequestID != "call-1" || service.command.Operation.Operation != "export_docx" {
		t.Fatalf("command=%+v", service.command)
	}

	body = []byte(`{"task_id":"task-1","tool_name":"export_docx","request_id":"call-2","requester_user_id":999,"operation":{"schema_version":1,"operation":"export_docx","parameters":{}}}`)
	req = httptest.NewRequest(http.MethodPost, "/v1/document/commands", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	rr = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("requester override status=%d body=%s", rr.Code, rr.Body.String())
	}
}

func TestDocumentGatewayRejectsMissingInvalidAndLLMCapabilityTokens(t *testing.T) {
	service := &documentToolFakeService{}
	srv, _ := newDocumentGatewayTestServer(t, service)
	body := []byte(`{"task_id":"task-1","tool_name":"export_docx","request_id":"call-1","operation":{"schema_version":1,"operation":"export_docx","parameters":{}}}`)

	llmIssuer, err := llmproxy.NewIssuer([]byte(documentGatewayTestKey), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	llmToken, _, err := llmIssuer.Issue(llmproxy.CapabilitySpec{
		SessionKey: "team:docs", ProviderID: 1, ProviderRevision: 1,
		ProviderType: domain.ProviderNativeOAI, Model: "model", PolicyVersion: "policy.v1",
	})
	if err != nil {
		t.Fatal(err)
	}

	for name, authorization := range map[string]string{
		"missing":        "",
		"invalid":        "Bearer not-a-jwt",
		"llm capability": "Bearer " + llmToken,
	} {
		t.Run(name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/v1/document/commands", bytes.NewReader(body))
			if authorization != "" {
				req.Header.Set("Authorization", authorization)
			}
			rr := httptest.NewRecorder()
			srv.Handler().ServeHTTP(rr, req)
			if rr.Code != http.StatusUnauthorized {
				t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
			}
		})
	}
	if service.submitCalls != 0 {
		t.Fatalf("submit calls=%d", service.submitCalls)
	}
}

func TestDocumentGatewayCloseStatusAndErrorMapping(t *testing.T) {
	now := time.Now().UTC()
	service := &documentToolFakeService{
		job: domain.DocumentJob{ID: "job-1", Status: domain.DocumentJobRunning, CommandsClosedAt: &now},
		status: domain.DocumentToolStatus{
			Job:     domain.DocumentJob{ID: "job-1", Status: domain.DocumentJobRunning, CommandsClosedAt: &now},
			Command: &domain.DocumentCommand{CommandID: "stable-command", Status: domain.DocumentCommandSucceeded},
		},
	}
	srv, token := newDocumentGatewayTestServer(t, service)

	req := httptest.NewRequest(http.MethodPost, "/v1/document/close", bytes.NewBufferString(`{"task_id":"task-1","tool_name":"document_job_close"}`))
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK || service.closeTaskID != "task-1" || service.closeToolName != "document_job_close" {
		t.Fatalf("close status=%d body=%s service=%+v", rr.Code, rr.Body.String(), service)
	}
	if bytes.Contains(rr.Body.Bytes(), []byte("job_id")) || bytes.Contains(rr.Body.Bytes(), []byte("command_id")) {
		t.Fatalf("close response leaked internal identifiers: %s", rr.Body.String())
	}

	req = httptest.NewRequest(http.MethodGet, "/v1/document/status?task_id=task-1&tool_name=export_docx&request_id=call-1", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rr = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK || service.statusTaskID != "task-1" || service.statusToolName != "export_docx" || service.statusRequestID != "call-1" {
		t.Fatalf("status status=%d body=%s service=%+v", rr.Code, rr.Body.String(), service)
	}
	if !bytes.Contains(rr.Body.Bytes(), []byte(`"status":"succeeded"`)) || bytes.Contains(rr.Body.Bytes(), []byte("command_id")) || bytes.Contains(rr.Body.Bytes(), []byte("job_id")) {
		t.Fatalf("status response leaked internal identifiers or missed state: %s", rr.Body.String())
	}

	service.err = domain.ErrDocumentUnauthorized
	req = httptest.NewRequest(http.MethodGet, "/v1/document/status?task_id=task-1&tool_name=export_docx", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rr = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("unauthorized status=%d body=%s", rr.Code, rr.Body.String())
	}
	service.err = domain.ErrDocumentTaskInactive
	rr = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusConflict {
		t.Fatalf("inactive status=%d body=%s", rr.Code, rr.Body.String())
	}
	service.err = domain.ErrDocumentGlobalQueueFull
	rr = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusTooManyRequests {
		t.Fatalf("queue status=%d body=%s", rr.Code, rr.Body.String())
	}
}

func TestDocumentGatewayArtifactDownloadUsesBearerScopeAndDurableMetadata(t *testing.T) {
	content := []byte("complete-docx")
	digest := sha256.Sum256(content)
	service := &documentToolFakeService{artifact: domain.DocumentArtifact{
		FileName: "Quarterly Report.docx", MediaType: "application/vnd.openxmlformats-officedocument.wordprocessingml.document",
		Content: content, SizeBytes: int64(len(content)), SHA256: hex.EncodeToString(digest[:]),
	}}
	srv, token := newDocumentGatewayTestServer(t, service)
	req := httptest.NewRequest(http.MethodGet, "/v1/document/artifact?task_id=task-1&tool_name=export_docx&request_id=call-1", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK || rr.Body.String() != "complete-docx" {
		t.Fatalf("status=%d body=%q", rr.Code, rr.Body.String())
	}
	if rr.Header().Get("Content-Type") != service.artifact.MediaType || rr.Header().Get("X-Content-SHA256") != service.artifact.SHA256 || rr.Header().Get("Cache-Control") != "no-store" || !strings.Contains(rr.Header().Get("Content-Disposition"), "Quarterly Report.docx") {
		t.Fatalf("headers=%v", rr.Header())
	}
	if service.artifactTaskID != "task-1" || service.artifactToolName != "export_docx" || service.artifactRequestID != "call-1" || service.principal.SessionKey != "team:docs" {
		t.Fatalf("service=%+v", service)
	}

	service.err = domain.ErrDocumentArtifactNotFound
	rr = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound || bytes.Contains(rr.Body.Bytes(), []byte("complete-docx")) {
		t.Fatalf("missing status=%d body=%q", rr.Code, rr.Body.String())
	}
	service.err = nil
	service.artifact.SHA256 = strings.Repeat("0", 64)
	rr = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusInternalServerError || bytes.Contains(rr.Body.Bytes(), content) {
		t.Fatalf("corrupt status=%d body=%q", rr.Code, rr.Body.String())
	}
}

func TestValidateDocumentArtifactResponseRejectsUnsafeMetadata(t *testing.T) {
	content := []byte("docx")
	digest := sha256.Sum256(content)
	base := domain.DocumentArtifact{
		FileName: "report.docx", MediaType: "application/vnd.openxmlformats-officedocument.wordprocessingml.document",
		Content: content, SizeBytes: int64(len(content)), SHA256: hex.EncodeToString(digest[:]),
	}
	for name, edit := range map[string]func(*domain.DocumentArtifact){
		"path separator":   func(a *domain.DocumentArtifact) { a.FileName = "../report.docx" },
		"control":          func(a *domain.DocumentArtifact) { a.FileName = "report\r.docx" },
		"wrong extension":  func(a *domain.DocumentArtifact) { a.FileName = "report.pdf" },
		"wrong media type": func(a *domain.DocumentArtifact) { a.MediaType = "application/octet-stream" },
	} {
		t.Run(name, func(t *testing.T) {
			artifact := base
			edit(&artifact)
			if err := validateDocumentArtifactResponse(artifact); err == nil {
				t.Fatal("expected unsafe metadata rejection")
			}
		})
	}
}

func TestDocumentGatewayConfigMustBeComplete(t *testing.T) {
	validator, err := documentgateway.NewValidator([]byte(documentGatewayTestKey))
	if err != nil {
		t.Fatal(err)
	}
	base := ServerConfig{
		Service: dashboardFakeTaskService{}, Registry: dashboardFakeRegistry{},
		DevToken: "dev-token", DevUserID: 1,
	}
	withService := base
	withService.DocumentTools = &documentToolFakeService{}
	if _, err := NewServer(withService); err == nil {
		t.Fatal("expected missing validator error")
	}
	withValidator := base
	withValidator.DocumentCapabilityValidator = validator
	if _, err := NewServer(withValidator); err == nil {
		t.Fatal("expected missing service error")
	}
}

func newDocumentGatewayTestServer(t *testing.T, service application.DocumentToolService) (*Server, string) {
	t.Helper()
	issuer, err := documentgateway.NewIssuer([]byte(documentGatewayTestKey), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	validator, err := documentgateway.NewValidator([]byte(documentGatewayTestKey))
	if err != nil {
		t.Fatal(err)
	}
	token, _, err := issuer.Issue(documentgateway.CapabilitySpec{
		SessionKey: "team:docs", WorkspaceID: "11111111-1111-1111-1111-111111111111",
	})
	if err != nil {
		t.Fatal(err)
	}
	srv, err := NewServer(ServerConfig{
		Service: dashboardFakeTaskService{}, Registry: dashboardFakeRegistry{},
		DocumentTools: service, DocumentCapabilityValidator: validator,
		DevToken: "dev-token", DevUserID: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	return srv, token
}

type documentToolFakeService struct {
	principal         application.DocumentToolPrincipal
	command           application.DocumentToolCommandRequest
	submission        domain.DocumentToolSubmission
	job               domain.DocumentJob
	status            domain.DocumentToolStatus
	err               error
	submitCalls       int
	closeTaskID       string
	closeToolName     string
	statusTaskID      string
	statusToolName    string
	statusRequestID   string
	artifact          domain.DocumentArtifact
	artifactTaskID    string
	artifactToolName  string
	artifactRequestID string
}

func (s *documentToolFakeService) SubmitCommand(_ context.Context, principal application.DocumentToolPrincipal, request application.DocumentToolCommandRequest) (domain.DocumentToolSubmission, error) {
	s.submitCalls++
	s.principal, s.command = principal, request
	return s.submission, s.err
}
func (s *documentToolFakeService) Close(_ context.Context, principal application.DocumentToolPrincipal, taskID, toolName string) (domain.DocumentJob, error) {
	s.principal, s.closeTaskID, s.closeToolName = principal, taskID, toolName
	return s.job, s.err
}
func (s *documentToolFakeService) Status(_ context.Context, principal application.DocumentToolPrincipal, taskID, toolName, requestID string) (domain.DocumentToolStatus, error) {
	s.principal, s.statusTaskID, s.statusToolName, s.statusRequestID = principal, taskID, toolName, requestID
	return s.status, s.err
}

func (s *documentToolFakeService) DownloadArtifact(_ context.Context, principal application.DocumentToolPrincipal, taskID, toolName, requestID string) (domain.DocumentArtifact, error) {
	s.principal, s.artifactTaskID, s.artifactToolName, s.artifactRequestID = principal, taskID, toolName, requestID
	return s.artifact, s.err
}

var _ application.DocumentToolService = (*documentToolFakeService)(nil)
