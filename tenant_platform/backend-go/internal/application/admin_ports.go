package application

import (
	"context"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/domain"
)

// 管理端端口(审查 I-1 收敛): 应用契约统一归位到 application 层, api 只
// 依赖本包端口, 由 main 注入 postgres.Store 实现。当前 CRUD 为纯透传
// (无领域规则), 不建空壳 service——未来出现校验/审计/领域不变量时,
// 在此端口与 Store 实现之间插入 service 即可, 接口位置已就位。

// AdminCommandPort 是平台命令管理端口(迁移 0004)。
// 审查 D1(去分级): tool_policies 的 CRUD/用户分配已移除, 工具能力由静态
// policy manifest 决定; 本端口仅保留平台命令管理。
type AdminCommandPort interface {
	ListAllCommands(ctx context.Context) ([]domain.PlatformCommand, error)
	UpdateCommand(ctx context.Context, id int64, action domain.CommandAction,
		helpText string, enabled bool, sortOrder int, updatedBy int64) (domain.PlatformCommand, error)
}

// RuntimeSettingsPort 是可热更新的平台运行时设置端口。
type RuntimeSettingsPort interface {
	GetIMInboundCoalesceWindowMS(ctx context.Context) (int, error)
	UpdateIMInboundCoalesceWindowMS(ctx context.Context, windowMS int, updatedBy int64) (int, error)
	GetAgentMaxTurns(ctx context.Context) (int, error)
	UpdateAgentMaxTurns(ctx context.Context, maxTurns int, updatedBy int64) (int, error)
}

// LLMProviderPort 是上游 LLM 提供方管理端口。
type LLMProviderPort interface {
	CreateProvider(ctx context.Context, input domain.LLMProviderCreate) (domain.LLMProvider, error)
	GetProvider(ctx context.Context, id int64) (domain.LLMProvider, error)
	ListProviders(ctx context.Context) ([]domain.LLMProvider, error)
	UpdateProvider(ctx context.Context, id int64, input domain.LLMProviderUpdate) (domain.LLMProvider, error)
	SetProviderState(ctx context.Context, id int64, state domain.LLMProviderState) (domain.LLMProvider, error)
	SetDefaultProvider(ctx context.Context, id int64) error
	DeleteProvider(ctx context.Context, id int64) error
}

// MCPServerPort 是全局共享 MCP 服务器管理端口。
type MCPServerPort interface {
	CreateMCPServer(ctx context.Context, input domain.MCPServerCreate) (domain.MCPServer, error)
	ListMCPServers(ctx context.Context) ([]domain.MCPServer, error)
	UpdateMCPServer(ctx context.Context, id int64, input domain.MCPServerUpdate) (domain.MCPServer, error)
	SetMCPServerEnabled(ctx context.Context, id int64, enabled bool) (domain.MCPServer, error)
	DeleteMCPServer(ctx context.Context, id int64) error
}

// TaskStatsPort 是管理端仪表盘统计端口。
type TaskStatsPort interface {
	CountRunningTasks(ctx context.Context) (int, error)
}
