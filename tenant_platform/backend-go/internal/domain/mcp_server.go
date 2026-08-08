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

	// MCPTransportHTTP 是 Streamable HTTP 型 MCP Server(现有行为)。
	MCPTransportHTTP = "http"
	// MCPTransportStdio 是 stdio 型 MCP Server(uvx/npx 启动的本地进程),
	// 由 mcp-gateway 托管(见 tenant_platform/docs/MCP_GATEWAY_DESIGN.zh-CN.md)。
	MCPTransportStdio = "stdio"

	// MCPIsolationShared: 无状态无凭据工具, 跨租户共享进程(如 pandoc)。
	MCPIsolationShared = "shared"
	// MCPIsolationWorkspace: 有状态/带凭据工具, 每工作区独立进程(预留)。
	MCPIsolationWorkspace = "workspace"

	// MCPStdioCommandPrefix 是 stdio 命令白名单前缀: 只允许镜像预装工具集。
	// 工具集变更 = 镜像变更(与 Runner 镜像能力同哲学, 防止管理员任意命令 RCE)。
	MCPStdioCommandPrefix = "/opt/mcp-tools/"

	DefaultMCPMaxInstances = 1
	MaxMCPMaxInstances     = 16
)

var (
	ErrMCPServerNotFound = errors.New("MCP server not found")
	ErrMCPServerConflict = errors.New("MCP server conflict")
)

var mcpServerKeyPattern = regexp.MustCompile(`^[A-Za-z0-9_]{1,32}$`)

type MCPServerCreate struct {
	ServerKey      string
	Name           string
	URL            string
	TimeoutSeconds int
	// Transport 是接入方式: http(默认) | stdio。
	Transport string
	// Command/Args 仅 stdio 使用: 白名单绝对路径 + 参数数组。
	Command string
	Args    []string
	// Isolation 隔离维度: shared(默认) | workspace(预留, v1 拒绝)。
	Isolation string
	// MaxInstances stdio 进程数上限(shared 池 / workspace 每工作区上限)。
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
			return fmt.Errorf("command/args are only valid for stdio transport")
		}
		parsed, err := url.Parse(strings.TrimSpace(input.URL))
		if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
			return fmt.Errorf("url must be an absolute http or https URL")
		}
		if parsed.User != nil || parsed.Fragment != "" || parsed.Opaque != "" {
			return fmt.Errorf("url must contain no credentials or fragment")
		}
	case MCPTransportStdio:
		command := strings.TrimSpace(input.Command)
		if !strings.HasPrefix(command, MCPStdioCommandPrefix) {
			return fmt.Errorf("command must be an absolute path under %s", MCPStdioCommandPrefix)
		}
		if strings.ContainsAny(command, " \t\n\r\x00") {
			return fmt.Errorf("command must contain no whitespace or NUL")
		}
		if len(input.Args) == 0 {
			// args 可空: 镜像预装工具可能无需参数(如 /opt/mcp-tools/mcp-pandoc)。
		} else {
			for _, arg := range input.Args {
				if strings.TrimSpace(arg) == "" || strings.ContainsRune(arg, '\x00') {
					return fmt.Errorf("args must be non-empty and contain no NUL")
				}
			}
		}
		// stdio 的 url 必须为空: gateway 路由由平台统一合成
		// (MCPServerGatewayURL), 管理员不需要也不应知道 gateway 内部地址。
		if strings.TrimSpace(input.URL) != "" {
			return fmt.Errorf("url must be empty for stdio transport (gateway route is synthesized by the platform)")
		}
	}
	return nil
}

// MCPServerGatewayURL 是 stdio MCP server 的 gateway 路由合成函数
// (平台内唯一实现: 快照下发与 proxy resolve 共用, 避免两处各自拼 URL)。
// base 形如 http://mcp-gateway:8083, serverKey 是 mcp_servers.server_key。
func MCPServerGatewayURL(base, serverKey string) string {
	return strings.TrimRight(strings.TrimSpace(base), "/") + "/v1/mcp/" + serverKey
}
