package application

import (
	"time"
	"os"
	"path/filepath"
	"testing"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/domain"
)

// mustSandboxRoot 断言 SandboxRoot 成功并返回根路径(签名带 error 后测试
// 便捷入口, 审查 B1: 非法 sessionKey 显式失败)。
func mustSandboxRoot(t *testing.T, sf SessionFiles, sessionKey string) string {
	t.Helper()
	root, err := sf.SandboxRoot(sessionKey)
	if err != nil {
		t.Fatalf("SandboxRoot(%q): %v", sessionKey, err)
	}
	return root
}

// mustSessionDirHash 与 domain.WorkspaceDirHash 同源, 供测试构造预期目录。
func mustSessionDirHash(t *testing.T, sessionKey string) string {
	t.Helper()
	hash, err := domain.WorkspaceDirHash(sessionKey)
	if err != nil {
		t.Fatalf("WorkspaceDirHash(%q): %v", sessionKey, err)
	}
	return hash
}

// TestWorkspaceSessionFilesFreshWorkspaceImportInbound 验证 fresh workspace
// (目录布局尚不存在) 的首条带附件消息: ImportInbound 必须先调用 ensure
// 回调预置布局, 再成功落盘附件(方案 §6 审查 C5)。
func TestWorkspaceSessionFilesFreshWorkspaceImportInbound(t *testing.T) {
	root := t.TempDir()
	var ensured []string
	files, err := NewWorkspaceSessionFiles(root, "", func(sessionKey string) error {
		ensured = append(ensured, sessionKey)
		// 模拟 Manager 预置布局: 创建 SandboxRoot 链路。
		return os.MkdirAll(filepath.Join(root, mustSessionDirHash(t, sessionKey), "temp",
			sessionFilesDirName, mustSessionDirHash(t, sessionKey)), 0o770)
	})
	if err != nil {
		t.Fatalf("NewWorkspaceSessionFiles: %v", err)
	}

	src := filepath.Join(t.TempDir(), "doc.txt")
	if err := os.WriteFile(src, []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	refs, err := files.ImportInbound("personal:1", []string{src})
	if err != nil {
		t.Fatalf("ImportInbound on fresh workspace must succeed after ensure: %v", err)
	}
	if len(ensured) != 1 || ensured[0] != "personal:1" {
		t.Fatalf("ensure callback calls = %v, want [personal:1]", ensured)
	}
	if len(refs) != 1 {
		t.Fatalf("imported refs = %d, want 1", len(refs))
	}
	abs := filepath.Join(mustSandboxRoot(t, files, "personal:1"), filepath.FromSlash(refs[0].RelativePath))
	body, err := os.ReadFile(abs)
	if err != nil {
		t.Fatalf("read imported attachment: %v", err)
	}
	if string(body) != "hello" {
		t.Fatalf("attachment body = %q, want hello", body)
	}
}

// TestWorkspaceSessionFilesEnsureFailureFailsImport 验证 ensure 失败时
// 附件导入必须显式失败(不允许在未初始化的目录上静默写文件)。
func TestWorkspaceSessionFilesEnsureFailureFailsImport(t *testing.T) {
	root := t.TempDir()
	files, err := NewWorkspaceSessionFiles(root, "", func(sessionKey string) error {
		return os.ErrPermission
	})
	if err != nil {
		t.Fatalf("NewWorkspaceSessionFiles: %v", err)
	}
	src := filepath.Join(t.TempDir(), "doc.txt")
	if err := os.WriteFile(src, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := files.ImportInbound("personal:2", []string{src}); err == nil {
		t.Fatal("ImportInbound must fail when ensure fails")
	}
}

// TestRecordOutboundRejectsNonOutputsEvenIfInManifest 验证 Runner 伪造
// manifest 条目(把 attachments/ 或 temp/ 下文件登记为出站)时,
// RecordOutbound 必须先校验 outputs/ 前缀再查 manifest(审查 R4-I7)。
func TestRecordOutboundRejectsNonOutputsEvenIfInManifest(t *testing.T) {
	root := t.TempDir()
	files, err := NewWorkspaceSessionFiles(root, "", func(sessionKey string) error {
		return os.MkdirAll(filepath.Join(root, mustSessionDirHash(t, sessionKey), "temp"), 0o770)
	})
	if err != nil {
		t.Fatalf("NewWorkspaceSessionFiles: %v", err)
	}
	const sessionKey = "personal:3"
	sandbox := mustSandboxRoot(t, files, sessionKey)
	if err := os.MkdirAll(filepath.Join(sandbox, sessionAttachmentsDir), 0o770); err != nil {
		t.Fatal(err)
	}
	// 伪造: 附件目录下的文件 + manifest 中已有该条目。
	sneaky := filepath.Join(sandbox, sessionAttachmentsDir, "F001_secret.txt")
	if err := os.WriteFile(sneaky, []byte("secret"), 0o640); err != nil {
		t.Fatal(err)
	}
	fakeManifest := sessionManifest{
		NextSeq: 1,
		Files: []SessionFileRef{{
			Alias: "F001", OriginalName: "secret.txt",
			RelativePath: filepath.ToSlash(filepath.Join(sessionAttachmentsDir, "F001_secret.txt")),
			Direction:    "outbound", CreatedAt: time.Now().UTC(),
		}},
	}
	if err := files.(*sessionFilesManager).saveManifest(sandbox, fakeManifest); err != nil {
		t.Fatalf("save fake manifest: %v", err)
	}
	marker := filepath.ToSlash(filepath.Join(sessionAttachmentsDir, "F001_secret.txt"))
	if _, err := files.RecordOutbound(sessionKey, marker); err == nil {
		t.Fatal("RecordOutbound must reject a manifest-listed file outside outputs/")
	}
}

// TestRecordOutboundAcceptsOutputsFile 验证 outputs/ 下文件的正常登记路径。
func TestRecordOutboundAcceptsOutputsFile(t *testing.T) {
	root := t.TempDir()
	files, err := NewWorkspaceSessionFiles(root, "", func(sessionKey string) error {
		return os.MkdirAll(filepath.Join(root, mustSessionDirHash(t, sessionKey), "temp"), 0o770)
	})
	if err != nil {
		t.Fatalf("NewWorkspaceSessionFiles: %v", err)
	}
	const sessionKey = "personal:4"
	sandbox := mustSandboxRoot(t, files, sessionKey)
	if err := os.MkdirAll(filepath.Join(sandbox, sessionOutputsDir), 0o770); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(sandbox, sessionOutputsDir, "report.docx")
	if err := os.WriteFile(out, []byte("doc"), 0o640); err != nil {
		t.Fatal(err)
	}
	ref, err := files.RecordOutbound(sessionKey, "outputs/report.docx")
	if err != nil {
		t.Fatalf("RecordOutbound outputs/ file: %v", err)
	}
	if ref.RelativePath != "outputs/report.docx" {
		t.Fatalf("ref path = %q, want outputs/report.docx", ref.RelativePath)
	}
	if ref.Direction != "outbound" {
		t.Fatalf("ref direction = %q, want outbound", ref.Direction)
	}
}

// TestLoadManifestRejectsOversize 验证 manifest.json 超限时拒绝读取
// (审查 R4-C4: Runner 可写 temp/ 内的 manifest 不得无界读入 Platform 内存)。
func TestLoadManifestRejectsOversize(t *testing.T) {
	root := t.TempDir()
	files, err := NewWorkspaceSessionFiles(root, "", func(sessionKey string) error {
		return os.MkdirAll(filepath.Join(root, mustSessionDirHash(t, sessionKey), "temp"), 0o770)
	})
	if err != nil {
		t.Fatalf("NewWorkspaceSessionFiles: %v", err)
	}
	const sessionKey = "personal:5"
	sandbox := mustSandboxRoot(t, files, sessionKey)
	if err := os.MkdirAll(sandbox, 0o770); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sandbox, sessionManifestName),
		[]byte("{\"next_seq\":1,\"files\":["+"\"x\","+string(make([]byte, sessionManifestMaxBytes))+ "]}"), 0o640); err != nil {
		t.Fatal(err)
	}
	if _, err := files.Recent(sessionKey, 8); err == nil {
		t.Fatal("Recent must fail when manifest exceeds size limit")
	}
	if _, err := files.RecordOutbound(sessionKey, "outputs/x"); err == nil {
		t.Fatal("RecordOutbound must fail when manifest exceeds size limit")
	}
}

// TestLoadManifestRejectsUnsafeOriginalName 验证 manifest 条目 OriginalName
// 含路径分隔符/.. 时(值源于 Runner 可写的 manifest, 审查 C1), 读取/登记必须
// 失败——该值会流入交付快照文件名与用户可见 displayName。
func TestLoadManifestRejectsUnsafeOriginalName(t *testing.T) {
	root := t.TempDir()
	files, err := NewWorkspaceSessionFiles(root, "", func(sessionKey string) error {
		return os.MkdirAll(filepath.Join(root, mustSessionDirHash(t, sessionKey), "temp"), 0o770)
	})
	if err != nil {
		t.Fatalf("NewWorkspaceSessionFiles: %v", err)
	}
	const sessionKey = "personal:7"
	sandbox := mustSandboxRoot(t, files, sessionKey)
	if err := os.MkdirAll(filepath.Join(sandbox, sessionOutputsDir), 0o770); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sandbox, sessionOutputsDir, "ok.txt"), []byte("x"), 0o640); err != nil {
		t.Fatal(err)
	}
	bad := sessionManifest{NextSeq: 1, Files: []SessionFileRef{{
		Alias: "F001", OriginalName: "../../../evil.txt",
		RelativePath: "outputs/ok.txt", Direction: "outbound", CreatedAt: time.Now().UTC(),
	}}}
	if err := files.(*sessionFilesManager).saveManifest(sandbox, bad); err != nil {
		t.Fatalf("save fake manifest: %v", err)
	}
	if _, err := files.RecordOutbound(sessionKey, "outputs/ok.txt"); err == nil {
		t.Fatal("RecordOutbound must reject manifest with path-traversal OriginalName")
	}
}

// TestLoadManifestRejectsEscapingRelativePath 验证 manifest 条目 RelativePath
// 逃逸 outputs/(如 ../../x)时读取/登记必须失败。
func TestLoadManifestRejectsEscapingRelativePath(t *testing.T) {
	root := t.TempDir()
	files, err := NewWorkspaceSessionFiles(root, "", func(sessionKey string) error {
		return os.MkdirAll(filepath.Join(root, mustSessionDirHash(t, sessionKey), "temp"), 0o770)
	})
	if err != nil {
		t.Fatalf("NewWorkspaceSessionFiles: %v", err)
	}
	const sessionKey = "personal:8"
	sandbox := mustSandboxRoot(t, files, sessionKey)
	if err := os.MkdirAll(filepath.Join(sandbox, sessionOutputsDir), 0o770); err != nil {
		t.Fatal(err)
	}
	bad := sessionManifest{NextSeq: 1, Files: []SessionFileRef{{
		Alias: "F001", OriginalName: "ok.txt",
		RelativePath: "../../../etc/passwd", Direction: "outbound", CreatedAt: time.Now().UTC(),
	}}}
	if err := files.(*sessionFilesManager).saveManifest(sandbox, bad); err != nil {
		t.Fatalf("save fake manifest: %v", err)
	}
	if _, err := files.Recent(sessionKey, 8); err == nil {
		t.Fatal("Recent must reject manifest with escaping RelativePath")
	}
}

// TestLoadManifestRejectsMidPathDotDot 验证 manifest RelativePath 含中段
// ..(如 outputs/../../x)时拒绝(审查 C1 收紧: Clean 后仍逃出沙箱)。
func TestLoadManifestRejectsMidPathDotDot(t *testing.T) {
	root := t.TempDir()
	files, err := NewWorkspaceSessionFiles(root, "", func(sessionKey string) error {
		return os.MkdirAll(filepath.Join(root, mustSessionDirHash(t, sessionKey), "temp"), 0o770)
	})
	if err != nil {
		t.Fatalf("NewWorkspaceSessionFiles: %v", err)
	}
	const sessionKey = "personal:9"
	sandbox := mustSandboxRoot(t, files, sessionKey)
	if err := os.MkdirAll(filepath.Join(sandbox, sessionOutputsDir), 0o770); err != nil {
		t.Fatal(err)
	}
	bad := sessionManifest{NextSeq: 1, Files: []SessionFileRef{{
		Alias: "F001", OriginalName: "ok.txt",
		RelativePath: "outputs/../../etc/passwd", Direction: "outbound", CreatedAt: time.Now().UTC(),
	}}}
	if err := files.(*sessionFilesManager).saveManifest(sandbox, bad); err != nil {
		t.Fatalf("save fake manifest: %v", err)
	}
	if _, err := files.Recent(sessionKey, 8); err == nil {
		t.Fatal("Recent must reject manifest with mid-path .. traversal")
	}
}

// TestRecentSinceFiltersOldRefs 验证会话引用隔离: since 之后的文件注入,
// 之前的过滤(2026-08-15: 旧会话附件曾注入新会话 prompt 导致误转旧文件)。
func TestRecentSinceFiltersOldRefs(t *testing.T) {
	root := t.TempDir()
	files, err := NewWorkspaceSessionFiles(root, "", func(sessionKey string) error {
		return os.MkdirAll(filepath.Join(root, mustSessionDirHash(t, sessionKey), "temp",
			sessionFilesDirName, mustSessionDirHash(t, sessionKey)), 0o770)
	})
	if err != nil {
		t.Fatalf("NewWorkspaceSessionFiles: %v", err)
	}
	srcA := filepath.Join(t.TempDir(), "old.txt")
	if err := os.WriteFile(srcA, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	refsA, err := files.ImportInbound("personal:1", []string{srcA})
	if err != nil || len(refsA) != 1 {
		t.Fatalf("import old: refs=%v err=%v", refsA, err)
	}
	// 模拟 /new: cutoff 取两次导入之间, 之后导入的附件属于新会话。
	cutoff := time.Now().UTC()
	time.Sleep(20 * time.Millisecond)
	srcB := filepath.Join(t.TempDir(), "new.md")
	if err := os.WriteFile(srcB, []byte("# new"), 0o644); err != nil {
		t.Fatal(err)
	}
	refsB, err := files.ImportInbound("personal:1", []string{srcB})
	if err != nil || len(refsB) != 1 {
		t.Fatalf("import new: refs=%v err=%v", refsB, err)
	}

	all, err := files.Recent("personal:1", 8)
	if err != nil {
		t.Fatalf("Recent: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("Recent(no filter) = %d refs, want 2", len(all))
	}
	filtered, err := files.RecentSince("personal:1", 8, cutoff)
	if err != nil {
		t.Fatalf("RecentSince: %v", err)
	}
	if len(filtered) != 1 || filtered[0].RelativePath != refsB[0].RelativePath {
		t.Fatalf("RecentSince(cutoff) = %+v, want only %s", filtered, refsB[0].RelativePath)
	}
	// 零值 since = 不过滤(兼容未 /new 场景)。
	filteredAll, err := files.RecentSince("personal:1", 8, time.Time{})
	if err != nil || len(filteredAll) != 2 {
		t.Fatalf("RecentSince(zero) = %d refs err=%v, want 2", len(filteredAll), err)
	}
}

// TestPruneInboundBeforeRemovesOldAttachments 验证 /new 物理清理旧会话附件
// (inbound 文件删除 + manifest 移除; outbound 产物保留磁盘)。
func TestPruneInboundBeforeRemovesOldAttachments(t *testing.T) {
	root := t.TempDir()
	files, err := NewWorkspaceSessionFiles(root, "", func(sessionKey string) error {
		return os.MkdirAll(filepath.Join(root, mustSessionDirHash(t, sessionKey), "temp",
			sessionFilesDirName, mustSessionDirHash(t, sessionKey)), 0o770)
	})
	if err != nil {
		t.Fatalf("NewWorkspaceSessionFiles: %v", err)
	}
	sandbox := mustSandboxRoot(t, files, "personal:1")
	srcA := filepath.Join(t.TempDir(), "old.txt")
	if err := os.WriteFile(srcA, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	refsA, err := files.ImportInbound("personal:1", []string{srcA})
	if err != nil || len(refsA) != 1 {
		t.Fatalf("import old: %v err=%v", refsA, err)
	}
	oldAbs := filepath.Join(sandbox, filepath.FromSlash(refsA[0].RelativePath))
	// outbound 产物(模拟): 手动登记一个 outputs 文件, 不应被 prune。
	outAbs := filepath.Join(sandbox, "outputs", "keep.docx")
	if err := os.MkdirAll(filepath.Dir(outAbs), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(outAbs, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	outRef, err := files.RecordOutbound("personal:1", "outputs/keep.docx")
	if err != nil {
		t.Fatalf("RecordOutbound: %v", err)
	}

	cutoff := time.Now().UTC()
	time.Sleep(20 * time.Millisecond)
	if err := files.PruneInboundBefore("personal:1", cutoff); err != nil {
		t.Fatalf("PruneInboundBefore: %v", err)
	}
	if _, err := os.Stat(oldAbs); !os.IsNotExist(err) {
		t.Fatalf("old attachment must be deleted, stat err=%v", err)
	}
	if _, err := os.Stat(outAbs); err != nil {
		t.Fatalf("outbound product must survive prune: %v", err)
	}
	refs, err := files.Recent("personal:1", 8)
	if err != nil {
		t.Fatalf("Recent: %v", err)
	}
	if len(refs) != 1 || refs[0].RelativePath != outRef.RelativePath {
		t.Fatalf("manifest after prune = %+v, want only %s", refs, outRef.RelativePath)
	}
	// 零值 before = no-op。
	if err := files.PruneInboundBefore("personal:1", time.Time{}); err != nil {
		t.Fatalf("PruneInboundBefore(zero): %v", err)
	}
}
