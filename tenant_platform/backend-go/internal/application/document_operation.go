package application

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/domain"
)

const (
	MaxDocumentOperationContentBytes = 1024 * 1024
	maxDocumentOperationTitleBytes   = 4096
	DocumentDOCXMediaType            = domain.DocumentDOCXMediaType
	documentToolExecutable           = "/usr/local/bin/ga-document-tool"
)

type DocumentArtifactPlan struct {
	FileName  string
	MediaType string
}

type DocumentOperationPlan struct {
	Argv     []string
	Stdin    []byte
	Artifact *DocumentArtifactPlan
}

type FixedDocumentOperationCompiler struct{}

func (FixedDocumentOperationCompiler) Compile(request domain.DocumentOperationRequest, commandID string) (DocumentOperationPlan, error) {
	if request.SchemaVersion != 1 {
		return DocumentOperationPlan{}, fmt.Errorf("document operation schema_version must be 1")
	}
	if strings.TrimSpace(request.Operation) != "export_docx" {
		return DocumentOperationPlan{}, fmt.Errorf("unknown document operation %q", request.Operation)
	}
	if strings.TrimSpace(commandID) == "" {
		return DocumentOperationPlan{}, fmt.Errorf("document command id is required")
	}
	parameters, err := decodeExportDOCXParameters(request.Parameters)
	if err != nil {
		return DocumentOperationPlan{}, err
	}
	requestContent, err := json.Marshal(struct {
		SchemaVersion int    `json:"schema_version"`
		Title         string `json:"title,omitempty"`
		Content       string `json:"content"`
	}{SchemaVersion: 1, Title: parameters.Title, Content: parameters.Content})
	if err != nil {
		return DocumentOperationPlan{}, fmt.Errorf("encode export_docx request: %w", err)
	}
	return DocumentOperationPlan{
		Argv:  documentExportDOCXArgv(),
		Stdin: requestContent,
		Artifact: &DocumentArtifactPlan{
			FileName: parameters.OutputName, MediaType: DocumentDOCXMediaType,
		},
	}, nil
}

type exportDOCXParameters struct {
	OutputName string `json:"output_name"`
	Title      string `json:"title"`
	Content    string `json:"content"`
}

func decodeExportDOCXParameters(raw json.RawMessage) (exportDOCXParameters, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var parameters exportDOCXParameters
	if err := decoder.Decode(&parameters); err != nil {
		return exportDOCXParameters{}, fmt.Errorf("decode export_docx parameters: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return exportDOCXParameters{}, fmt.Errorf("export_docx parameters must contain one JSON object")
		}
		return exportDOCXParameters{}, fmt.Errorf("decode trailing export_docx parameters: %w", err)
	}
	parameters.OutputName = strings.TrimSpace(parameters.OutputName)
	parameters.Title = strings.TrimSpace(parameters.Title)
	if parameters.OutputName == "" {
		parameters.OutputName = "document.docx"
	}
	if err := domain.ValidateDocumentArtifactMetadata(parameters.OutputName, DocumentDOCXMediaType); err != nil {
		return exportDOCXParameters{}, err
	}
	if len([]byte(parameters.Title)) > maxDocumentOperationTitleBytes || !validXMLDocumentText(parameters.Title) {
		return exportDOCXParameters{}, fmt.Errorf("export_docx title is too large or contains invalid XML characters")
	}
	contentBytes := len([]byte(parameters.Content))
	if strings.TrimSpace(parameters.Content) == "" || contentBytes > MaxDocumentOperationContentBytes || !validXMLDocumentText(parameters.Content) {
		return exportDOCXParameters{}, fmt.Errorf("export_docx content must be non-empty, <= %d bytes, and valid XML text", MaxDocumentOperationContentBytes)
	}
	return parameters, nil
}

func validXMLDocumentText(value string) bool {
	for _, r := range value {
		if r == '\t' || r == '\n' || r == '\r' || (r >= 0x20 && r <= 0xD7FF) || (r >= 0xE000 && r <= 0xFFFD) || (r >= 0x10000 && r <= 0x10FFFF) {
			continue
		}
		return false
	}
	return true
}

func documentExportDOCXArgv() []string {
	return []string{documentToolExecutable, "export-docx", "--input", "-", "--output", "-"}
}

func isDocumentExportDOCXArgv(argv []string) bool {
	want := documentExportDOCXArgv()
	if len(argv) != len(want) {
		return false
	}
	for i := range want {
		if argv[i] != want[i] {
			return false
		}
	}
	return true
}
