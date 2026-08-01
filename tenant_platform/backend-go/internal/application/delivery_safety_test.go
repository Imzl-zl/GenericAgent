package application

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// 交付安全: 符号链接/特殊文件/目录/替换必须拒绝(方案 §4/§6)。
func TestValidateDeliverableRejectsSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink requires privileges on Windows")
	}
	root := t.TempDir()
	target := filepath.Join(root, "secret.txt")
	if err := os.WriteFile(target, []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "outputs")
	if err := os.MkdirAll(link, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(link, "leak.docx")); err != nil {
		t.Fatal(err)
	}

	if _, err := validateDeliverable(link, "leak.docx"); err == nil {
		t.Fatal("symlinked deliverable must be rejected")
	}
	// 平台不能读取链接目标 —— 链接文件本身不得被打开。
	data, _ := os.ReadFile(target)
	if string(data) != "secret" {
		t.Fatal("fixture broken")
	}
}

// TestValidateDeliverableRejectsIntermediateSymlink: outputs/sub -> /etc 时,
// outputs/sub/passwd 必须被拒绝(中间组件链接逃逸, C1 回归)。
func TestValidateDeliverableRejectsIntermediateSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink requires privileges on Windows")
	}
	root := t.TempDir()
	outputs := filepath.Join(root, "outputs")
	if err := os.MkdirAll(outputs, 0o755); err != nil {
		t.Fatal(err)
	}
	// outputs/sub -> root(输出根之外)
	if err := os.Symlink(root, filepath.Join(outputs, "sub")); err != nil {
		t.Fatal(err)
	}
	victim := filepath.Join(root, "victim.txt")
	if err := os.WriteFile(victim, []byte("confidential"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := validateDeliverable(outputs, "sub/victim.txt"); err == nil {
		t.Fatal("intermediate symlink escape must be rejected")
	}
}

func TestValidateDeliverableRejectsPathEscape(t *testing.T) {
	root := t.TempDir()
	outputs := filepath.Join(root, "outputs")
	if err := os.MkdirAll(outputs, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := validateDeliverable(outputs, "../outside.docx"); err == nil {
		t.Fatal("path escape must be rejected")
	}
	if _, err := validateDeliverable(outputs, "a/../../outside.docx"); err == nil {
		t.Fatal("nested path escape must be rejected")
	}
}

func TestValidateDeliverableRejectsDirectory(t *testing.T) {
	root := t.TempDir()
	outputs := filepath.Join(root, "outputs")
	if err := os.MkdirAll(filepath.Join(outputs, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := validateDeliverable(outputs, "sub"); err == nil {
		t.Fatal("directory deliverable must be rejected")
	}
}

func TestValidateDeliverableAcceptsRegularFile(t *testing.T) {
	root := t.TempDir()
	outputs := filepath.Join(root, "outputs")
	if err := os.MkdirAll(outputs, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(outputs, "report.docx")
	if err := os.WriteFile(path, []byte("docx-bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := validateDeliverable(outputs, "report.docx")
	if err != nil {
		t.Fatalf("regular file rejected: %v", err)
	}
	if got != path {
		t.Fatalf("got %s, want %s", got, path)
	}
}

func TestValidateDeliverableRejectsOversize(t *testing.T) {
	root := t.TempDir()
	outputs := filepath.Join(root, "outputs")
	if err := os.MkdirAll(outputs, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(outputs, "big.pdf")
	if err := os.WriteFile(path, make([]byte, 2<<20), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := validateDeliverableLimited(outputs, "big.pdf", 1<<20); err == nil {
		t.Fatal("oversize deliverable must be rejected")
	}
}
