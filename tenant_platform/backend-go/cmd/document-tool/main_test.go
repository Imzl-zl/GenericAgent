package main

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"io"
	"strings"
	"testing"
)

func TestRunExportDOCXWritesValidBoundedDocument(t *testing.T) {
	input, err := json.Marshal(exportRequest{SchemaVersion: 1, Title: "Q2", Content: "hello\n\nworld"})
	if err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if code := run([]string{"export-docx", "--input", "-", "--output", "-"}, bytes.NewReader(input), &stdout, &stderr); code != 0 {
		t.Fatalf("code=%d stderr=%q", code, stderr.String())
	}
	if stdout.Len() == 0 || stdout.Len() > maxDOCXOutputBytes || stderr.Len() != 0 {
		t.Fatalf("stdout=%d stderr=%q", stdout.Len(), stderr.String())
	}
	reader, err := zip.NewReader(bytes.NewReader(stdout.Bytes()), int64(stdout.Len()))
	if err != nil {
		t.Fatalf("invalid docx zip: %v", err)
	}
	files := map[string][]byte{}
	for _, file := range reader.File {
		handle, err := file.Open()
		if err != nil {
			t.Fatal(err)
		}
		files[file.Name], err = io.ReadAll(handle)
		_ = handle.Close()
		if err != nil {
			t.Fatal(err)
		}
	}
	if !bytes.Contains(files["word/document.xml"], []byte("hello")) || !bytes.Contains(files["docProps/core.xml"], []byte("Q2")) {
		t.Fatalf("docx files=%v", files)
	}
	for _, required := range []string{"[Content_Types].xml", "_rels/.rels", "word/document.xml", "word/styles.xml"} {
		if _, ok := files[required]; !ok {
			t.Fatalf("missing %s", required)
		}
	}
}

func TestRunExportDOCXRejectsUnsafeOrOversizedInputWithoutPartialOutput(t *testing.T) {
	tests := []struct {
		name  string
		args  []string
		input string
	}{
		{"unknown field", []string{"export-docx", "--input", "-", "--output", "-"}, `{"schema_version":1,"content":"ok","path":"/host"}`},
		{"wrong schema", []string{"export-docx", "--input", "-", "--output", "-"}, `{"schema_version":2,"content":"ok"}`},
		{"empty content", []string{"export-docx", "--input", "-", "--output", "-"}, `{"schema_version":1,"content":""}`},
		{"invalid XML control", []string{"export-docx", "--input", "-", "--output", "-"}, `{"schema_version":1,"content":"bad\u0001xml"}`},
		{"dynamic path", []string{"export-docx", "--input", "/tmp/input", "--output", "-"}, `{"schema_version":1,"content":"ok"}`},
		{"unknown command", []string{"shell", "sh", "-c", "id"}, ``},
		{"oversized input", []string{"export-docx", "--input", "-", "--output", "-"}, strings.Repeat("x", maxRequestBytes+1)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if code := run(test.args, strings.NewReader(test.input), &stdout, &stderr); code == 0 {
				t.Fatal("expected non-zero exit")
			}
			if stdout.Len() != 0 || stderr.Len() == 0 {
				t.Fatalf("stdout=%d stderr=%q", stdout.Len(), stderr.String())
			}
		})
	}
}

func TestCgroupPathUsesFixedAllowlist(t *testing.T) {
	for name, want := range map[string]string{
		"memory.max": "/sys/fs/cgroup/memory.max",
		"cpu.max":    "/sys/fs/cgroup/cpu.max",
		"pids.max":   "/sys/fs/cgroup/pids.max",
	} {
		got, err := cgroupPath(name)
		if err != nil || got != want {
			t.Fatalf("%s got=%q err=%v", name, got, err)
		}
	}
	for _, name := range []string{"", "../etc/passwd", "/etc/passwd", "memory.current"} {
		if _, err := cgroupPath(name); err == nil {
			t.Fatalf("expected %q rejection", name)
		}
	}
}
