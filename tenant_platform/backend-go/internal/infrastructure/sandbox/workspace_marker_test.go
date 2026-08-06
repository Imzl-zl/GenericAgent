package sandbox

import (
	"os"
	"path/filepath"
	"testing"
	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/domain"
)

// ---------------------------------------------------------------------------
// round11 审查 I7: 首次初始化判定改用工作区根的标记文件, 而非 memory 目录
// 是否为空——用户主动清空 memory 后不得重新灌入模板。
// ---------------------------------------------------------------------------

// TestPrepareWorkspaceDirsDoesNotReseedAfterUserClearsMemory is the core
// regression: after the template is seeded once, the user deletes every file
// in memory/; a later prepare must NOT re-seed the template.
func TestPrepareWorkspaceDirsDoesNotReseedAfterUserClearsMemory(t *testing.T) {
	root := t.TempDir()
	hash, err := domain.WorkspaceDirHash("personal:4")
	if err != nil {
		t.Fatal(err)
	}
	tmpl := t.TempDir()
	if err := os.WriteFile(filepath.Join(tmpl, "global_mem.txt"), []byte("template"), 0o644); err != nil {
		t.Fatal(err)
	}

	dirs, err := prepareWorkspaceDirs(root, hash, tmpl, 10002, 10002, 10003)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dirs.Memory, "global_mem.txt")); err != nil {
		t.Fatalf("first run must seed the template: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dirs.Workspace, memoryInitMarkerName)); err != nil {
		t.Fatalf("init marker must be written: %v", err)
	}

	// 用户清空 memory(目录保留但为空)。
	if err := os.Remove(filepath.Join(dirs.Memory, "global_mem.txt")); err != nil {
		t.Fatal(err)
	}

	dirs2, err := prepareWorkspaceDirs(root, hash, tmpl, 10002, 10002, 10003)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dirs2.Memory, "global_mem.txt")); !os.IsNotExist(err) {
		t.Fatalf("template must NOT be re-seeded after user cleared memory, stat err=%v", err)
	}
}

// TestPrepareWorkspaceDirsBackfillsMarkerWithoutReseed covers the legacy
// layout: memory non-empty but no marker (workspace initialized before the
// marker existed) — backfill the marker without re-seeding or overwriting.
func TestPrepareWorkspaceDirsBackfillsMarkerWithoutReseed(t *testing.T) {
	root := t.TempDir()
	hash, err := domain.WorkspaceDirHash("personal:5")
	if err != nil {
		t.Fatal(err)
	}
	dirs, err := prepareWorkspaceDirs(root, hash, "", 10002, 10002, 10003)
	if err != nil {
		t.Fatal(err)
	}
	userMem := filepath.Join(dirs.Memory, "global_mem.txt")
	if err := os.WriteFile(userMem, []byte("user data"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dirs.Workspace, memoryInitMarkerName)); !os.IsNotExist(err) {
		t.Fatal("marker must not exist yet (legacy layout)")
	}

	tmpl := t.TempDir()
	if err := os.WriteFile(filepath.Join(tmpl, "global_mem.txt"), []byte("template"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := prepareWorkspaceDirs(root, hash, tmpl, 10002, 10002, 10003); err != nil {
		t.Fatal(err)
	}
	// 模板不得覆盖用户数据。
	data, err := os.ReadFile(userMem)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "user data" {
		t.Fatalf("user memory overwritten: %q", string(data))
	}
	// 标记已补写, 后续清空不再重灌。
	if _, err := os.Stat(filepath.Join(dirs.Workspace, memoryInitMarkerName)); err != nil {
		t.Fatalf("marker must be backfilled: %v", err)
	}
}
