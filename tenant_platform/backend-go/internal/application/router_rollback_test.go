package application

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/domain"
	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/infrastructure/transport"
)

// ---------------------------------------------------------------------------
// round11 审查 I1: 附件写入先于任务授权/幂等事务——提交失败(成员被移除/
// 重复消息/DB 错误)时必须回滚本次导入, 防止团队 workspace 残留未授权附件。
// ---------------------------------------------------------------------------

// TestRemoveInboundRemovesFilesAndManifestEntries verifies RemoveInbound
// deletes the imported files and prunes the manifest entries.
func TestRemoveInboundRemovesFilesAndManifestEntries(t *testing.T) {
	files, err := NewSessionFiles(t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(t.TempDir(), "doc.txt")
	if err := os.WriteFile(src, []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	refs, err := files.ImportInbound("personal:1", []string{src})
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 1 {
		t.Fatalf("imported %d refs, want 1", len(refs))
	}
	abs := filepath.Join(mustSandboxRoot(t, files, "personal:1"), filepath.FromSlash(refs[0].RelativePath))
	if _, err := os.Stat(abs); err != nil {
		t.Fatalf("attachment must exist before rollback: %v", err)
	}
	if err := files.RemoveInbound("personal:1", refs); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(abs); !os.IsNotExist(err) {
		t.Fatalf("attachment must be removed after rollback, stat err=%v", err)
	}
	recent, err := files.Recent("personal:1", 8)
	if err != nil {
		t.Fatal(err)
	}
	if len(recent) != 0 {
		t.Fatalf("manifest must be pruned after rollback, got %d refs", len(recent))
	}
}

// TestRemoveInboundIsIdempotent verifies rolling back already-rolled-back
// refs (e.g. retried message) is a no-op success.
func TestRemoveInboundIsIdempotent(t *testing.T) {
	files, err := NewSessionFiles(t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(t.TempDir(), "doc.txt")
	if err := os.WriteFile(src, []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	refs, err := files.ImportInbound("personal:1", []string{src})
	if err != nil {
		t.Fatal(err)
	}
	if err := files.RemoveInbound("personal:1", refs); err != nil {
		t.Fatal(err)
	}
	if err := files.RemoveInbound("personal:1", refs); err != nil {
		t.Fatalf("second rollback must be idempotent: %v", err)
	}
}

// TestRemoveInboundKeepsOtherMessages verifies rolling back one message's
// refs does not touch attachments imported by other messages.
func TestRemoveInboundKeepsOtherMessages(t *testing.T) {
	files, err := NewSessionFiles(t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	srcA := filepath.Join(dir, "a.txt")
	srcB := filepath.Join(dir, "b.txt")
	if err := os.WriteFile(srcA, []byte("a"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(srcB, []byte("b"), 0o644); err != nil {
		t.Fatal(err)
	}
	refsA, err := files.ImportInbound("personal:1", []string{srcA})
	if err != nil {
		t.Fatal(err)
	}
	refsB, err := files.ImportInbound("personal:1", []string{srcB})
	if err != nil {
		t.Fatal(err)
	}
	if err := files.RemoveInbound("personal:1", refsA); err != nil {
		t.Fatal(err)
	}
	recent, err := files.Recent("personal:1", 8)
	if err != nil {
		t.Fatal(err)
	}
	if len(recent) != 1 || recent[0].RelativePath != refsB[0].RelativePath {
		t.Fatalf("other message attachments must survive, got %+v", recent)
	}
}

// TestRouterRollsBackAttachmentsOnSubmitFailure verifies the router removes
// imported attachments when task submission (with authz/idempotency) fails.
func TestRouterRollsBackAttachmentsOnSubmitFailure(t *testing.T) {
	store := newFakeRouterStore()
	store.bots["b1"] = domain.Bot{ID: 1, BotUUID: "b1", OwnerID: 42, IlinkUserID: "u1", State: domain.BotActive}
	store.statuses[42] = domain.UserApproved
	src := filepath.Join(t.TempDir(), "resume.txt")
	if err := os.WriteFile(src, []byte("resume"), 0o644); err != nil {
		t.Fatal(err)
	}
	tr := transport.NewLoopbackTransport()
	r, tasks, sessionFiles := newTestRouterWithSessionFiles(t, store, tr)
	tasks.submitErr = errors.New("member removed from team")

	_, err := r.HandleMessage(context.Background(), IncomingMessage{
		BotUUID: "b1", IlinkUserID: "u1", MessageID: "m1", Text: "整理简历",
		MediaPaths: []string{src},
	})
	if err == nil {
		t.Fatal("expected submit failure")
	}
	refs, err := sessionFiles.Recent("personal:42", 8)
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 0 {
		t.Fatalf("attachments must be rolled back after failed submission, got %d refs", len(refs))
	}
}

// TestRouterKeepsAttachmentsOnSuccessfulSubmit verifies the happy path keeps
// the imported attachments (no rollback on success).
func TestRouterKeepsAttachmentsOnSuccessfulSubmit(t *testing.T) {
	store := newFakeRouterStore()
	store.bots["b1"] = domain.Bot{ID: 1, BotUUID: "b1", OwnerID: 42, IlinkUserID: "u1", State: domain.BotActive}
	store.statuses[42] = domain.UserApproved
	src := filepath.Join(t.TempDir(), "resume.txt")
	if err := os.WriteFile(src, []byte("resume"), 0o644); err != nil {
		t.Fatal(err)
	}
	tr := transport.NewLoopbackTransport()
	r, _, sessionFiles := newTestRouterWithSessionFiles(t, store, tr)

	res, err := r.HandleMessage(context.Background(), IncomingMessage{
		BotUUID: "b1", IlinkUserID: "u1", MessageID: "m1", Text: "整理简历",
		MediaPaths: []string{src},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Action != ActionTaskCreated {
		t.Fatalf("expected task_created, got %s", res.Action)
	}
	refs, err := sessionFiles.Recent("personal:42", 8)
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 1 {
		t.Fatalf("attachments must be kept on success, got %d refs", len(refs))
	}
}
