package postgres

import (
	"context"
	"testing"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/domain"
)

// TestGetIMInboundCoalesceWindowMSMissingRowFallsBackToDefault 守护审查 M2:
// DB 无行(从未配置/种子被删)时 GET 必须回退域默认值而非报错——ReconcileBots
// 对账与 admin GET 在缺行时保持可用, 默认值单真值源在 domain 常量。
func TestGetIMInboundCoalesceWindowMSMissingRowFallsBackToDefault(t *testing.T) {
	pool := OpenTestPool(t)
	ctx := context.Background()
	store := &Store{pool: pool}

	// 先确保行存在(迁移种子), 删除模拟"缺行"状态。
	if _, err := pool.Exec(ctx, `DELETE FROM platform_runtime_settings WHERE setting_key = $1`,
		domain.IMInboundCoalesceWindowSettingKey); err != nil {
		t.Fatalf("delete settings row: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `
INSERT INTO platform_runtime_settings (setting_key, int_value, updated_by)
VALUES ($1, $2, 0) ON CONFLICT (setting_key) DO NOTHING`,
			domain.IMInboundCoalesceWindowSettingKey, domain.DefaultIMInboundCoalesceWindowMS)
	})

	got, err := store.GetIMInboundCoalesceWindowMS(ctx)
	if err != nil {
		t.Fatalf("missing row must fall back, got error: %v", err)
	}
	if got != domain.DefaultIMInboundCoalesceWindowMS {
		t.Fatalf("fallback = %d, want domain default %d", got, domain.DefaultIMInboundCoalesceWindowMS)
	}
}
