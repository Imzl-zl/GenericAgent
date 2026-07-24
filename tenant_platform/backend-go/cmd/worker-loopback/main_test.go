package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWriteFixtureMyKey_RefusesExistingFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "mykey.py")
	original := []byte("# production-ish fixture sentinel — do not overwrite\nnative_oai_config = {'apikey': 'keep-me'}\n")
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatalf("seed mykey.py: %v", err)
	}

	err := writeFixtureMyKey(dir, "http://127.0.0.1:9")
	if err == nil {
		t.Fatal("want error when mykey.py already exists, got nil")
	}
	// Visible, explicit refusal (not a silent overwrite or wrap of success).
	msg := err.Error()
	if !strings.Contains(msg, "mykey.py") {
		t.Fatalf("error should name mykey.py, got: %v", err)
	}
	if !strings.Contains(strings.ToLower(msg), "exist") && !errors.Is(err, os.ErrExist) {
		t.Fatalf("error should indicate existing file refusal, got: %v", err)
	}

	got, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatalf("read after failed write: %v", readErr)
	}
	if string(got) != string(original) {
		t.Fatalf("existing mykey.py was modified\nwant: %q\ngot:  %q", original, got)
	}
}

func TestWriteFixtureMyKey_CreatesWhenMissing(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "mykey.py")
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("precondition: mykey.py must not exist, err=%v", err)
	}

	apibase := "http://127.0.0.1:12345"
	if err := writeFixtureMyKey(dir, apibase); err != nil {
		t.Fatalf("writeFixtureMyKey on empty root: %v", err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read created mykey.py: %v", err)
	}
	body := string(raw)
	if !strings.Contains(body, testToken) {
		t.Fatalf("created mykey missing fixture token")
	}
	if !strings.Contains(body, apibase) {
		t.Fatalf("created mykey missing apibase %q", apibase)
	}
	// Must not look like a real cloud key pattern.
	if strings.Contains(body, "sk-ant-") || strings.Contains(body, "sk-proj-") {
		t.Fatalf("created mykey contains real-key-like pattern")
	}
}
