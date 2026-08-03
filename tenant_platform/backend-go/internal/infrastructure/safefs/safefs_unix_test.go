package safefs

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeFile(t *testing.T, dir, name string, size int) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, make([]byte, size), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// 审查 R5-I5: 恰好等于上限的文件必须成功(读上限 maxBytes+1 不得误拒)。
func TestReadFileBeneathLimitedAtExactLimit(t *testing.T) {
	dir := t.TempDir()
	p := writeFile(t, dir, "exact.bin", 1000)
	buf, err := ReadFileBeneathLimited(dir, "exact.bin", 1000)
	if err != nil {
		t.Fatalf("read at exact limit: %v", err)
	}
	if len(buf) != 1000 {
		t.Fatalf("read %d bytes, want 1000", len(buf))
	}
	_ = p
}

// 超限文件(fstat 即可发现)必须拒绝, 且不读入内存。
func TestReadFileBeneathLimitedRejectsOverLimit(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "over.bin", 1001)
	if _, err := ReadFileBeneathLimited(dir, "over.bin", 1000); err == nil {
		t.Fatal("over-limit read must fail")
	}
	// 上限内正常读取。
	buf, err := ReadFileBeneathLimited(dir, "over.bin", 4096)
	if err != nil || len(buf) != 1001 {
		t.Fatalf("within-limit read = %d bytes, err %v", len(buf), err)
	}
}

// 增长检测的错误哨兵必须可被 errors.Is 识别(调用方据此区分截断与超限)。
func TestReadFileBeneathLimitedTooLargeSentinel(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "grow.bin", 2000)
	_, err := ReadFileBeneathLimited(dir, "grow.bin", 1000)
	if err == nil || !strings.Contains(err.Error(), "exceeds size limit") {
		t.Fatalf("err = %v, want size-limit error", err)
	}
}
