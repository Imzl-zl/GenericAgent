package mcpgateway

import (
	"context"
	"errors"
	"time"
)

// 默认与上限常量(退避/熔断见 stdioPool)。
const (
	// DefaultIdleTTL 是 stdio 进程空闲回收 TTL(compose 默认 5m)。
	DefaultIdleTTL = 5 * time.Minute
	// DefaultCrashBackoff 是崩溃重建的初始退避窗口(指数增长,
	// 见 stdioPool.backoffDelay)。
	DefaultCrashBackoff = time.Second
	// maxCrashBackoff 是退避窗口上限。
	maxCrashBackoff = 60 * time.Second
	// maxResponseLineBytes 是 stdio 单行响应上限(防内存放大,
	// 超限视为响应流不可信, 重建进程)。
	maxResponseLineBytes = 8 << 20 // 8 MiB
	// maxRequestBytes 限制请求体(JSON-RPC 工具参数, 防内存放大)。
	maxRequestBytes = 1 << 20 // 1 MiB
)

// 熔断参数(var 便于测试覆盖)。
var (
	// circuitBreakThreshold 是连续失败熔断阈值: 达到后进入熔断,
	// 只按 circuitProbeInterval 探活, 不再随请求反复重建。
	circuitBreakThreshold = 8
	// circuitProbeInterval 是熔断后探活重建的间隔。
	circuitProbeInterval = 30 * time.Second
)

// Server 是 gateway 视角的 stdio MCP server 定义(来自启用中的
// mcp_servers 表, 即白名单)。
type Server struct {
	ServerID    string
	Name        string
	Command     string
	Args        []string
	Timeout     time.Duration
	MaxInstance int
	// Revision 用于配置热更新: 变化时排空旧进程滚动重建。
	Revision int64
}

// CatalogSource 是 gateway 的 MCP server 白名单来源(生产 = 只读
// postgres; 测试 = 内存)。
type CatalogSource interface {
	// EnabledServers 返回启用中的 stdio server(transport='stdio')。
	EnabledServers(ctx context.Context) ([]Server, error)
}

// CatalogError 表示白名单加载失败(DB 不可用): 一律 fail-closed。
type CatalogError struct {
	Err error
}

func (e *CatalogError) Error() string { return "mcp catalog unavailable: " + e.Err.Error() }
func (e *CatalogError) Unwrap() error { return e.Err }

// 进程宿主错误(映射为明确 HTTP 状态码, 见 handleMCPError)。
var (
	errUnknownServer  = errors.New("MCP server is not enabled or does not exist")
	errBackoff        = errors.New("stdio server is backing off after crashes")
	errCircuitOpen    = errors.New("stdio server is circuit-broken (too many crashes); re-enable or wait for probe")
	errInvalidRequest = errors.New("request body must be a JSON-RPC object")
	errProcessDead    = errors.New("stdio process is dead")
)
