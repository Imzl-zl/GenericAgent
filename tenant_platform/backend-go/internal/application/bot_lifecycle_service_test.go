package application

import (
	"encoding/json"
	"testing"
)

// TestNormalizeChannelConfigJSON: restore 兜底——非 JSON 凭据明文按
// {"token": <明文>} 包装(08-10 契约), 合法 JSON 原样透传(2026-08-14 复盘:
// 微信 QR 写入路径曾直接加密裸 BotToken, restore 时 poller marshal 失败)。
func TestNormalizeChannelConfigJSON(t *testing.T) {
	// 合法 JSON 原样返回
	valid := []byte(`{"token":"abc"}`)
	if out := normalizeChannelConfigJSON(valid); string(out) != string(valid) {
		t.Fatalf("valid JSON must pass through, got %q", out)
	}
	// 非 JSON 裸凭据(iLink 格式) → {"token": ...} 包装
	legacy := []byte("ea09125f26ec@im.bot:0600004b56ff3cec0e30d370a1d2df7534bd78")
	out := normalizeChannelConfigJSON(legacy)
	if !json.Valid(out) {
		t.Fatalf("wrapped output is not valid JSON: %q", out)
	}
	var parsed struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(out, &parsed); err != nil {
		t.Fatalf("unmarshal wrapped: %v", err)
	}
	if parsed.Token != string(legacy) {
		t.Fatalf("token = %q", parsed.Token)
	}
	// 空明文原样返回(不包装)
	if out := normalizeChannelConfigJSON(nil); out != nil {
		t.Fatalf("nil must pass through, got %q", out)
	}
	if out := normalizeChannelConfigJSON([]byte("  ")); string(out) != "  " {
		t.Fatalf("whitespace must pass through, got %q", out)
	}
}
