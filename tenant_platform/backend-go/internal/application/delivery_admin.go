package application

import (
	"context"
	"time"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/domain"
)

// DeliveryAdminStore 是 admin 死信管理端口(2026-08-14 独立审查 E2):
// 08-14 事故恢复靠手动 SQL 重投 26 条死信——管理员需要可审计的查询与
// 重投能力, 不依赖直接改库。
type DeliveryAdminStore interface {
	// ListDeliveries 按状态列出 delivery 行(管理视图, 最新在前)。
	ListDeliveries(ctx context.Context, status string, limit int) ([]domain.DeliveryAdminRow, error)
	// RequeueDeadLetterDelivery 把死信行重置为 pending(attempt_count 归零,
	// 清终端字段)供 delivery 循环重投。仅 dead_letter 行可重投; 返回 found。
	RequeueDeadLetterDelivery(ctx context.Context, deliveryID string, now time.Time) (bool, error)
}
