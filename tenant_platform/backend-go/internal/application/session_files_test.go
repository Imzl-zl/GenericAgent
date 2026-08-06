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
