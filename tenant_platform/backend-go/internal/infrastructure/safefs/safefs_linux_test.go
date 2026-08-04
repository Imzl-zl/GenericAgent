//go:build linux

package safefs

import (
	"os"
	"path/filepath"
	"testing"
)

// round9 审查: 媒体源侧 TOCTOU——父目录在预检后被替换为 symlink 时,
// openat2 RESOLVE_NO_SYMLINKS 必须在单次路径解析中拒绝, 不能读取链接目标。
func TestCopyFileFromBeneathRejectsSymlinkComponents(t *testing.T) {
	srcRoot := t.TempDir()
	secret := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(secret, []byte("s3cret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(srcRoot, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	// 中间组件是 symlink(指向根外)。
	link := filepath.Join(srcRoot, "sub")
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Dir(secret), link); err != nil {
		t.Skipf("symlink not permitted: %v", err)
	}
	dstRoot := t.TempDir()
	err := CopyFileFromBeneath(srcRoot, "sub/secret.txt", dstRoot, "attachments/F001.txt", 0o640, 0)
	if err == nil {
		t.Fatal("symlink component must be rejected by openat2")
	}
	// 最终组件是 symlink 同样拒绝。
	plain := filepath.Join(t.TempDir(), "plain.txt")
	if err := os.WriteFile(plain, []byte("plain"), 0o600); err != nil {
		t.Fatal(err)
	}
	finalLink := filepath.Join(srcRoot, "final-link.txt")
	if err := os.Symlink(plain, finalLink); err != nil {
		t.Fatal(err)
	}
	if err := CopyFileFromBeneath(srcRoot, "final-link.txt", dstRoot, "attachments/F002.txt", 0o640, 0); err == nil {
		t.Fatal("final symlink component must be rejected")
	}
}

// round9 审查: 正常路径(无符号链接)复制成功且内容一致。
func TestCopyFileFromBeneathHappyPath(t *testing.T) {
	srcRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(srcRoot, "bot1"), 0o755); err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(srcRoot, "bot1", "doc.txt")
	if err := os.WriteFile(src, []byte("hello attachment"), 0o600); err != nil {
		t.Fatal(err)
	}
	dstRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dstRoot, "attachments"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := CopyFileFromBeneath(srcRoot, "bot1/doc.txt", dstRoot, "attachments/F001_doc.txt", 0o640, 0); err != nil {
		t.Fatalf("copy: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dstRoot, "attachments", "F001_doc.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "hello attachment" {
		t.Fatalf("content mismatch: %q", got)
	}
	// 超限拒绝且目标不残留(unlinkat 清理路径)。
	if err := CopyFileFromBeneath(srcRoot, "bot1/doc.txt", dstRoot, "attachments/F002_doc.txt", 0o640, 5); err == nil {
		t.Fatal("over-limit copy must fail")
	}
	if _, statErr := os.Stat(filepath.Join(dstRoot, "attachments", "F002_doc.txt")); !os.IsNotExist(statErr) {
		t.Fatalf("target must not remain after over-limit copy: %v", statErr)
	}
}

// round9 审查: 目标父目录是符号链接时 createBeneath 必须拒绝(不能沿链接
// 创建/删除其他目录中的同名文件)。
func TestCopyFileBeneathRejectsSymlinkTargetParent(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "real"), 0o755); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "evil")); err != nil {
		t.Skipf("symlink not permitted: %v", err)
	}
	src := filepath.Join(t.TempDir(), "src.txt")
	if err := os.WriteFile(src, []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := CopyFileBeneath(root, "evil/victim.txt", src, 0o640, 0)
	if err == nil {
		t.Fatal("symlink target parent must be rejected")
	}
	if _, statErr := os.Stat(filepath.Join(outside, "victim.txt")); !os.IsNotExist(statErr) {
		t.Fatalf("must not create file through symlink parent: %v", statErr)
	}
}
