//go:build unix

package safefs

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// Round8 审查: 入站附件源不得是符号链接——Poller 被攻破后可把 media_paths
// 指向 Platform 容器内任意文件, 复制时跟随 symlink 即读取该文件。
func TestCopyFileBeneathRejectsSymlinkSource(t *testing.T) {
	root := t.TempDir()
	// 目标沙箱外的秘密文件。
	outside := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(outside, []byte("s3cret"), 0o600); err != nil {
		t.Fatal(err)
	}
	// 源是 symlink 指向它。
	link := filepath.Join(t.TempDir(), "evil-link.txt")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlink not permitted: %v", err)
	}
	err := CopyFileBeneath(root, "attachments/F001_evil.txt", link, 0o640, 0)
	if err == nil {
		t.Fatal("symlink source must be rejected")
	}
	// 目标不得残留半写文件。
	if _, statErr := os.Stat(filepath.Join(root, "attachments", "F001_evil.txt")); !os.IsNotExist(statErr) {
		t.Fatalf("target must not remain after rejected copy: %v", statErr)
	}
}

// Round8 审查: 源必须是普通文件——设备/FIFO/socket 拒绝(防 /proc 特殊文件
// 或 FIFO 阻塞读取)。
func TestCopyFileBeneathRejectsFIFOSource(t *testing.T) {
	root := t.TempDir()
	fifo := filepath.Join(t.TempDir(), "evil-fifo")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Skipf("mkfifo not permitted: %v", err)
	}
	err := CopyFileBeneath(root, "attachments/F001_fifo.bin", fifo, 0o640, 0)
	if err == nil {
		t.Fatal("FIFO source must be rejected")
	}
}

// Round8 审查: 超过上限的源在复制中被截断拒绝, 目标不残留。
func TestCopyFileBeneathEnforcesMaxBytes(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "attachments"), 0o755); err != nil {
		t.Fatal(err)
	}
	big := writeFile(t, t.TempDir(), "big.bin", 2000)
	err := CopyFileBeneath(root, "attachments/F001_big.bin", big, 0o640, 1000)
	if err == nil {
		t.Fatal("over-limit source must fail")
	}
	if _, statErr := os.Stat(filepath.Join(root, "attachments", "F001_big.bin")); !os.IsNotExist(statErr) {
		t.Fatalf("target must not remain after over-limit copy: %v", statErr)
	}
	// 恰好等于上限成功。
	exact := writeFile(t, t.TempDir(), "exact.bin", 1000)
	if err := CopyFileBeneath(root, "attachments/F001_exact.bin", exact, 0o640, 1000); err != nil {
		t.Fatalf("copy at exact limit: %v", err)
	}
}
