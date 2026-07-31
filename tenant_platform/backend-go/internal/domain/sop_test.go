package domain

import (
	"strings"
	"testing"
)

func TestSOPContentDigestUsesExactBoundedMarkdown(t *testing.T) {
	first, err := SOPContentDigest("# Report\n\nUse export_docx.\n")
	if err != nil {
		t.Fatal(err)
	}
	second, err := SOPContentDigest("# Report\n\nUse export_docx.\n")
	if err != nil {
		t.Fatal(err)
	}
	if first != second || len(first) != 64 {
		t.Fatalf("digest first=%q second=%q", first, second)
	}
	changed, err := SOPContentDigest("# Report\n\nUse export_docx.\n\n")
	if err != nil {
		t.Fatal(err)
	}
	if changed == first {
		t.Fatal("digest must cover exact content bytes")
	}

	for name, content := range map[string]string{
		"empty":    " \n\t",
		"nul":      "# SOP\x00",
		"oversize": strings.Repeat("x", MaxSOPContentBytes+1),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := SOPContentDigest(content); err == nil {
				t.Fatal("expected invalid SOP content")
			}
		})
	}
}

func TestValidateImportSOPCandidate(t *testing.T) {
	valid := ImportSOPCandidateCommand{
		RemoteSOPID: "remote-1",
		Title:       "Document report",
		Description: "Use for approved document reports.",
		FileType:    SOPFileTypeMarkdown,
		Content:     "# Document report\n\nFollow the approved process.\n",
	}
	if err := ValidateImportSOPCandidate(valid); err != nil {
		t.Fatal(err)
	}

	for name, edit := range map[string]func(*ImportSOPCandidateCommand){
		"remote id": func(cmd *ImportSOPCandidateCommand) { cmd.RemoteSOPID = "" },
		"title":     func(cmd *ImportSOPCandidateCommand) { cmd.Title = " " },
		"python":    func(cmd *ImportSOPCandidateCommand) { cmd.FileType = "python" },
		"content":   func(cmd *ImportSOPCandidateCommand) { cmd.Content = "" },
	} {
		t.Run(name, func(t *testing.T) {
			cmd := valid
			edit(&cmd)
			if err := ValidateImportSOPCandidate(cmd); err == nil {
				t.Fatal("expected validation error")
			}
		})
	}
}
