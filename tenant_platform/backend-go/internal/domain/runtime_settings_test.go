package domain

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestIMInboundCoalesceWindowDefaultSingleSourceOfTruth 守护窗口默认值的
// 单一真值源(审查 M2): domain.DefaultIMInboundCoalesceWindowMS 是"默认"的
// 唯一定义——migration 0025 种子必须与其一致, 否则新库/重建库的默认行为
// 与 Go 常量漂移(此前该常量为死代码, 默认值散落在 SQL 种子与 web 表单,
// 改动任何一处都不会报错)。
func TestIMInboundCoalesceWindowDefaultSingleSourceOfTruth(t *testing.T) {
	// Go 测试以包目录为 cwd: internal/domain → ../../../infra/postgres/migrations
	path := filepath.Clean(filepath.Join("..", "..", "..", "infra", "postgres", "migrations", "0025_platform_runtime_settings.sql"))
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read migration 0025: %v", err)
	}
	// 种子行: VALUES ('im_inbound_coalesce_window_ms', 4000, 0)
	needle := "'im_inbound_coalesce_window_ms', "
	idx := strings.Index(string(raw), needle)
	if idx < 0 {
		t.Fatalf("migration 0025 seed for im_inbound_coalesce_window_ms not found")
	}
	rest := string(raw)[idx+len(needle):]
	end := strings.IndexByte(rest, ',')
	if end < 0 {
		t.Fatalf("malformed seed row")
	}
	seed, err := strconv.Atoi(strings.TrimSpace(rest[:end]))
	if err != nil {
		t.Fatalf("parse seed value: %v", err)
	}
	if seed != DefaultIMInboundCoalesceWindowMS {
		t.Fatalf("migration seed = %d, domain default = %d — must stay in sync", seed, DefaultIMInboundCoalesceWindowMS)
	}
	if DefaultIMInboundCoalesceWindowMS > MaxIMInboundCoalesceWindowMS {
		t.Fatalf("default %d exceeds max %d", DefaultIMInboundCoalesceWindowMS, MaxIMInboundCoalesceWindowMS)
	}
}
