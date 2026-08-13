package application

import (
	"testing"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/domain"
)

// toTaskMedia/workerTaskEnvelope 的 content_type 透传(2026-08-14 审查 I1):
// webhook media_items 的渠道侧 MIME 必须在整个任务媒体链路中保留——此前
// 在 toTaskMedia 与 workerTaskEnvelope 两处丢失, 契约字段空转, Phase C
// 视频抽帧分派失效。

func TestToTaskMediaCarriesContentType(t *testing.T) {
	refs := []SessionFileRef{
		{
			Alias:        "F001",
			OriginalName: "img_v2_key",
			RelativePath: "attachments/F001_img_v2_key.png",
			SizeBytes:    1234,
			Direction:    "inbound",
			ContentType:  "image/png", // 飞书 image 无扩展名, 魔数嗅探落盘后由 webhook media_items 提供
		},
		{
			Alias:        "F002",
			OriginalName: "out.docx",
			RelativePath: "outputs/out.docx",
			SizeBytes:    99,
			Direction:    "outbound",
			ContentType:  "application/vnd.openxmlformats-officedocument.wordprocessingml.document",
		},
		{
			Alias:        "F003",
			OriginalName: "no_type.jpg",
			RelativePath: "attachments/F003_no_type.jpg",
			SizeBytes:    1,
			Direction:    "inbound",
			ContentType:  "",
		},
	}
	media := toTaskMedia(refs)
	if len(media) != 2 {
		t.Fatalf("toTaskMedia: want 2 inbound items, got %d", len(media))
	}
	if media[0].ContentType != "image/png" {
		t.Fatalf("toTaskMedia dropped content_type: got %q", media[0].ContentType)
	}
	if media[1].ContentType != "" {
		t.Fatalf("empty content_type must stay empty, got %q", media[1].ContentType)
	}
}

func TestWorkerTaskEnvelopeCarriesContentType(t *testing.T) {
	task := domain.Task{Media: []domain.TaskMedia{
		{
			Alias:        "F001",
			OriginalName: "img_v2_key",
			RelativePath: "attachments/F001_img_v2_key.png",
			SizeBytes:    1234,
			ContentType:  "image/png",
		},
	}}
	env := workerTaskEnvelope(task, 0, "")
	if len(env.Media) != 1 {
		t.Fatalf("envelope media: want 1, got %d", len(env.Media))
	}
	if env.Media[0].ContentType != "image/png" {
		t.Fatalf("envelope dropped content_type: got %q", env.Media[0].ContentType)
	}
}
