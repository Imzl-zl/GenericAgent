package sandbox

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// prepareWorkspaceDirs(root, hash) 创建 root/hash/{memory,temp,state}。
// 首次创建且 memory 为空时从 template 初始化。
func TestPrepareWorkspaceDirsCreatesSubdirs(t *testing.T) {
	root := t.TempDir()
	hash := WorkspaceDirHash("personal:1")

	dirs, err := prepareWorkspaceDirs(root, hash, "", 10002, 10002, 10003)
	if err != nil {
		t.Fatalf("prepareWorkspaceDirs: %v", err)
	}
	for _, sub := range []string{"memory", "temp", "state", "config"} {
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

	dirs, err := prepareWorkspaceDirs(root, hash, tmpl, 10002, 10002, 10003)
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
	dirs, err := prepareWorkspaceDirs(root, hash, "", 10002, 10002, 10003)
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
	if _, err := prepareWorkspaceDirs(root, hash, tmpl, 10002, 10002, 10003); err != nil {
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
	if _, err := prepareWorkspaceDirs(root, "../../etc", "", 10002, 10002, 10003); err == nil {
		t.Fatal("path traversal hash must fail")
	}
}

// TestPrepareWorkspaceDirsReplacesSymlinkDirs 回归测试(审查): Runner 把
// 工作区子目录替换为指向工作区外路径的符号链接时, prepareWorkspaceDirs
// 必须删除链接并用目录重建, 而不是跟随链接修改外部目标。
func TestPrepareWorkspaceDirsReplacesSymlinkDirs(t *testing.T) {
	root := t.TempDir()
	hash := WorkspaceDirHash("personal:9")
	ws := filepath.Join(root, hash)

	// 模拟 Runner 已把 state/staging 替换为指向外部目录的符号链接。
	external := t.TempDir()
	if err := os.WriteFile(filepath.Join(external, "victim.txt"), []byte("victim"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(ws, "state"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(external, filepath.Join(ws, "state", "staging")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}

	if _, err := prepareWorkspaceDirs(root, hash, "", 10002, 10002, 10003); err != nil {
		t.Fatalf("prepareWorkspaceDirs must recover from symlink: %v", err)
	}
	staging := filepath.Join(ws, "state", "staging")
	info, err := os.Lstat(staging)
	if err != nil {
		t.Fatalf("staging missing: %v", err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		t.Fatal("staging is still a symlink")
	}
	if !info.IsDir() {
		t.Fatal("staging is not a directory")
	}
	// 外部目标必须保持原样(链接未被跟随写操作)。
	if data, err := os.ReadFile(filepath.Join(external, "victim.txt")); err != nil || string(data) != "victim" {
		t.Fatalf("external target modified: %q err=%v", data, err)
	}
}

// TestPrepareWorkspaceDirsFixesAncestorPermissions 回归测试(审查): 目标目录
// 的祖先链(如 temp 下的中间目录)也必须被预置, 不允许存在 Runner 无法
// 穿过的中间目录。
func TestPrepareWorkspaceDirsFixesAncestorPermissions(t *testing.T) {
	root := t.TempDir()
	hash := WorkspaceDirHash("personal:10")
	dirs, err := prepareWorkspaceDirs(root, hash, "", 10002, 10002, 10003)
	if err != nil {
		t.Fatal(err)
	}
	// temp/attachments 与 temp/outputs 的父链必须已创建为目录。
	for _, sub := range []string{
		filepath.Join(dirs.Temp, "attachments"),
		filepath.Join(dirs.Temp, "outputs"),
	} {
		info, err := os.Lstat(sub)
		if err != nil {
			t.Fatalf("missing %s: %v", sub, err)
		}
		if !info.IsDir() {
			t.Fatalf("%s is not a dir", sub)
		}
	}
}

// TestPrepareWorkspaceDirsSeedsMemoryAtomically 验证模板初始化采用 staging +
// rename: 复制失败时不得留下半成品(否则下次因目录非空跳过初始化); 成功时
// 不得残留 .memory-init-* 临时目录(审查 I13)。
func TestPrepareWorkspaceDirsSeedsMemoryAtomically(t *testing.T) {
	root := t.TempDir()
	hash := WorkspaceDirHash("personal:atomic")
	ws := filepath.Join(root, hash)

	// 失败路径: 模板路径不是目录(ReadDir 失败) → 报错且无残留。
	tmplFile := filepath.Join(t.TempDir(), "not-a-dir.txt")
	if err := os.WriteFile(tmplFile, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := prepareWorkspaceDirs(root, hash, tmplFile, 10002, 10002, 10003); err == nil {
		t.Fatal("invalid template must fail")
	}
	entries, err := os.ReadDir(filepath.Join(ws, "memory"))
	if err != nil {
		t.Fatalf("memory dir must still exist: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("failed init must leave memory empty, got %v", entries)
	}

	// 成功路径: 正常模板, 无 .memory-init-* 残留。
	tmpl := t.TempDir()
	if err := os.WriteFile(filepath.Join(tmpl, "base.txt"), []byte("v"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := prepareWorkspaceDirs(root, hash, tmpl, 10002, 10002, 10003); err != nil {
		t.Fatalf("prepareWorkspaceDirs: %v", err)
	}
	wsEntries, err := os.ReadDir(ws)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range wsEntries {
		if len(e.Name()) >= len(".memory-init-") && e.Name()[:len(".memory-init-")] == ".memory-init-" {
			t.Fatalf("staging dir leaked: %s", e.Name())
		}
	}
}

// TestWriteConfigFilesGroupReadable 验证 config 文件必须 0640(组可读):
// Platform(10001:10003 共享组)在凭证刷新时读取, 0600 会导致 EACCES
// (审查 I2)。chmod 语义在 Windows 不完整, 仅 unix 下断言。
func TestWriteConfigFilesGroupReadable(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("windows chmod 不保留 unix 权限位")
	}
	if os.Geteuid() != 0 {
		t.Log("non-root: ownership not asserted, mode still checked")
	}
	root := t.TempDir()
	hash := WorkspaceDirHash("personal:perm")
	// config/ 目录由 prepareWorkspaceDirs 预置(writeConfigFiles 只写文件)。
	if _, err := prepareWorkspaceDirs(root, hash, "", 10002, 10002, 10003); err != nil {
		t.Fatal(err)
	}
	if err := writeConfigFiles(root, hash, map[string][]byte{"policy.json": []byte("{}")}, 10002, 10002, 10003); err != nil {
		t.Fatalf("writeConfigFiles: %v", err)
	}
	info, err := os.Stat(filepath.Join(root, hash, "config", "policy.json"))
	if err != nil {
		t.Fatal(err)
	}
	perm := info.Mode().Perm()
	if perm&0o004 == 0 {
		t.Fatalf("config file mode %o: world must not be readable", perm)
	}
	if perm&0o040 == 0 {
		t.Fatalf("config file mode %o: group must be readable (Platform refresh reads it)", perm)
	}
	if perm&0o002 != 0 {
		t.Fatalf("config file mode %o: world writable", perm)
	}
}
