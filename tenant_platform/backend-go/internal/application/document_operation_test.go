package application

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/domain"
)

func TestFixedDocumentOperationCompilerBuildsExportDOCXPlan(t *testing.T) {
	compiler := FixedDocumentOperationCompiler{}
	plan, err := compiler.Compile(domain.DocumentOperationRequest{
		SchemaVersion: 1,
		Operation:     "export_docx",
		Parameters:    json.RawMessage(`{"output_name":"Quarterly Report.docx","title":"Q2","content":"hello"}`),
	}, "gateway:stable-command")
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Argv) != 6 || plan.Argv[0] != "/usr/local/bin/ga-document-tool" || plan.Argv[1] != "export-docx" || plan.Argv[3] != "-" || plan.Argv[5] != "-" {
		t.Fatalf("argv=%q", plan.Argv)
	}
	if plan.Artifact == nil || plan.Artifact.FileName != "Quarterly Report.docx" || plan.Artifact.MediaType != DocumentDOCXMediaType {
		t.Fatalf("artifact=%+v", plan.Artifact)
	}
	if strings.Contains(strings.Join(plan.Argv, "\x00"), "hello") || strings.Contains(strings.Join(plan.Argv, "\x00"), "Quarterly") {
		t.Fatalf("argv leaked request content: %q", plan.Argv)
	}
	var request struct {
		Title   string `json:"title"`
		Content string `json:"content"`
	}
	if err := json.Unmarshal(plan.Stdin, &request); err != nil || request.Title != "Q2" || request.Content != "hello" {
		t.Fatalf("request=%+v err=%v", request, err)
	}
}

func TestFixedDocumentOperationCompilerRejectsUnknownUnsafeOrOversizedRequests(t *testing.T) {
	compiler := FixedDocumentOperationCompiler{}
	tests := []struct {
		name       string
		operation  string
		parameters string
		commandID  string
	}{
		{"unknown operation", "run_shell", `{}`, "command"},
		{"unknown field", "export_docx", `{"content":"ok","argv":["sh"]}`, "command"},
		{"missing content", "export_docx", `{"output_name":"a.docx"}`, "command"},
		{"path output", "export_docx", `{"output_name":"../a.docx","content":"ok"}`, "command"},
		{"wrong extension", "export_docx", `{"output_name":"a.pdf","content":"ok"}`, "command"},
		{"invalid XML control", "export_docx", `{"content":"bad\u0001xml"}`, "command"},
		{"oversized content", "export_docx", `{"content":"` + strings.Repeat("x", MaxDocumentOperationContentBytes+1) + `"}`, "command"},
		{"empty command", "export_docx", `{"content":"ok"}`, " "},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := compiler.Compile(domain.DocumentOperationRequest{
				SchemaVersion: 1, Operation: test.operation, Parameters: json.RawMessage(test.parameters),
			}, test.commandID)
			if err == nil {
				t.Fatal("expected compiler error")
			}
		})
	}
}
