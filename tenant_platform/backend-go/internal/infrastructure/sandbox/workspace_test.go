package sandbox

import (
	"os"
	"path/filepath"
	"testing"
)

// prepareWorkspaceDirs(root, hash) 创建 root/hash/{memory,temp,state}。
// 首次创建且 memory 为空时从 template 初始化。
func TestPrepareWorkspaceDirsCreatesSubdirs(t *testing.T) {
	root := t.TempDir()
	hash := WorkspaceDirHash("personal:1")

	dirs, err := prepareWorkspaceDirs(root, hash, "", 10002, 10002)
	if err != nil {
		t.Fatalf("prepareWorkspaceDirs: %v", err)
	}
	for _, sub := range []string{"memory", "temp", "state", "config", "attachments"} {
		info, err := os.Stat(filepath.Join(dirs.Workspace, sub))
		if err != nil {
			t.Fatalf("missing %s: %v", sub, err)
		}
		if !info.IsDir() {
			t.Fatalf("%s is not a dir", sub)
		}
	}
}

func TestPrepareWorkspaceDirsSeedsMemoryFromTemplate(t *testing.T) {
	root := t.TempDir()
	hash := WorkspaceDirHash("personal:2")

	// 构造一个 2 文件的模板目录。
	tmpl := t.TempDir()
	if err := os.MkdirAll(filepath.Join(tmpl, "sops"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tmpl, "global_mem.txt"), []byte("baseline"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tmpl, "sops", "default.md"), []byte("# default"), 0o644); err != nil {
		t.Fatal(err)
	}

	dirs, err := prepareWorkspaceDirs(root, hash, tmpl, 10002, 10002)
	if err != nil {
		t.Fatalf("prepareWorkspaceDirs: %v", err)
	}
	memFile := filepath.Join(dirs.Workspace, "memory", "global_mem.txt")
	data, err := os.ReadFile(memFile)
	if err != nil {
		t.Fatalf("seeded memory missing: %v", err)
	}
	if string(data) != "baseline" {
		t.Fatalf("seeded content = %q, want baseline", string(data))
	}
	if _, err := os.Stat(filepath.Join(dirs.Workspace, "memory", "sops", "default.md")); err != nil {
		t.Fatalf("nested seeded file missing: %v", err)
	}
	// temp 不应被初始化。
	if _, err := os.Stat(filepath.Join(dirs.Workspace, "temp", "global_mem.txt")); !os.IsNotExist(err) {
		t.Fatal("temp must not be seeded from template")
	}
}

func TestPrepareWorkspaceDirsDoesNotOverwriteExistingMemory(t *testing.T) {
	root := t.TempDir()
	hash := WorkspaceDirHash("personal:3")

	// 预先创建带用户内容的 memory。
	dirs, err := prepareWorkspaceDirs(root, hash, "", 10002, 10002)
	if err != nil {
		t.Fatal(err)
	}
	userMem := filepath.Join(dirs.Workspace, "memory", "global_mem.txt")
	if err := os.WriteFile(userMem, []byte("user data"), 0o644); err != nil {
		t.Fatal(err)
	}

	// 模板存在也不得覆盖已修改 memory。
	tmpl := t.TempDir()
	if err := os.WriteFile(filepath.Join(tmpl, "global_mem.txt"), []byte("template"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := prepareWorkspaceDirs(root, hash, tmpl, 10002, 10002); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(userMem)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "user data" {
		t.Fatalf("user memory overwritten: %q", string(data))
	}
}

func TestPrepareWorkspaceDirsRejectsTraversalHash(t *testing.T) {
	root := t.TempDir()
	if _, err := prepareWorkspaceDirs(root, "../../etc", "", 10002, 10002); err == nil {
		t.Fatal("path traversal hash must fail")
	}
}
