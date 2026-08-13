package application

import (
	"testing"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/domain"
)


func TestInferMessageTypeWithContentTypes(t *testing.T) {
	paths := []string{"/media/bot/a", "/media/bot/b"}
	cases := []struct {
		name  string
		items []IncomingMediaItem
		want  string
	}{
		{"image", []IncomingMediaItem{{ContentType: "image/jpeg"}, {ContentType: "video/mp4"}}, domain.MessageTypeImage},
		{"video", []IncomingMediaItem{{ContentType: "application/octet-stream"}, {ContentType: "VIDEO/MP4"}}, domain.MessageTypeVideo},
		{"file", []IncomingMediaItem{{ContentType: "application/pdf"}}, domain.MessageTypeFile},
		{"no items fallback", nil, domain.MessageTypeFile},
	}
	for _, c := range cases {
		if got := inferMessageType(paths, c.items); got != c.want {
			t.Errorf("%s: inferMessageType = %q, want %q", c.name, got, c.want)
		}
	}
	if got := inferMessageType(nil, nil); got != domain.MessageTypeText {
		t.Errorf("empty paths = %q, want text", got)
	}
}

func TestApplyInboundContentTypes(t *testing.T) {
	refs := []SessionFileRef{
		{Alias: "F001", RelativePath: "attachments/F001_a.jpg"},
		{Alias: "F002", RelativePath: "attachments/F002_b"},
	}
	items := []IncomingMediaItem{
		{ContentType: "image/jpeg"},
		{ContentType: ""},
	}
	applyInboundContentTypes(refs, items)
	if refs[0].ContentType != "image/jpeg" {
		t.Errorf("refs[0].ContentType = %q", refs[0].ContentType)
	}
	if refs[1].ContentType != "" {
		t.Errorf("refs[1].ContentType = %q, want empty", refs[1].ContentType)
	}
	// 越界安全: items 短于 refs 时不 panic。
	refs2 := []SessionFileRef{{Alias: "F001"}, {Alias: "F002"}}
	applyInboundContentTypes(refs2, items[:1])
	if refs2[1].ContentType != "" {
		t.Errorf("refs2[1].ContentType = %q, want empty", refs2[1].ContentType)
	}
}
