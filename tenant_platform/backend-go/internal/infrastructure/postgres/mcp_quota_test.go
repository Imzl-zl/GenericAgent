package postgres

import (
	"context"
	"reflect"
	"sync"
	"testing"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/domain"
)

// headers 读写: Create/Get/Update/List 全链路往返一致。
func TestMCPServerHeadersRoundTrip(t *testing.T) {
	pool := requireDB(t)
	ctx := context.Background()
	store, err := NewStore(pool)
	if err != nil {
		t.Fatalf("new store: %v", err)
	}

	created, err := store.CreateMCPServer(ctx, domain.MCPServerCreate{
		ServerKey:      "headers_roundtrip",
		Name:           "Headers Roundtrip",
		URL:            "https://example.com/mcp",
		TimeoutSeconds: 30,
		Headers: map[string]string{
			"Authorization": "Bearer secret-key-123",
			"X-Custom":      "custom-value",
		},
	})
	if err != nil {
		t.Fatalf("create with headers: %v", err)
	}
	if !reflect.DeepEqual(created.Headers, map[string]string{
		"Authorization": "Bearer secret-key-123",
		"X-Custom":      "custom-value",
	}) {
		t.Fatalf("created headers mismatch: %+v", created.Headers)
	}

	got, err := store.GetMCPServer(ctx, created.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if !reflect.DeepEqual(got.Headers, created.Headers) {
		t.Fatalf("get headers mismatch: %+v", got.Headers)
	}

	listed, err := store.ListMCPServers(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	var found *domain.MCPServer
	for i := range listed {
		if listed[i].ID == created.ID {
			found = &listed[i]
			break
		}
	}
	if found == nil {
		t.Fatalf("created server not in list")
	}
	if !reflect.DeepEqual(found.Headers, created.Headers) {
		t.Fatalf("list headers mismatch: %+v", found.Headers)
	}

	updated, err := store.UpdateMCPServer(ctx, created.ID, domain.MCPServerUpdate{
		MCPServerCreate: domain.MCPServerCreate{
			ServerKey:      created.ServerKey,
			Name:           created.Name,
			URL:            created.URL,
			TimeoutSeconds: 30,
			Headers:        map[string]string{"Authorization": "Bearer rotated-key-456"},
		},
	})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if !reflect.DeepEqual(updated.Headers, map[string]string{"Authorization": "Bearer rotated-key-456"}) {
		t.Fatalf("updated headers mismatch: %+v", updated.Headers)
	}
}

// headers 为空时: 返回 nil 而非空 map(避免 JSON 往返差异)。
func TestMCPServerHeadersEmptyIsNil(t *testing.T) {
	pool := requireDB(t)
	ctx := context.Background()
	store, err := NewStore(pool)
	if err != nil {
		t.Fatalf("new store: %v", err)
	}

	created, err := store.CreateMCPServer(ctx, domain.MCPServerCreate{
		ServerKey:      "headers_empty",
		Name:           "Headers Empty",
		URL:            "https://example.com/mcp",
		TimeoutSeconds: 30,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if created.Headers != nil {
		t.Fatalf("expected nil headers, got %+v", created.Headers)
	}
}

func TestConsumeMCPQuotaBasics(t *testing.T) {
	pool := requireDB(t)
	ctx := context.Background()
	store, err := NewStore(pool)
	if err != nil {
		t.Fatalf("new store: %v", err)
	}

	const (
		owner    = "user-quota-basics"
		serverID = "quota-server-1"
		limit    = 3
	)
	if err := store.SetMCPQuotaLimit(ctx, owner, serverID, "day", limit); err != nil {
		t.Fatalf("set limit: %v", err)
	}

	for i := int64(1); i <= limit; i++ {
		allowed, err := store.ConsumeMCPQuota(ctx, owner, serverID, "day")
		if err != nil {
			t.Fatalf("consume %d: %v", i, err)
		}
		if !allowed {
			t.Fatalf("consume %d should be allowed (limit %d)", i, limit)
		}
	}
	allowed, err := store.ConsumeMCPQuota(ctx, owner, serverID, "day")
	if err != nil {
		t.Fatalf("consume over limit: %v", err)
	}
	if allowed {
		t.Fatalf("consume over limit should be rejected")
	}
}

// 无配额行 = 默认放行(D6: 无配额行默认放行, 与现有 max_turns 行为一致)。
func TestConsumeMCPQuotaNoLimitRowAllows(t *testing.T) {
	pool := requireDB(t)
	ctx := context.Background()
	store, err := NewStore(pool)
	if err != nil {
		t.Fatalf("new store: %v", err)
	}

	allowed, err := store.ConsumeMCPQuota(ctx, "user-no-limit", "server-no-limit", "day")
	if err != nil {
		t.Fatalf("consume without limit row: %v", err)
	}
	if !allowed {
		t.Fatalf("consume without limit row should be allowed")
	}
}

// 周期隔离: day 与 month 各自计数; 不同 server 互不影响。
func TestConsumeMCPQuotaPeriodAndServerIsolation(t *testing.T) {
	pool := requireDB(t)
	ctx := context.Background()
	store, err := NewStore(pool)
	if err != nil {
		t.Fatalf("new store: %v", err)
	}

	const owner = "user-isolation"
	if err := store.SetMCPQuotaLimit(ctx, owner, "server-a", "day", 1); err != nil {
		t.Fatal(err)
	}
	if err := store.SetMCPQuotaLimit(ctx, owner, "server-a", "month", 5); err != nil {
		t.Fatal(err)
	}
	if err := store.SetMCPQuotaLimit(ctx, owner, "server-b", "day", 1); err != nil {
		t.Fatal(err)
	}

	// day 限额 1: 第二次拒绝。
	if ok, _ := store.ConsumeMCPQuota(ctx, owner, "server-a", "day"); !ok {
		t.Fatalf("server-a day consume 1 should pass")
	}
	if ok, _ := store.ConsumeMCPQuota(ctx, owner, "server-a", "day"); ok {
		t.Fatalf("server-a day consume 2 should be rejected")
	}
	// month 限额 5: 独立计数, 不受 day 影响。
	for i := 0; i < 5; i++ {
		if ok, _ := store.ConsumeMCPQuota(ctx, owner, "server-a", "month"); !ok {
			t.Fatalf("server-a month consume %d should pass", i+1)
		}
	}
	if ok, _ := store.ConsumeMCPQuota(ctx, owner, "server-a", "month"); ok {
		t.Fatalf("server-a month consume 6 should be rejected")
	}
	// server-b 独立: 自己的 day 限额 1。
	if ok, _ := store.ConsumeMCPQuota(ctx, owner, "server-b", "day"); !ok {
		t.Fatalf("server-b day consume 1 should pass")
	}
	if ok, _ := store.ConsumeMCPQuota(ctx, owner, "server-b", "day"); ok {
		t.Fatalf("server-b day consume 2 should be rejected")
	}
}

// 并发原子性: limit=5 时 20 个并发调用恰好 5 个成功(配额核心不变量)。
func TestConsumeMCPQuotaAtomicUnderConcurrency(t *testing.T) {
	pool := requireDB(t)
	ctx := context.Background()
	store, err := NewStore(pool)
	if err != nil {
		t.Fatalf("new store: %v", err)
	}

	const (
		owner    = "user-concurrent"
		serverID = "server-concurrent"
		limit    = 5
		workers  = 20
	)
	if err := store.SetMCPQuotaLimit(ctx, owner, serverID, "day", limit); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	allowed := make([]bool, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ok, err := store.ConsumeMCPQuota(ctx, owner, serverID, "day")
			if err != nil {
				t.Errorf("worker %d: %v", i, err)
				return
			}
			allowed[i] = ok
		}(i)
	}
	wg.Wait()

	success := 0
	for _, ok := range allowed {
		if ok {
			success++
		}
	}
	if success != limit {
		t.Fatalf("expected exactly %d allowed, got %d", limit, success)
	}
}

// MCPQuotaAvailable: 无限额行=可用; 有限额行且未耗尽=可用; 耗尽=不可用。
func TestMCPQuotaAvailable(t *testing.T) {
	pool := requireDB(t)
	ctx := context.Background()
	store, err := NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}

	// 无限额行 → 可用(默认放行)。
	ok, err := store.MCPQuotaAvailable(ctx, "user-avail", "server-avail")
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatalf("no limit row should be available")
	}

	// day 限额 1, 未消耗 → 可用。
	if err := store.SetMCPQuotaLimit(ctx, "user-avail", "server-avail", "day", 1); err != nil {
		t.Fatal(err)
	}
	ok, err = store.MCPQuotaAvailable(ctx, "user-avail", "server-avail")
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatalf("unconsumed quota should be available")
	}

	// day 消耗完 → 不可用(即使 month 无限额)。
	if ok, _ := store.ConsumeMCPQuota(ctx, "user-avail", "server-avail", "day"); !ok {
		t.Fatalf("consume should pass")
	}
	ok, err = store.MCPQuotaAvailable(ctx, "user-avail", "server-avail")
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatalf("exhausted day quota should be unavailable")
	}

	// 另一 server 独立。
	ok, err = store.MCPQuotaAvailable(ctx, "user-avail", "server-other")
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatalf("other server should be available")
	}
}

// 组合扣减: day+month 都扣, 任一耗尽 → false。
func TestConsumeMCPQuotasDayAndMonth(t *testing.T) {
	pool := requireDB(t)
	ctx := context.Background()
	store, err := NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	const owner, server = "user-combo", "server-combo"
	if err := store.SetMCPQuotaLimit(ctx, owner, server, "day", 1); err != nil {
		t.Fatal(err)
	}
	if err := store.SetMCPQuotaLimit(ctx, owner, server, "month", 10); err != nil {
		t.Fatal(err)
	}
	// 第 1 次: day 与 month 都成功。
	if ok, err := store.ConsumeMCPQuotas(ctx, owner, server); err != nil || !ok {
		t.Fatalf("first consume: ok=%v err=%v", ok, err)
	}
	// 第 2 次: day 耗尽 → false。
	if ok, err := store.ConsumeMCPQuotas(ctx, owner, server); err != nil || ok {
		t.Fatalf("second consume: ok=%v err=%v (want false)", ok, err)
	}
	// 无任何限额行 → 放行且不写 usage。
	if ok, err := store.ConsumeMCPQuotas(ctx, "user-nolimit", "server-nolimit"); err != nil || !ok {
		t.Fatalf("no-limit consume: ok=%v err=%v", ok, err)
	}
}

// TestConsumeMCPQuotasRejectedDoesNotBurnDay(Y2 回归): month 耗尽导致整体
// 拒绝时, day 不得被计数——被拒绝的调用不消耗任何周期配额。
// 旧实现先扣 day 再查 month, 拒绝后 day 已 +1, 会逐步把 day 烧尽。
func TestConsumeMCPQuotasRejectedDoesNotBurnDay(t *testing.T) {
	pool := requireDB(t)
	ctx := context.Background()
	store, err := NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	const owner, server = "user-noburn", "server-noburn"
	if err := store.SetMCPQuotaLimit(ctx, owner, server, "day", 5); err != nil {
		t.Fatal(err)
	}
	if err := store.SetMCPQuotaLimit(ctx, owner, server, "month", 1); err != nil {
		t.Fatal(err)
	}
	// 第 1 次成功(day=1, month=1)。
	if ok, err := store.ConsumeMCPQuotas(ctx, owner, server); err != nil || !ok {
		t.Fatalf("first consume: ok=%v err=%v", ok, err)
	}
	// 第 2 次: month 已耗尽 → 拒绝, 且不得烧 day。
	if ok, err := store.ConsumeMCPQuotas(ctx, owner, server); err != nil || ok {
		t.Fatalf("second consume: ok=%v err=%v (want false)", ok, err)
	}
	// 管理员把月配额提到 100(不再成为瓶颈)。day 限额 5 只被成功调用消耗:
	// 第 1 次已用 1, 故之后恰好 4 次成功、第 5 次起被拒。若被拒调用烧了
	// day(旧实现), 则只能再成功 3 次——本断言可区分。
	if err := store.SetMCPQuotaLimit(ctx, owner, server, "month", 100); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 4; i++ {
		if ok, err := store.ConsumeMCPQuotas(ctx, owner, server); err != nil || !ok {
			t.Fatalf("consume %d after month raise: ok=%v err=%v (want true)", i+3, ok, err)
		}
	}
	if ok, err := store.ConsumeMCPQuotas(ctx, owner, server); err != nil || ok {
		t.Fatalf("final consume: ok=%v err=%v (want false: day exhausted by 5 successful calls)", ok, err)
	}
}

// TestConsumeMCPQuotasAtomicUnderConcurrency(Y2 并发回归): day+month 双周期
// 事务扣减在并发下仍精确生效——limit=5 恰 5 成功, 其余整体拒绝。
func TestConsumeMCPQuotasAtomicUnderConcurrency(t *testing.T) {
	pool := requireDB(t)
	ctx := context.Background()
	store, err := NewStore(pool)
	if err != nil {
		t.Fatalf("new store: %v", err)
	}

	const (
		owner    = "user-combo-concurrent"
		serverID = "server-combo-concurrent"
		limit    = 5
		workers  = 20
	)
	if err := store.SetMCPQuotaLimit(ctx, owner, serverID, "day", limit); err != nil {
		t.Fatal(err)
	}
	if err := store.SetMCPQuotaLimit(ctx, owner, serverID, "month", 1000); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	allowed := make([]bool, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ok, err := store.ConsumeMCPQuotas(ctx, owner, serverID)
			if err != nil {
				t.Errorf("worker %d: %v", i, err)
				return
			}
			allowed[i] = ok
		}(i)
	}
	wg.Wait()

	success := 0
	for _, ok := range allowed {
		if ok {
			success++
		}
	}
	if success != limit {
		t.Fatalf("expected exactly %d allowed, got %d", limit, success)
	}
}

// 配额 CRUD: 设置/列出/删除。
func TestMCPQuotaLimitCRUD(t *testing.T) {
	pool := requireDB(t)
	ctx := context.Background()
	store, err := NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	const owner = "user-crud"
	if err := store.SetMCPQuotaLimit(ctx, owner, "tavily", "day", 50); err != nil {
		t.Fatal(err)
	}
	if err := store.SetMCPQuotaLimit(ctx, owner, "exa", "month", 200); err != nil {
		t.Fatal(err)
	}
	// 覆盖更新。
	if err := store.SetMCPQuotaLimit(ctx, owner, "tavily", "day", 80); err != nil {
		t.Fatal(err)
	}
	limits, err := store.ListMCPQuotaLimits(ctx, owner)
	if err != nil {
		t.Fatal(err)
	}
	if len(limits) != 2 {
		t.Fatalf("expected 2 limits, got %d: %+v", len(limits), limits)
	}
	byServer := map[string]map[string]int64{}
	for _, l := range limits {
		if byServer[l.ServerID] == nil {
			byServer[l.ServerID] = map[string]int64{}
		}
		byServer[l.ServerID][string(l.Period)] = l.LimitCount
	}
	if byServer["tavily"]["day"] != 80 || byServer["exa"]["month"] != 200 {
		t.Fatalf("limits mismatch: %+v", byServer)
	}
	if err := store.DeleteMCPQuotaLimit(ctx, owner, "tavily", "day"); err != nil {
		t.Fatal(err)
	}
	limits, err = store.ListMCPQuotaLimits(ctx, owner)
	if err != nil {
		t.Fatal(err)
	}
	if len(limits) != 1 {
		t.Fatalf("expected 1 limit after delete, got %d", len(limits))
	}
}

// TestConsumeMCPQuotasHighContention(二轮审查压力): 100 并发 × 3 轮,
// 验证事务锁序下无死锁、无随机失败, 每轮恰 limit 成功(配额精确)。
func TestConsumeMCPQuotasHighContention(t *testing.T) {
	pool := requireDB(t)
	ctx := context.Background()
	store, err := NewStore(pool)
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	const (
		owner    = "user-contention"
		serverID = "server-contention"
		limit    = 5
		workers  = 100
		rounds   = 3
	)
	if err := store.SetMCPQuotaLimit(ctx, owner, serverID, "day", limit); err != nil {
		t.Fatal(err)
	}
	if err := store.SetMCPQuotaLimit(ctx, owner, serverID, "month", 10000); err != nil {
		t.Fatal(err)
	}
	for round := 0; round < rounds; round++ {
		// 每轮重置用量(直插清理: 事务锁序验证的是并发行为, 与表清理无关)。
		if _, err := pool.Exec(ctx, `DELETE FROM mcp_quota_usage WHERE owner_key = $1 AND server_id = $2`, owner, serverID); err != nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		allowed := make([]bool, workers)
		errs := make([]error, workers)
		for i := 0; i < workers; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				ok, err := store.ConsumeMCPQuotas(ctx, owner, serverID)
				allowed[i] = ok
				errs[i] = err
			}(i)
		}
		wg.Wait()
		success := 0
		for i, ok := range allowed {
			if errs[i] != nil {
				t.Fatalf("round %d worker %d: unexpected error %v (deadlock/contention must not surface as error)", round, i, errs[i])
			}
			if ok {
				success++
			}
		}
		if success != limit {
			t.Fatalf("round %d: expected exactly %d allowed, got %d", round, limit, success)
		}
	}
}
