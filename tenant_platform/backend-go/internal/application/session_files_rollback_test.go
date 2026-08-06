package application

import (
	"os"
	"path/filepath"
	"testing"
)

// round12 审查(I5): 多附件导入必须原子——中途失败(复制失败/manifest 保存
// 失败)时, 已复制的文件必须回滚, 不得残留无 manifest 归属的附件。

// TestImportInboundRollsBackCopiedFilesOnLaterFailure: 第 3 个附件复制失败
// 时, 前 2 个已复制文件必须删除, manifest 不变。
func TestImportInboundRollsBackCopiedFilesOnLaterFailure(t *testing.T) {
	root := t.TempDir()
	mediaRoot := t.TempDir()
	files, err := NewWorkspaceSessionFiles(root, mediaRoot, nil)
	if err != nil {
		t.Fatal(err)
	}
	src1 := filepath.Join(mediaRoot, "a.txt")
	src2 := filepath.Join(mediaRoot, "b.txt")
	if err := os.WriteFile(src1, []byte("a"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(src2, []byte("b"), 0o644); err != nil {
		t.Fatal(err)
	}
	// 逃逸 media root 的路径: 第 3 个源复制必然失败。
	outside := filepath.Join(t.TempDir(), "c.txt")
	if err := os.WriteFile(outside, []byte("c"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := files.ImportInbound("personal:1", []string{src1, src2, outside}); err == nil {
		t.Fatal("expected import failure for source escaping media root")
	}

	attachmentsDir := filepath.Join(mustSandboxRoot(t, files, "personal:1"), sessionAttachmentsDir)
	entries, err := os.ReadDir(attachmentsDir)
	if err != nil {
		t.Fatalf("read attachments dir: %v", err)
	}
	if len(entries) != 0 {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("attachments left after failed import: %v", names)
	}
	refs, err := files.Recent("personal:1", 8)
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 0 {
		t.Fatalf("manifest has %d entries after failed import, want 0", len(refs))
	}
}

// TestImportInboundSuccessKeepsAllFiles: 对照组——全部成功时文件与 manifest
// 完整(防止回滚逻辑误删成功路径)。
func TestImportInboundSuccessKeepsAllFiles(t *testing.T) {
	root := t.TempDir()
	mediaRoot := t.TempDir()
	files, err := NewWorkspaceSessionFiles(root, mediaRoot, nil)
	if err != nil {
		t.Fatal(err)
	}
	src1 := filepath.Join(mediaRoot, "a.txt")
	src2 := filepath.Join(mediaRoot, "b.txt")
	if err := os.WriteFile(src1, []byte("a"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(src2, []byte("b"), 0o644); err != nil {
		t.Fatal(err)
	}
	refs, err := files.ImportInbound("personal:1", []string{src1, src2})
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 2 {
		t.Fatalf("imported refs = %d, want 2", len(refs))
	}
	attachmentsDir := filepath.Join(mustSandboxRoot(t, files, "personal:1"), sessionAttachmentsDir)
	entries, err := os.ReadDir(attachmentsDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("attachments = %d, want 2", len(entries))
	}
}
