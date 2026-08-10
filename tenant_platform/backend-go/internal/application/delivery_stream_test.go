package application

import (
	"testing"
	"time"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/domain"
)

// TestDeliveryServiceSkipsTextWhenStreamFinal 流式最终交付(IM_STREAMING_
// DELIVERY §4.2): task.StreamFinalAt 非空 → 文本 part 跳过(不调
// SendMessage), 文件 part 照发; ack 语义不变。
func TestDeliveryServiceSkipsTextWhenStreamFinal(t *testing.T) {
	ctx, svc, deps := setupDeliveryService(t)
	now := time.Now().UTC()
	deps.tasks.task = domain.Task{ID: "t1", SessionKey: "personal:1", RequesterID: 1, TerminalAt: &now, StreamFinalAt: &now}
	deps.bots.bot = boundBot(1)
	deps.results.payload = domain.ResultPayload{Ref: "ref:1", Digest: "sha256:a", Body: []byte("task complete text")}

	delivery := domain.Delivery{DeliveryID: "t1:task_complete", TaskID: "t1", DeliveryType: domain.DeliveryTaskComplete, PayloadRef: "ref:1", PayloadDigest: "sha256:a"}
	deps.store.pending = []domain.Delivery{delivery}

	if err := svc.tick(ctx); err != nil {
		t.Fatalf("tick: %v", err)
	}

	// 文本已流式交付 → 不发送文本。
	if len(deps.transport.sent) != 0 {
		t.Fatalf("stream-final delivery must skip text, got %d sent: %+v", len(deps.transport.sent), deps.transport.sent)
	}
	// 仍 ack(文本 part 不是失败——是主动跳过)。
	if len(deps.store.acked) != 1 {
		t.Fatalf("expected ack, got acked=%v", deps.store.acked)
	}
}

// TestDeliveryServiceSendsFilesEvenWhenStreamFinal 流式最终交付 + 文件:
// 文本跳过但文件照发。
func TestDeliveryServiceSendsFilesEvenWhenStreamFinal(t *testing.T) {
	ctx, svc, deps := setupDeliveryService(t)
	now := time.Now().UTC()
	deps.tasks.task = domain.Task{ID: "t1", SessionKey: "personal:1", RequesterID: 1, TerminalAt: &now, StreamFinalAt: &now}
	deps.bots.bot = boundBot(1)
	deps.results.payload = domain.ResultPayload{Ref: "ref:1", Digest: "sha256:a", Body: []byte("见文件 [FILE:out.txt]")}
	deps.store.files = []domain.DeliveryFile{{
		Marker: "out.txt", FileName: "out.txt", RelPath: "out.txt",
		Content: []byte("x"), Digest: "sha256:aaaa", SizeBytes: 1,
	}}

	delivery := domain.Delivery{DeliveryID: "t1:task_complete", TaskID: "t1", DeliveryType: domain.DeliveryTaskComplete, PayloadRef: "ref:1", PayloadDigest: "sha256:a"}
	deps.store.pending = []domain.Delivery{delivery}

	if err := svc.tick(ctx); err != nil {
		t.Fatalf("tick: %v", err)
	}

	if len(deps.transport.sent) != 0 {
		t.Fatalf("text must be skipped, got %d text sends", len(deps.transport.sent))
	}
	if len(deps.transport.sentFiles) != 1 || deps.transport.sentFiles[0].FileName != "out.txt" {
		t.Fatalf("file must still be sent, got %+v", deps.transport.sentFiles)
	}
	if len(deps.store.acked) != 1 {
		t.Fatalf("expected ack, got acked=%v", deps.store.acked)
	}
}

// TestDeliveryServiceSendsTextWhenNoStreamFinal 失败/未流式兜底: 无
// stream_final_at 标记 → 文本照发(设计: 流式中途失败补发最终结果)。
func TestDeliveryServiceSendsTextWhenNoStreamFinal(t *testing.T) {
	ctx, svc, deps := setupDeliveryService(t)
	now := time.Now().UTC()
	deps.tasks.task = domain.Task{ID: "t1", SessionKey: "personal:1", RequesterID: 1, TerminalAt: &now}
	deps.bots.bot = boundBot(1)
	deps.results.payload = domain.ResultPayload{Ref: "ref:1", Digest: "sha256:a", Body: []byte("result body")}

	delivery := domain.Delivery{DeliveryID: "t1:task_complete", TaskID: "t1", DeliveryType: domain.DeliveryTaskComplete, PayloadRef: "ref:1", PayloadDigest: "sha256:a"}
	deps.store.pending = []domain.Delivery{delivery}

	if err := svc.tick(ctx); err != nil {
		t.Fatalf("tick: %v", err)
	}

	if len(deps.transport.sent) != 1 || deps.transport.sent[0].Text != "任务完成：\nresult body" {
		t.Fatalf("expected full text delivery, got %+v", deps.transport.sent)
	}
}

var _ = domain.ChannelFeishu // 保持 domain 导入引用(测试断言用)
