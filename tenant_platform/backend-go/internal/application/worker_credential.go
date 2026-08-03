package application

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/domain"
	workerv1 "github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/gen/worker/v1"
	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/infrastructure/llmproxy"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type workerCredentialSet struct {
	Generation  uint64 // credential 版本(reload 协议递增)
	// RunnerGeneration 是签发时绑定的 Runner lease generation(方案 §7):
	// token 的 runner_generation 声明与 Worker 会话校验用它, 刷新时不变
	// (审查 C4: 与 credential generation 分离)。
	RunnerGeneration uint64
	Checksum         string
	ExpiresAt        time.Time
	JTIs             []string
	Snapshot         routingSnapshot
	MCPSnapshot      RuntimeMCPSnapshot
}

const credentialRevokeTimeout = 5 * time.Second

type pendingCredentialRefresh struct {
	Previous      workerCredentialSet
	Next          workerCredentialSet
	PreviousJSON  []byte
	RollbackCause error
}

func (s *scheduler) issueProviderCapabilities(
	ctx context.Context,
	sessionKey string,
	snapshot routingSnapshot,
	generation uint64,
) (workerCredentialSet, RuntimeConfigFiles, error) {
	mcpSnapshot, err := s.resolveMCPSnapshot(ctx)
	if err != nil {
		return workerCredentialSet{}, RuntimeConfigFiles{}, err
	}
	return s.issueProviderCapabilitiesWithRuntime(ctx, sessionKey, snapshot, mcpSnapshot, generation, generation, "")
}

func (s *scheduler) issueProviderCapabilitiesWithMCP(
	ctx context.Context,
	sessionKey string,
	snapshot routingSnapshot,
	mcpSnapshot RuntimeMCPSnapshot,
	generation uint64,
) (workerCredentialSet, RuntimeConfigFiles, error) {
	return s.issueProviderCapabilitiesWithRuntime(ctx, sessionKey, snapshot, mcpSnapshot, generation, generation, "")
}

func (s *scheduler) issueProviderCapabilitiesWithRuntime(
	ctx context.Context,
	sessionKey string,
	snapshot routingSnapshot,
	mcpSnapshot RuntimeMCPSnapshot,
	credGeneration, runnerGeneration uint64,
	taskID string,
) (workerCredentialSet, RuntimeConfigFiles, error) {
	if s.cfg.TokenIssuer == nil {
		return workerCredentialSet{}, RuntimeConfigFiles{}, nil
	}
	bindings := make([]RuntimeProviderBinding, 0, len(snapshot.Providers))
	set := workerCredentialSet{Generation: credGeneration, RunnerGeneration: runnerGeneration, Snapshot: snapshot, MCPSnapshot: mcpSnapshot}
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
		sophubToken, sophubClaims, err := s.cfg.TokenIssuer.IssueSophubToken(sessionKey, taskID, runnerGeneration, llmproxy.DefaultTokenTTL)
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
	files, err := BuildRuntimeConfig(RuntimeConfigInput{
		Generation: credGeneration, ProxyBaseURL: s.cfg.LLMProxyAddr,
		RoutingSnapshotID: snapshot.ID, Providers: bindings, MCP: mcpSnapshot,
		JTIs: set.JTIs,
		Sophub: sophub,
	})
	if err != nil {
		s.revokeCredentialSetBestEffort(ctx, set)
		return workerCredentialSet{}, RuntimeConfigFiles{}, fmt.Errorf("build token-only runtime config: %w", err)
	}
	set.Checksum = files.Checksum
	return set, files, nil
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
	set, files, err := s.issueProviderCapabilitiesWithRuntime(ctx, task.SessionKey, snapshot, mcpSnapshot, generation, generation, task.ID)
	if err != nil {
		return workerCredentialSet{}, RuntimeConfigFiles{}, err
	}
	if err := WriteRuntimeConfigAtomic(s.runtimeConfigDir(task.SessionKey), files); err != nil {
		s.revokeCredentialSetBestEffort(ctx, set)
		return workerCredentialSet{}, RuntimeConfigFiles{}, fmt.Errorf("write token-only runtime config: %w", err)
	}
	return set, files, nil
}

func (s *scheduler) credentialsNeedRefresh(set workerCredentialSet) bool {
	if set.Generation == 0 {
		return false
	}
	refreshBefore := time.Now().UTC().Add(s.cfg.MaxTaskWallClock + s.cfg.TokenRefreshSkew)
	return !set.ExpiresAt.After(refreshBefore)
}

func (s *scheduler) refreshWorkerCredentials(ctx context.Context, entry *workerEntry) error {
	if entry.pendingRefresh == nil {
		generation := entry.credentials.Generation + 1
		if generation == 0 {
			return errors.New("credential generation overflow")
		}
		configPath := filepath.Join(s.runtimeConfigDir(entry.sessionKey), runtimeConfigFilename)
		previousJSON, err := os.ReadFile(configPath)
		if err != nil {
			return fmt.Errorf("read previous runtime config: %w", err)
		}
		// Credential reload intentionally preserves the worker's immutable MCP
		// catalog. MCP changes are detected in prepareWorkerEntry and applied by
		// replacing the worker at a task boundary, never by hot-reloading tools.
		// runnerGeneration 保持当前 Runner lease generation 不变(审查 C4:
		// credential 版本与 runner generation 分离, 刷新不得改变 token 绑定)。
		newSet, files, err := s.issueProviderCapabilitiesWithRuntime(
			ctx, entry.sessionKey, entry.credentials.Snapshot,
			entry.credentials.MCPSnapshot, generation, entry.runnerGeneration,
			entry.taskID,
		)
		if err != nil {
			return err
		}
		entry.pendingRefresh = &pendingCredentialRefresh{
			Previous: entry.credentials, Next: newSet, PreviousJSON: previousJSON,
		}
		if err := WriteRuntimeConfigAtomic(s.runtimeConfigDir(entry.sessionKey), files); err != nil {
			entry.pendingRefresh.RollbackCause = fmt.Errorf("write refreshed runtime config: %w", err)
			return s.rollbackPendingCredentialRefresh(ctx, entry)
		}
	}
	if entry.pendingRefresh.RollbackCause != nil {
		return s.rollbackPendingCredentialRefresh(ctx, entry)
	}
	return s.acknowledgePendingCredentialRefresh(ctx, entry)
}

func (s *scheduler) acknowledgePendingCredentialRefresh(ctx context.Context, entry *workerEntry) error {
	pending := entry.pendingRefresh
	response, err := entry.client.ReloadCredentials(ctx, &workerv1.ReloadCredentialsRequest{
		CredentialGeneration: pending.Next.Generation,
		ConfigChecksum:       pending.Next.Checksum,
		// 控制面身份 fencing(方案 §7, 审查): 绑定当前 workspace/generation。
		WorkspaceKey:     entry.sessionKey,
		RunnerGeneration: entry.runnerGeneration,
	})
	if err != nil {
		if !isDefinitiveReloadRejection(err) {
			return fmt.Errorf("Worker credential reload outcome is ambiguous: %w", err)
		}
		pending.RollbackCause = fmt.Errorf("Worker rejected credential reload: %w", err)
		return s.rollbackPendingCredentialRefresh(ctx, entry)
	}
	if response.GetCredentialGeneration() != pending.Next.Generation ||
		response.GetConfigChecksum() != pending.Next.Checksum {
		pending.RollbackCause = errors.New("Worker credential acknowledgment mismatch")
		return s.rollbackPendingCredentialRefresh(ctx, entry)
	}

	entry.credentials = pending.Next
	entry.pendingRefresh = nil
	if err := s.revokeCredentialSet(ctx, pending.Previous); err != nil {
		entry.pendingRevocations = append(entry.pendingRevocations, pending.Previous)
		return fmt.Errorf("revoke previous Worker credentials: %w", err)
	}
	return nil
}

func isDefinitiveReloadRejection(err error) bool {
	code := status.Code(err)
	return code == codes.FailedPrecondition || code == codes.InvalidArgument
}

func (s *scheduler) rollbackPendingCredentialRefresh(ctx context.Context, entry *workerEntry) error {
	pending := entry.pendingRefresh
	configPath := filepath.Join(s.runtimeConfigDir(entry.sessionKey), runtimeConfigFilename)
	restoreErr := writeFileAtomic(configPath, pending.PreviousJSON, 0o640)
	revokeErr := s.revokeCredentialSet(ctx, pending.Next)
	cause := pending.RollbackCause
	if restoreErr == nil && revokeErr == nil {
		entry.pendingRefresh = nil
	}
	return errors.Join(cause, restoreErr, revokeErr)
}

func (s *scheduler) flushPendingCredentialRevocations(ctx context.Context, entry *workerEntry) error {
	if len(entry.pendingRevocations) == 0 {
		return nil
	}
	remaining := entry.pendingRevocations[:0]
	var flushErr error
	for _, set := range entry.pendingRevocations {
		if err := s.revokeCredentialSet(ctx, set); err != nil {
			remaining = append(remaining, set)
			flushErr = errors.Join(flushErr, err)
		}
	}
	entry.pendingRevocations = remaining
	return flushErr
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
