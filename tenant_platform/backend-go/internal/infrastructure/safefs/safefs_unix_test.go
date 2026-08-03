//go:build unix

package safefs

import (
	"os"
	"path/filepath"
	"testing"
)

func TestReadFileBeneathRejectsSymlinkComponents(t *testing.T) {
	root := t.TempDir()
	// 中间目录替换为指向 root 外部的符号链接。
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "state", "staging"), 0o755); err != nil {
		t.Fatal(err)
	}
	// 先移除原目录再建符号链接(直接 Symlink 会 EEXIST)。
	if err := os.Remove(filepath.Join(root, "state", "staging")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "state", "staging")); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadFileBeneath(root, "state/staging/secret.txt"); err == nil {
		t.Fatal("symlinked intermediate directory must be rejected")
	}
}

func TestReadFileBeneathRejectsFinalSymlink(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "secret.txt"), filepath.Join(root, "link.txt")); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadFileBeneath(root, "link.txt"); err == nil {
		t.Fatal("final symlink must be rejected")
	}
}

func TestAtomicWriteBeneathRejectsSymlinkedParent(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "committed")); err != nil {
		t.Fatal(err)
	}
	if err := AtomicWriteBeneath(root, "committed/x.json", []byte("{}"), 0o640); err == nil {
		t.Fatal("write through symlinked parent must be rejected")
	}
	if _, err := os.Stat(filepath.Join(outside, "x.json")); err == nil {
		t.Fatal("file must not be created outside root")
	}
}

func TestAtomicWriteBeneathRoundTrip(t *testing.T) {
	root := t.TempDir()
	if err := AtomicWriteBeneath(root, "state/results/r.result", []byte("body"), 0o640); err != nil {
		t.Fatal(err)
	}
	got, err := ReadFileBeneath(root, "state/results/r.result")
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "body" {
		t.Fatalf("round trip = %q", got)
	}
}

func TestCleanRelRejectsEscapes(t *testing.T) {
	for _, rel := range []string{"../x", "a/../../b", "/abs", "a/b/../../.."} {
		if _, err := CleanRel("/root", rel); err == nil {
			t.Fatalf("%q must be rejected", rel)
		}
	}
}

func TestReadFileBeneathLimitedRejectsOversizeBeforeRead(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "state"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "state", "big.bin"), make([]byte, 4096), 0o640); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadFileBeneathLimited(root, "state/big.bin", 1024); err == nil {
		t.Fatal("oversize file must be rejected without reading")
	}
	got, err := ReadFileBeneathLimited(root, "state/big.bin", 8192)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 4096 {
		t.Fatalf("read %d bytes, want 4096", len(got))
	}
}
