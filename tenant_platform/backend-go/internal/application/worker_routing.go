package application

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/domain"
)

type routingProvider struct {
	ID            int64                    `json:"id"`
	Revision      int64                    `json:"revision"`
	ProviderType  domain.LLMProviderType   `json:"provider_type"`
	Model         string                   `json:"model"`
	RuntimeName   string                   `json:"runtime_name"`
	SessionConfig domain.GASessionConfig    `json:"session_config"`
	Capabilities  []domain.ProviderCapability `json:"capabilities"`
}

func (p routingProvider) runtimeProvider() domain.LLMProvider {
	return domain.LLMProvider{
		ID: p.ID, Revision: p.Revision, ProviderType: p.ProviderType,
		Model: p.Model, SessionConfig: p.SessionConfig, State: "active",
		Capabilities: p.Capabilities,
	}
}

// HasCapability 判断该路由 provider 是否具备某能力维度(省略 = chat)。
func (p routingProvider) HasCapability(c domain.ProviderCapability) bool {
	caps := p.Capabilities
	if len(caps) == 0 {
		caps = []domain.ProviderCapability{domain.ProviderCapabilityChat}
	}
	for _, have := range caps {
		if have == c {
			return true
		}
	}
	return false
}

type routingSnapshot struct {
	ID        string
	Providers []routingProvider
}

func (s *scheduler) resolveRoutingSnapshot(ctx context.Context) (routingSnapshot, error) {
	providers, err := s.cfg.LLMProvider.ListActiveProviders(ctx)
	if err != nil {
		return routingSnapshot{}, fmt.Errorf("list active LLM providers: %w", err)
	}
	if len(providers) == 0 {
		return routingSnapshot{}, errors.New("no active LLM providers")
	}
	defaultCount := 0
	seen := make(map[int64]struct{}, len(providers))
	for index, provider := range providers {
		if provider.IsDefault {
			defaultCount++
			if index != 0 {
				return routingSnapshot{}, errors.New("default LLM provider must be first")
			}
		}
		if _, duplicate := seen[provider.ID]; duplicate {
			return routingSnapshot{}, fmt.Errorf("duplicate LLM provider id %d", provider.ID)
		}
		seen[provider.ID] = struct{}{}
		if !provider.IsActive() || provider.ID <= 0 || provider.Revision <= 0 {
			return routingSnapshot{}, fmt.Errorf("invalid active LLM provider %d", provider.ID)
		}
		if err := provider.SessionConfig.Validate(provider.ProviderType); err != nil {
			return routingSnapshot{}, fmt.Errorf("provider %d session config: %w", provider.ID, err)
		}
	}
	if defaultCount != 1 {
		return routingSnapshot{}, fmt.Errorf("exactly one default LLM provider is required, got %d", defaultCount)
	}

	routing := make([]routingProvider, 0, len(providers))
	for _, provider := range providers {
		routing = append(routing, newRoutingProvider(provider))
	}
	encoded, err := json.Marshal(routing)
	if err != nil {
		return routingSnapshot{}, fmt.Errorf("marshal routing snapshot: %w", err)
	}
	digest := sha256.Sum256(encoded)
	return routingSnapshot{ID: "sha256:" + hex.EncodeToString(digest[:]), Providers: routing}, nil
}

func newRoutingProvider(provider domain.LLMProvider) routingProvider {
	return routingProvider{
		ID: provider.ID, Revision: provider.Revision, ProviderType: provider.ProviderType,
		Model: provider.Model, RuntimeName: runtimeProviderName(provider.ID),
		SessionConfig: provider.SessionConfig,
		Capabilities:  provider.Capabilities,
	}
}

type routingAuditDetail struct {
	ProviderIDs       []int64 `json:"provider_ids"`
	RoutingSnapshotID string  `json:"routing_snapshot_id,omitempty"`
	Result            string  `json:"result"`
	ErrorCode         string  `json:"error_code,omitempty"`
}

func (s *scheduler) auditRoutingBinding(
	ctx context.Context,
	task domain.Task,
	entry *workerEntry,
	result string,
	errorCode string,
) {
	if s.cfg.Audit == nil {
		return
	}
	detail := routingAuditDetail{Result: result, ErrorCode: errorCode, ProviderIDs: []int64{}}
	if entry != nil {
		entry.lifecycleMu.Lock()
		detail.RoutingSnapshotID = entry.credentials.Snapshot.ID
		for _, provider := range entry.credentials.Snapshot.Providers {
			detail.ProviderIDs = append(detail.ProviderIDs, provider.ID)
		}
		entry.lifecycleMu.Unlock()
	}
	encoded, err := json.Marshal(detail)
	if err != nil {
		slog.ErrorContext(ctx, "scheduler: marshal routing audit failed", "task_id", task.ID, "error", err)
		return
	}
	if err := s.cfg.Audit.AppendAuditEvent(ctx, domain.AuditEvent{
		ActorUserID: task.RequesterID, Action: domain.AuditLLMRoutingBound,
		TargetType: "task", TargetID: task.ID, SessionKey: task.SessionKey,
		Detail: encoded, PolicyVersion: s.cfg.ModelPolicyVersion,
	}); err != nil {
		slog.ErrorContext(ctx, "scheduler: append routing audit failed", "task_id", task.ID, "error", err)
	}
}
