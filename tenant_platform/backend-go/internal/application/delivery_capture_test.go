package application

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestCaptureTaskDeliverableFilesSnapshotsMarkers 验证审查 R5-I3 核心:
// 成功事务前捕获 [FILE:...] 标记文件内容(任务完成时刻), 返回 digest/大小
// 与内容; 无标记时返回 nil 不阻断。
func TestCaptureTaskDeliverableFilesSnapshotsMarkers(t *testing.T) {
	root := t.TempDir()
	files, err := NewSessionFiles(root, "")
	if err != nil {
		t.Fatal(err)
	}
	outDir := filepath.Join(mustSandboxRoot(t, files, "personal:1"), "outputs")
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outDir, "report.docx"), []byte("task-a-content"), 0o644); err != nil {
		t.Fatal(err)
	}
	body := "整理好了。\n[FILE:outputs/report.docx]"
	captured, err := captureTaskDeliverableFiles(context.Background(), files, "personal:1", "", body)
	if err != nil {
		t.Fatalf("capture: %v", err)
	}
	if len(captured) != 1 {
		t.Fatalf("captured = %d, want 1", len(captured))
	}
	f := captured[0]
	if f.Marker != "outputs/report.docx" || f.FileName != "report.docx" {
		t.Fatalf("marker/file name = %q/%q", f.Marker, f.FileName)
	}
	if string(f.Content) != "task-a-content" {
		t.Fatalf("content = %q", f.Content)
	}
	if f.SizeBytes != int64(len("task-a-content")) || !strings.HasPrefix(f.Digest, "sha256:") {
		t.Fatalf("size/digest = %d/%q", f.SizeBytes, f.Digest)
	}

	// 无标记: 返回 nil 不阻断成功。
	nilFiles, err := captureTaskDeliverableFiles(context.Background(), files, "personal:1", "", "无文件的结果")
	if err != nil || nilFiles != nil {
		t.Fatalf("no-marker capture = %v, %v; want nil", nilFiles, err)
	}
	// SessionFiles 未接线: nil 不阻断。
	if v, err := captureTaskDeliverableFiles(context.Background(), nil, "personal:1", "", "[FILE:outputs/x]"); err != nil || v != nil {
		t.Fatalf("nil session files = %v, %v; want nil", v, err)
	}
}

// TestCaptureTaskDeliverableFilesFailClosed 验证捕获失败 fail-closed:
// marker 文件缺失/非 outputs/超限时返回错误, 成功事务不得提交。
func TestCaptureTaskDeliverableFilesFailClosed(t *testing.T) {
	root := t.TempDir()
	files, err := NewSessionFiles(root, "")
	if err != nil {
		t.Fatal(err)
	}
	sandbox := mustSandboxRoot(t, files, "personal:2")
	// 缺失文件。
	if _, err := captureTaskDeliverableFiles(context.Background(), files, "personal:2", "", "[FILE:outputs/missing.docx]"); err == nil {
		t.Fatal("missing marker file must fail capture")
	}
	// 非 outputs/ 前缀(Runner 伪造 marker 指向附件/其他路径)。
	att := filepath.Join(sandbox, "attachments", "evil.txt")
	if err := os.MkdirAll(filepath.Dir(att), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(att, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := captureTaskDeliverableFiles(context.Background(), files, "personal:2", "", "[FILE:attachments/evil.txt]"); err == nil {
		t.Fatal("non-outputs marker must fail capture")
	}
	// 超限文件(大于 8MiB 上限)。
	big := filepath.Join(sandbox, "outputs", "big.bin")
	if err := os.MkdirAll(filepath.Dir(big), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(big, make([]byte, defaultMaxDeliverableBytes+1), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := captureTaskDeliverableFiles(context.Background(), files, "personal:2", "", "[FILE:outputs/big.bin]"); err == nil {
		t.Fatal("oversized marker file must fail capture")
	}
}

// 审查 R5-I5: 交付文件除单文件 8MiB 上限外, 还必须有任务级 marker 数量与
// 总字节上限——大量小文件可累计数十 GB 内存/事务压力。
func TestCaptureTaskDeliverableFilesRejectsAggregateOverflow(t *testing.T) {
	root := t.TempDir()
	files, err := NewSessionFiles(root, "")
	if err != nil {
		t.Fatal(err)
	}
	sandbox := mustSandboxRoot(t, files, "personal:3")
	outDir := filepath.Join(sandbox, "outputs")
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeMarker := func(name string) string {
		if err := os.WriteFile(filepath.Join(outDir, name), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		return "[FILE:outputs/" + name + "]"
	}
	// 超 marker 数量: 每个文件 1 字节, 数量超上限即拒绝。
	var many strings.Builder
	for i := 0; i <= maxDeliverableFiles; i++ {
		many.WriteString(writeMarker("f" + strconv.Itoa(i) + ".txt"))
	}
	if _, err := captureTaskDeliverableFiles(context.Background(), files, "personal:3", "", many.String()); err == nil {
		t.Fatal("capture with too many markers must fail")
	}
	// 超总字节: 数量在上限内, 但合计超过 maxTotalDeliverableBytes。
	if maxDeliverableFiles < 9 {
		t.Fatal("test assumes maxDeliverableFiles >= 9")
	}
	perFile := maxTotalDeliverableBytes/8 + 1 // 9 个合计 > 上限, 单个远小于单文件上限
	var total strings.Builder
	for i := 0; i < 9; i++ {
		name := "big" + strconv.Itoa(i) + ".bin"
		if err := os.WriteFile(filepath.Join(outDir, name), make([]byte, perFile), 0o644); err != nil {
			t.Fatal(err)
		}
		total.WriteString("[FILE:outputs/" + name + "]")
	}
	if _, err := captureTaskDeliverableFiles(context.Background(), files, "personal:3", "", total.String()); err == nil {
		t.Fatal("capture with aggregate byte overflow must fail")
	}
	// 上限内(数量与字节都满足): 成功。
	var okMarkers strings.Builder
	for i := 0; i < maxDeliverableFiles; i++ {
		okMarkers.WriteString(writeMarker("ok" + strconv.Itoa(i) + ".txt"))
	}
	captured, err := captureTaskDeliverableFiles(context.Background(), files, "personal:3", "", okMarkers.String())
	if err != nil {
		t.Fatalf("capture within aggregate limits: %v", err)
	}
	if len(captured) != maxDeliverableFiles {
		t.Fatalf("captured = %d, want %d", len(captured), maxDeliverableFiles)
	}
}

func TestCaptureDeliverableFilesToSpool(t *testing.T) {
	files, err := NewWorkspaceSessionFiles(t.TempDir(), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	sandbox := mustSandboxRoot(t, files, "personal:1")
	if err := os.MkdirAll(filepath.Join(sandbox, "outputs"), 0o755); err != nil {
		t.Fatal(err)
	}
	doc := []byte("docx-content")
	if err := os.WriteFile(filepath.Join(sandbox, "outputs", "report.docx"), doc, 0o644); err != nil {
		t.Fatal(err)
	}
	spoolDir := t.TempDir()
	captured, err := captureTaskDeliverableFiles(context.Background(), files, "personal:1", spoolDir,
		"完成\n[FILE:outputs/report.docx]")
	if err != nil {
		t.Fatalf("capture: %v", err)
	}
	if len(captured) != 1 {
		t.Fatalf("captured %d files, want 1", len(captured))
	}
	f := captured[0]
	if f.SpoolPath == "" {
		t.Fatal("SpoolPath must be set for spool capture")
	}
	if f.Content != nil {
		t.Fatal("Content must be empty for spool capture")
	}
	if f.SizeBytes != int64(len(doc)) {
		t.Fatalf("SizeBytes = %d, want %d", f.SizeBytes, len(doc))
	}
	if !strings.HasPrefix(f.Digest, "sha256:") {
		t.Fatalf("Digest = %q", f.Digest)
	}
	// spool 文件真实存在且内容一致。
	spoolAbs := filepath.Join(spoolDir, filepath.FromSlash(f.SpoolPath))
	got, err := os.ReadFile(spoolAbs)
	if err != nil {
		t.Fatalf("read spool file: %v", err)
	}
	if string(got) != string(doc) {
		t.Fatalf("spool content mismatch")
	}
	// 原文件后续被覆盖不影响 spool 快照(审查 R5-I3 语义)。
	if err := os.WriteFile(filepath.Join(sandbox, "outputs", "report.docx"), []byte("overwritten"), 0o644); err != nil {
		t.Fatal(err)
	}
	got2, _ := os.ReadFile(spoolAbs)
	if string(got2) != string(doc) {
		t.Fatal("spool snapshot must be immutable after capture")
	}
}

func TestCapturePerTypeSizeLimits(t *testing.T) {
	files, err := NewWorkspaceSessionFiles(t.TempDir(), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	sandbox := mustSandboxRoot(t, files, "personal:1")
	spoolDir := t.TempDir()
	write := func(name string, size int) {
		if err := os.MkdirAll(filepath.Join(sandbox, "outputs"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(sandbox, "outputs", name), make([]byte, size), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// file 类型仍限 8MiB(原默认)。
	write("big.bin", 9<<20)
	if _, err := captureTaskDeliverableFiles(context.Background(), files, "personal:1", spoolDir,
		"[FILE:outputs/big.bin]"); err == nil {
		t.Fatal("9MiB .bin must exceed file-type limit")
	}
	// video 类型放宽到 100MiB(Phase C 视频, 审查 B4/T5)。
	write("clip.mp4", 50<<20)
	captured, err := captureTaskDeliverableFiles(context.Background(), files, "personal:1", spoolDir,
		"[FILE:outputs/clip.mp4]")
	if err != nil {
		t.Fatalf("50MiB video capture failed: %v", err)
	}
	if captured[0].SizeBytes != 50<<20 {
		t.Fatalf("video size = %d", captured[0].SizeBytes)
	}
	// image 类型 20MiB 内放行。
	write("photo.jpg", 15<<20)
	captured, err = captureTaskDeliverableFiles(context.Background(), files, "personal:1", spoolDir,
		"[FILE:outputs/photo.jpg]")
	if err != nil {
		t.Fatalf("15MiB image capture failed: %v", err)
	}
	if captured[0].SizeBytes != 15<<20 {
		t.Fatalf("image size = %d", captured[0].SizeBytes)
	}
}

func TestCleanupSpoolCaptureDir(t *testing.T) {
	spoolDir := t.TempDir()
	captureRoot := filepath.Join(spoolDir, "capture", "session-1")
	if err := os.MkdirAll(captureRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	oldFile := filepath.Join(captureRoot, "old.bin")
	newFile := filepath.Join(captureRoot, "new.bin")
	if err := os.WriteFile(oldFile, []byte("x"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(newFile, []byte("x"), 0o640); err != nil {
		t.Fatal(err)
	}
	past := time.Now().Add(-31 * 24 * time.Hour)
	if err := os.Chtimes(oldFile, past, past); err != nil {
		t.Fatal(err)
	}
	if n := cleanupSpoolCaptureDir(spoolDir, time.Now().Add(-30*24*time.Hour)); n != 1 {
		t.Fatalf("cleaned %d files, want 1", n)
	}
	if _, err := os.Stat(oldFile); !os.IsNotExist(err) {
		t.Fatal("old spool file must be removed")
	}
	if _, err := os.Stat(newFile); err != nil {
		t.Fatal("new spool file must be kept")
	}
	// 无 capture 目录: 幂等返回 0。
	if n := cleanupSpoolCaptureDir(t.TempDir(), time.Now()); n != 0 {
		t.Fatalf("missing capture dir cleaned %d", n)
	}
}
