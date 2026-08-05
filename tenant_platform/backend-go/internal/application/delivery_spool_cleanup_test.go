package application

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/domain"
)

// round12 审查(I6): 交付 spool 快照的清理所有权必须覆盖整个 process——
// 文本发送失败或前序文件发送失败时, buildPayload 已物化的全部文件都必须
// 删除, 不得按"逐文件 defer"只清理已进入循环的文件。

func deliveryFileFixture() []domain.DeliveryFile {
	return []domain.DeliveryFile{
		{
			Marker: "outputs/a.docx", FileName: "a.docx", RelPath: "outputs/a.docx",
			Content: []byte("aaa"), Digest: "sha256:aaa", SizeBytes: 3,
		},
		{
			Marker: "outputs/b.pdf", FileName: "b.pdf", RelPath: "outputs/b.pdf",
			Content: []byte("bbb"), Digest: "sha256:bbb", SizeBytes: 3,
		},
	}
}

func spoolFileCount(t *testing.T, svc *deliveryService) int {
	t.Helper()
	count := 0
	err := filepath.WalkDir(svc.snapshotDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			count++
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk spool dir: %v", err)
	}
	return count
}

func TestDeliveryRemovesAllSpoolFilesWhenTextSendFails(t *testing.T) {
	ctx, svc, deps := setupDeliveryService(t)
	deps.tasks.task = domain.Task{ID: "t1", SessionKey: "personal:1", RequesterID: 1, TerminalAt: ptr(time.Now().UTC())}
	deps.bots.bot = boundBot(1)
	deps.store.files = deliveryFileFixture()
	deps.results.payload = domain.ResultPayload{Ref: "ref:1", Digest: "sha256:a", Body: []byte("完成。\n[FILE:outputs/a.docx]\n[FILE:outputs/b.pdf]")}
	deps.transport.err = errors.New("text send failed")
	deps.store.pending = []domain.Delivery{{
		DeliveryID: "t1:task_complete", TaskID: "t1", DeliveryType: domain.DeliveryTaskComplete,
		PayloadRef: "ref:1", PayloadDigest: "sha256:a", AttemptCount: 1,
	}}

	if err := svc.tick(ctx); err != nil {
		t.Fatalf("tick: %v", err)
	}
	if len(deps.transport.sentFiles) != 0 {
		t.Fatalf("no file send expected, got %+v", deps.transport.sentFiles)
	}
	if got := spoolFileCount(t, svc); got != 0 {
		t.Fatalf("spool has %d files after text send failure, want 0", got)
	}
}

func TestDeliveryRemovesAllSpoolFilesWhenFirstFileSendFails(t *testing.T) {
	ctx, svc, deps := setupDeliveryService(t)
	deps.tasks.task = domain.Task{ID: "t1", SessionKey: "personal:1", RequesterID: 1, TerminalAt: ptr(time.Now().UTC())}
	deps.bots.bot = boundBot(1)
	deps.store.files = deliveryFileFixture()
	deps.results.payload = domain.ResultPayload{Ref: "ref:1", Digest: "sha256:a", Body: []byte("完成。\n[FILE:outputs/a.docx]\n[FILE:outputs/b.pdf]")}
	deps.transport.fileErr = errors.New("file send failed")
	deps.store.pending = []domain.Delivery{{
		DeliveryID: "t1:task_complete", TaskID: "t1", DeliveryType: domain.DeliveryTaskComplete,
		PayloadRef: "ref:1", PayloadDigest: "sha256:a", AttemptCount: 1,
	}}

	if err := svc.tick(ctx); err != nil {
		t.Fatalf("tick: %v", err)
	}
	if got := spoolFileCount(t, svc); got != 0 {
		t.Fatalf("spool has %d files after first file send failure, want 0", got)
	}
}

// TestDeliveryRemovesAllSpoolFilesAfterSuccess: 成功路径同样清理(对照),
// 且发送的文件确实来自 spool 快照。
func TestDeliveryRemovesAllSpoolFilesAfterSuccess(t *testing.T) {
	ctx, svc, deps := setupDeliveryService(t)
	deps.tasks.task = domain.Task{ID: "t1", SessionKey: "personal:1", RequesterID: 1, TerminalAt: ptr(time.Now().UTC())}
	deps.bots.bot = boundBot(1)
	deps.store.files = deliveryFileFixture()
	deps.results.payload = domain.ResultPayload{Ref: "ref:1", Digest: "sha256:a", Body: []byte("完成。\n[FILE:outputs/a.docx]\n[FILE:outputs/b.pdf]")}
	deps.store.pending = []domain.Delivery{{
		DeliveryID: "t1:task_complete", TaskID: "t1", DeliveryType: domain.DeliveryTaskComplete,
		PayloadRef: "ref:1", PayloadDigest: "sha256:a",
	}}

	if err := svc.tick(ctx); err != nil {
		t.Fatalf("tick: %v", err)
	}
	if len(deps.transport.sentFiles) != 2 {
		t.Fatalf("file sends = %d, want 2", len(deps.transport.sentFiles))
	}
	if got := spoolFileCount(t, svc); got != 0 {
		t.Fatalf("spool has %d files after success, want 0", got)
	}
	if len(deps.store.acked) != 1 {
		t.Fatalf("delivery not acked: %+v", deps.store.acked)
	}
}

// TestBuildPayloadCleansPartialFilesOnError: buildPayload 自身中途失败时
// 不得残留已写入的前序快照。用目录占据第二个文件的物化路径, 使第二个
// WriteFile 必然失败(第一个文件已写入)。
func TestBuildPayloadCleansPartialFilesOnError(t *testing.T) {
	ctx, svc, deps := setupDeliveryService(t)
	deps.tasks.task = domain.Task{ID: "t1", SessionKey: "personal:1", RequesterID: 1, TerminalAt: ptr(time.Now().UTC())}
	deps.bots.bot = boundBot(1)
	files := deliveryFileFixture()
	deps.store.files = files
	deps.results.payload = domain.ResultPayload{Ref: "ref:1", Digest: "sha256:a", Body: []byte("完成。")}

	// 第二个文件的物化路径预先被目录占据 → WriteFile 失败。
	f2 := files[1]
	dir := filepath.Join(svc.snapshotDir, deliveryFileKey("t1:task_complete"))
	tmp2 := filepath.Join(dir, deliveryFileMarkerKey(f2.Marker)+"_"+deliverableSnapshotBase(f2.RelPath))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(tmp2, 0o700); err != nil {
		t.Fatal(err)
	}

	payload, err := svc.buildPayload(ctx, domain.Delivery{
		DeliveryID: "t1:task_complete", TaskID: "t1", DeliveryType: domain.DeliveryTaskComplete,
		PayloadRef: "ref:1", PayloadDigest: "sha256:a",
	}, deps.tasks.task)
	if err == nil {
		t.Fatal("expected buildPayload failure")
	}
	if len(payload.Files) != 0 {
		t.Fatalf("expected empty payload on failure, got %d files", len(payload.Files))
	}
	if got := spoolFileCount(t, svc); got != 0 {
		t.Fatalf("spool has %d files after buildPayload failure, want 0", got)
	}
}
