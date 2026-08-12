package domain

import (
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"
)

const (
	DefaultMCPTimeoutSeconds = 30
	MaxMCPTimeoutSeconds     = 300

	// MCPTransportHTTP 是 Streamable HTTP 型 MCP Server(默认)。
	MCPTransportHTTP = "http"
	// MCPTransportStdio 是 stdio 型 MCP Server: Worker 沙箱内进程宿主,
	// JSON-RPC over stdin/stdout(主流 mcp.json 兼容; 2026-08 恢复, 见
	// mcp_client.py MCPStdioClient)。stdio 调用不经过 Platform proxy,
	// 不参与 proxy 每调用计量(配额仍按快照签发时 MCPQuotaAvailable 门控)。
	MCPTransportStdio = "stdio"

	// MCPIsolationShared: 无状态无凭据工具, 跨租户共享进程(如 pandoc)。
	MCPIsolationShared = "shared"
	// MCPIsolationWorkspace: 有状态/带凭据工具, 每工作区独立进程(预留)。
	MCPIsolationWorkspace = "workspace"

	DefaultMCPMaxInstances = 1
	MaxMCPMaxInstances     = 16
)

// mcpCommandPattern: 绝对路径或裸命令名(不含空白/引号/重定向/变量展开)。
// exec.Command 不经 shell 拼接, 此校验是 fail-fast(防手误)而非安全边界;
// 管理员可信(D1 可信部署), 命令在 Worker 沙箱内执行。
var mcpCommandPattern = regexp.MustCompile(`^[A-Za-z0-9_./+\-]{1,256}$`)

const (
	MaxMCPArgs        = 64
	MaxMCPArgLength   = 256
	MaxMCPCommandSize = 256
)

var (
	ErrMCPServerNotFound = errors.New("MCP server not found")
	ErrMCPServerConflict = errors.New("MCP server conflict")
)

// mcpReservedHeaders 是禁止注入的请求头(hop-by-hop 与代理自有语义头):
// proxy 只透传 MCP 语义头, 管理员配置的头若覆盖它们会破坏转发或造成
// 请求走私/主机头注入, fail-closed 拒绝。
var mcpReservedHeaders = map[string]struct{}{
	"host":               {},
	"content-length":     {},
	"transfer-encoding":  {},
	"connection":         {},
	"keep-alive":         {},
	"proxy-authenticate": {},
	"proxy-authorization": {},
	"te":                 {},
	"trailer":            {},
	"upgrade":            {},
}
// MCPQuotaPeriod 是配额周期粒度(day | month)。
type MCPQuotaPeriod string

const (
	MCPQuotaPeriodDay   MCPQuotaPeriod = "day"
	MCPQuotaPeriodMonth MCPQuotaPeriod = "month"
)

// MCPQuotaLimit 是每用户 × 每 server × 周期的配额限额(admin 配置真值)。
// 无行 = 默认放行(与 max_turns 行为一致); 超限由 proxy 原子扣减强制。
type MCPQuotaLimit struct {
	OwnerKey   string
	ServerID   string
	Period     MCPQuotaPeriod
	LimitCount int64
}

func (l MCPQuotaLimit) Validate() error {
	if strings.TrimSpace(l.OwnerKey) == "" {
		return fmt.Errorf("owner_key is required")
	}
	if strings.TrimSpace(l.ServerID) == "" {
		return fmt.Errorf("server_id is required")
	}
	if l.Period != MCPQuotaPeriodDay && l.Period != MCPQuotaPeriodMonth {
		return fmt.Errorf("period must be day or month")
	}
	if l.LimitCount <= 0 {
		return fmt.Errorf("limit_count must be positive")
	}
	return nil
}

var mcpServerKeyPattern = regexp.MustCompile(`^[A-Za-z0-9_]{1,32}$`)

type MCPServerCreate struct {
	ServerKey      string
	Name           string
	URL            string
	TimeoutSeconds int
	// Headers 是 proxy 转发时注入上游的请求头(Authorization/x-api-key 等),
	// 平台侧持有: 绝不下发 worker 快照, admin API 回显掩码。
	Headers map[string]string
	// Transport 接入方式: http(默认, Streamable HTTP) | stdio(Worker 沙箱内
	// 进程宿主)。stdio 调用不经过 Platform proxy, 不参与每调用计量。
	Transport string
	// Command/Args 是 stdio 进程的命令与参数(exec.Command 直接执行, 不经
	// shell); http 下必须为空。
	Command string
	Args    []string
	// Isolation 隔离维度: shared(默认) | workspace(预留, v1 拒绝)。
	Isolation string
	// MaxInstances 保留字段(与 isolation 同源遗留)。
	MaxInstances int
}

type MCPServerUpdate struct {
	MCPServerCreate
}

type MCPServer struct {
	ID             int64
	ServerKey      string
	Name           string
	URL            string
	TimeoutSeconds int
	Headers        map[string]string
	Transport      string
	Command        string
	Args           []string
	Isolation      string
	MaxInstances   int
	Enabled        bool
	Revision       int64
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

func ValidateMCPServerInput(input MCPServerCreate) error {
	if !mcpServerKeyPattern.MatchString(strings.TrimSpace(input.ServerKey)) {
		return fmt.Errorf("server_key must contain 1-32 letters, digits, or underscores")
	}
	if strings.TrimSpace(input.Name) == "" {
		return fmt.Errorf("name is required")
	}
	transport := strings.TrimSpace(input.Transport)
	if transport == "" {
		transport = MCPTransportHTTP
	}
	if transport != MCPTransportHTTP && transport != MCPTransportStdio {
		return fmt.Errorf("transport must be %q or %q", MCPTransportHTTP, MCPTransportStdio)
	}
	for name := range input.Headers {
		lower := strings.ToLower(strings.TrimSpace(name))
		if lower == "" {
			return fmt.Errorf("header name must not be empty")
		}
		if _, reserved := mcpReservedHeaders[lower]; reserved {
			return fmt.Errorf("header %q is reserved and must not be configured", name)
		}
	}
	isolation := strings.TrimSpace(input.Isolation)
	if isolation == "" {
		isolation = MCPIsolationShared
	}
	if isolation != MCPIsolationShared && isolation != MCPIsolationWorkspace {
		return fmt.Errorf("isolation must be %q or %q", MCPIsolationShared, MCPIsolationWorkspace)
	}
	// v1 fail-closed: workspace 隔离(带凭据/有状态)留待 secret 管理就绪。
	if isolation == MCPIsolationWorkspace {
		return fmt.Errorf("isolation %q is not supported yet", MCPIsolationWorkspace)
	}
	maxInstances := input.MaxInstances
	if maxInstances == 0 {
		maxInstances = DefaultMCPMaxInstances
	}
	if maxInstances < 1 || maxInstances > MaxMCPMaxInstances {
		return fmt.Errorf("max_instances must be between 1 and %d", MaxMCPMaxInstances)
	}
	if input.TimeoutSeconds <= 0 || input.TimeoutSeconds > MaxMCPTimeoutSeconds {
		return fmt.Errorf("timeout_seconds must be between 1 and %d", MaxMCPTimeoutSeconds)
	}
	switch transport {
	case MCPTransportHTTP:
		if strings.TrimSpace(input.Command) != "" || len(input.Args) > 0 {
			return fmt.Errorf("command/args are only valid with transport %q", MCPTransportStdio)
		}
		parsed, err := url.Parse(strings.TrimSpace(input.URL))
		if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
			return fmt.Errorf("url must be an absolute http or https URL")
		}
		if parsed.User != nil || parsed.Fragment != "" || parsed.Opaque != "" {
			return fmt.Errorf("url must contain no credentials or fragment")
		}
	case MCPTransportStdio:
		if strings.TrimSpace(input.URL) != "" {
			return fmt.Errorf("url must be empty for transport %q", MCPTransportStdio)
		}
		if len(input.Headers) > 0 {
			return fmt.Errorf("headers are not supported for transport %q (stdio has no HTTP request headers)", MCPTransportStdio)
		}
		command := strings.TrimSpace(input.Command)
		if command == "" {
			return fmt.Errorf("command is required for transport %q", MCPTransportStdio)
		}
		if !mcpCommandPattern.MatchString(command) {
			return fmt.Errorf("command %q is invalid: use an absolute path or a bare executable name (letters, digits, _, ., /, +, -)", command)
		}
		if len(input.Args) > MaxMCPArgs {
			return fmt.Errorf("args must not exceed %d entries", MaxMCPArgs)
		}
		for _, arg := range input.Args {
			if arg == "" || len(arg) > MaxMCPArgLength || strings.ContainsRune(arg, '\x00') {
				return fmt.Errorf("each arg must be 1-%d chars without NUL bytes", MaxMCPArgLength)
			}
		}
	}
	return nil
}

