package application

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/domain"
	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/infrastructure/llmproxy"
)

type workerCredentialSet struct {
	// RunnerGeneration 是签发时绑定的 Runner lease generation(方案 §7):
	// token 的 runner_generation 声明与 Worker 会话校验用它。
	RunnerGeneration uint64
	ExpiresAt        time.Time
	JTIs             []string
	// ControlJTI 是独立签发的控制 capability JTI(round11 审查 I4): 控制
	// RPC(CancelTask/Shutdown/BeginCheckpoint)必须使用它,
	// 不得复用 LLM/Sophub capability——LLM token 随调用发给 llm-proxy,
	// 暴露面更大; 独立 control token 只在容器内传递。
	ControlJTI      string
	Snapshot         routingSnapshot
	MCPSnapshot      RuntimeMCPSnapshot
}

const credentialRevokeTimeout = 5 * time.Second

func (s *scheduler) issueProviderCapabilitiesWithRuntime(
	ctx context.Context,
	sessionKey string,
	snapshot routingSnapshot,
	mcpSnapshot RuntimeMCPSnapshot,
	runnerGeneration uint64,
	taskID string,
	ownerKey string,
) (workerCredentialSet, RuntimeConfigFiles, error) {
	if s.cfg.TokenIssuer == nil {
		return workerCredentialSet{}, RuntimeConfigFiles{}, nil
	}
	bindings := make([]RuntimeProviderBinding, 0, len(snapshot.Providers))
	set := workerCredentialSet{RunnerGeneration: runnerGeneration, Snapshot: snapshot, MCPSnapshot: mcpSnapshot}
	// 方案 §7: capability 必须包含操作与预算。预算与 RuntimePolicy 对齐,
	// 其中 max_turns 由 llm-proxy 按 JTI 原子计量强制执行(审查 R4-I9:
	// Runner 内代码绕过 Worker 限制直接刷 Proxy 时, 请求次数被硬性截断)。
	maxTurns, err := s.agentMaxTurns(ctx)
	if err != nil {
		return workerCredentialSet{}, RuntimeConfigFiles{}, fmt.Errorf("resolve agent max turns: %w", err)
	}
	budget := fmt.Sprintf(`{"max_turns":%d,"max_history_bytes":%d,"max_working_bytes":%d,"max_output_bytes":%d}`, maxTurns, defaultMaxHistoryBytes, defaultMaxWorkingBytes, defaultMaxOutputBytes)
	for _, routed := range snapshot.Providers {
		token, claims, err := s.cfg.TokenIssuer.Issue(llmproxy.CapabilitySpec{
			SessionKey: sessionKey, ProviderID: routed.ID, ProviderRevision: routed.Revision,
			ProviderType: routed.ProviderType, Model: routed.Model,
			PolicyVersion: s.cfg.ModelPolicyVersion,
			TaskID: taskID, RunnerGeneration: runnerGeneration,
			Operation: "llm.chat", Budget: budget,
		})
		if err != nil {
			s.revokeCredentialSetBestEffort(ctx, set)
			return workerCredentialSet{}, RuntimeConfigFiles{}, fmt.Errorf("issue capability for provider %d: %w", routed.ID, err)
		}
		if claims.ExpiresAt == nil || claims.IssuedAt == nil {
			set.JTIs = append(set.JTIs, claims.ID)
			s.revokeCredentialSetBestEffort(ctx, set)
			return workerCredentialSet{}, RuntimeConfigFiles{}, fmt.Errorf("provider %d capability timestamps are incomplete", routed.ID)
		}
		expiresAt := claims.ExpiresAt.Time.UTC()
		issuedAt := claims.IssuedAt.Time.UTC()
		if expiresAt.Sub(issuedAt) < s.cfg.MaxTaskWallClock+s.cfg.TokenRefreshSkew {
			set.JTIs = append(set.JTIs, claims.ID)
			s.revokeCredentialSetBestEffort(ctx, set)
			return workerCredentialSet{}, RuntimeConfigFiles{}, errors.New("issued capability lifetime cannot cover task wall clock")
		}
		if set.ExpiresAt.IsZero() || expiresAt.Before(set.ExpiresAt) {
			set.ExpiresAt = expiresAt
		}
		set.JTIs = append(set.JTIs, claims.ID)
		bindings = append(bindings, RuntimeProviderBinding{Provider: routed.runtimeProvider(), Token: token})
	}
	// Sophub proxy capability(方案 §5.2): 经同一签发体系, audience=ga-sophub-proxy。
	// 绑定 task_id + runner_generation(方案 §7), 并将 JTI 纳入同一撤销集合:
	// 终态撤销后旧 task 的 sophub token 立即失效。
	var sophub *RuntimeSophubProxy
	if s.cfg.TokenIssuer != nil && s.cfg.SophubProxyBaseURL != "" {
		// 审查 F10: sophub capability 必须携带预算——Runner 不得在 token
		// 有效期内无限调用代理(search/install 合计按 JTI 原子计量)。
		sophubBudget := fmt.Sprintf(`{"max_turns":%d}`, maxTurns)
		sophubToken, sophubClaims, err := s.cfg.TokenIssuer.IssueSophubToken(sessionKey, taskID, runnerGeneration, llmproxy.DefaultTokenTTL, sophubBudget)
		if err != nil {
			s.revokeCredentialSetBestEffort(ctx, set)
			return workerCredentialSet{}, RuntimeConfigFiles{}, fmt.Errorf("issue sophub capability: %w", err)
		}
		set.JTIs = append(set.JTIs, sophubClaims.ID)
		if sophubClaims.ExpiresAt != nil {
			expiresAt := sophubClaims.ExpiresAt.Time.UTC()
			if set.ExpiresAt.IsZero() || expiresAt.Before(set.ExpiresAt) {
				set.ExpiresAt = expiresAt
			}
		}
		sophub = &RuntimeSophubProxy{BaseURL: s.cfg.SophubProxyBaseURL, CapabilityToken: sophubToken}
	}
	// MCP proxy capability: 与 Sophub 同模式(audience=ga-mcp-proxy)。
	// 仅在 MCP 快照含 server 且配置了 proxy 地址时签发——Runner 无公网出口,
	// 外部 MCP Server 必须经 Platform 受控代理(server_id → URL 映射即白名单)。
	// JTI 纳入同一撤销集合 + 预算计量(无预算 fail-closed, 同审查 F10)。
	if s.cfg.TokenIssuer != nil && s.cfg.MCPProxyBaseURL != "" && len(mcpSnapshot.Servers) > 0 {
		mcpBudget := fmt.Sprintf(`{"max_turns":%d}`, maxTurns)
		mcpToken, mcpClaims, err := s.cfg.TokenIssuer.IssueMCPToken(sessionKey, taskID, runnerGeneration, llmproxy.DefaultTokenTTL, mcpBudget)
		if err != nil {
			s.revokeCredentialSetBestEffort(ctx, set)
			return workerCredentialSet{}, RuntimeConfigFiles{}, fmt.Errorf("issue mcp capability: %w", err)
		}
		set.JTIs = append(set.JTIs, mcpClaims.ID)
		if mcpClaims.ExpiresAt != nil {
			expiresAt := mcpClaims.ExpiresAt.Time.UTC()
			if set.ExpiresAt.IsZero() || expiresAt.Before(set.ExpiresAt) {
				set.ExpiresAt = expiresAt
			}
		}
		mcpSnapshot.Proxy = &RuntimeMCPProxy{
			BaseURL:         s.cfg.MCPProxyBaseURL,
			CapabilityToken: mcpToken,
		}
	}
	// round11 审查(I4): 独立签发的 control capability——控制 RPC
	// (CancelTask/Shutdown/BeginCheckpoint)使用它而不是任意 LLM/Sophub JTI。
	// JTI 纳入同一持久化/撤销集合。
	// Worker 侧只持有 JTI 值(非完整 JWT), 无法解析 claims——用 "ctrl:" 前缀
	// 标记 control JTI, 成员检查(集合)仍保证真实性; llm-proxy 只接受
	// LLM audience 的 token, control token 不会到达 proxy, 前缀不影响校验。
	_, controlClaims, err := s.cfg.TokenIssuer.IssueControlToken(sessionKey, taskID, runnerGeneration, llmproxy.DefaultTokenTTL)
	if err != nil {
		s.revokeCredentialSetBestEffort(ctx, set)
		return workerCredentialSet{}, RuntimeConfigFiles{}, fmt.Errorf("issue control capability: %w", err)
	}
	set.ControlJTI = controlJTIPrefix + controlClaims.ID
	set.JTIs = append(set.JTIs, set.ControlJTI)
	if controlClaims.ExpiresAt != nil {
		expiresAt := controlClaims.ExpiresAt.Time.UTC()
		if set.ExpiresAt.IsZero() || expiresAt.Before(set.ExpiresAt) {
			set.ExpiresAt = expiresAt
		}
	}
	files, err := BuildRuntimeConfig(RuntimeConfigInput{
		ProxyBaseURL: s.cfg.LLMProxyAddr,
		RoutingSnapshotID: snapshot.ID, Providers: bindings, MCP: mcpSnapshot,
		JTIs: set.JTIs,
		Sophub: sophub,
	})
	if err != nil {
		s.revokeCredentialSetBestEffort(ctx, set)
		return workerCredentialSet{}, RuntimeConfigFiles{}, fmt.Errorf("build token-only runtime config: %w", err)
	}
	// 审查 F1: JTI 必须在 token 暴露给 Runner 之前持久化(写 runtime config /
	// 启动容器都发生在本函数返回之后)。若 Platform 在签发后、暴露前崩溃,
	// 恢复路径(RecoverAfterRestart)依据 tasks.capability_jtis 撤销 token;
	// 未持久化则旧 token 在 TTL 内无人能撤销。
	// 失败 fail-closed: 就地撤销已签发 token 并中止, 不允许在撤销依据
	// 缺失时继续。taskID 为空(loopback/测试签发)或 Store 未接线(单测)
	// 时跳过——生产 NewScheduler 强制 Store 非 nil。
	if taskID != "" && s.cfg.Store != nil && len(set.JTIs) > 0 {
		if err := s.cfg.Store.SetTaskCapabilityJTIs(ctx, taskID, s.cfg.PlatformInstanceID, set.JTIs); err != nil {
			s.revokeCredentialSetBestEffort(ctx, set)
			return workerCredentialSet{}, RuntimeConfigFiles{}, fmt.Errorf("persist task capability jtis before exposure: %w", err)
		}
	}
	return set, files, nil
}

// filterMCPServersByQuota 按用户配额过滤快照 server: 无限额行/未耗尽保留,
// 任一周期耗尽的剔除。配额源不可用时 fail-closed(剔除全部, 不冒泄漏风险)。
func (s *scheduler) filterMCPServersByQuota(ctx context.Context, ownerKey string, snapshot RuntimeMCPSnapshot) RuntimeMCPSnapshot {
	if s.cfg.MCPServer == nil || len(snapshot.Servers) == 0 {
		return snapshot
	}
	filtered := make([]RuntimeMCPServer, 0, len(snapshot.Servers))
	for _, server := range snapshot.Servers {
		available, err := s.cfg.MCPServer.MCPQuotaAvailable(ctx, ownerKey, server.ServerID)
		if err != nil {
			slog.ErrorContext(ctx, "mcp quota check failed, excluding server",
				"server_id", server.ServerID, "error", err)
			continue
		}
		if !available {
			slog.InfoContext(ctx, "mcp server excluded: user quota exhausted",
				"server_id", server.ServerID, "owner_key", ownerKey)
			continue
		}
		filtered = append(filtered, server)
	}
	snapshot.Servers = filtered
	return snapshot
}

func (s *scheduler) issueInitialWorkerCredentials(
	ctx context.Context, task domain.Task, generation uint64,
) (workerCredentialSet, RuntimeConfigFiles, error) {
	if s.cfg.TokenIssuer == nil {
		return workerCredentialSet{}, RuntimeConfigFiles{}, nil
	}
	snapshot, err := s.resolveRoutingSnapshot(ctx)
	if err != nil {
		return workerCredentialSet{}, RuntimeConfigFiles{}, err
	}
	mcpSnapshot, err := s.resolveMCPSnapshot(ctx)
	if err != nil {
		return workerCredentialSet{}, RuntimeConfigFiles{}, err
	}
	set, files, err := s.issueProviderCapabilitiesWithRuntime(ctx, task.SessionKey, snapshot, mcpSnapshot, generation, task.ID, strconv.FormatInt(task.RequesterID, 10))
	if err != nil {
		return workerCredentialSet{}, RuntimeConfigFiles{}, err
	}
	if err := WriteRuntimeConfigAtomic(s.runtimeConfigDir(task.SessionKey, generation), files); err != nil {
		s.revokeCredentialSetBestEffort(ctx, set)
		return workerCredentialSet{}, RuntimeConfigFiles{}, fmt.Errorf("write token-only runtime config: %w", err)
	}
	return set, files, nil
}

func (s *scheduler) revokeCredentialSet(ctx context.Context, set workerCredentialSet) error {
	if s.cfg.CapabilityStore == nil || len(set.JTIs) == 0 {
		return nil
	}
	var revokeErr error
	for _, jti := range set.JTIs {
		if err := s.cfg.CapabilityStore.RevokeCapability(ctx, jti, set.ExpiresAt); err != nil {
			revokeErr = errors.Join(revokeErr, err)
		}
	}
	return revokeErr
}

func (s *scheduler) revokeCredentialSetBestEffort(ctx context.Context, set workerCredentialSet) {
	revokeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), credentialRevokeTimeout)
	defer cancel()
	if err := s.revokeCredentialSet(revokeCtx, set); err != nil {
		slog.WarnContext(ctx, "scheduler: capability revocation failed",
			"count", len(set.JTIs), "routing_snapshot_id", set.Snapshot.ID, "error", err)
	}
}
