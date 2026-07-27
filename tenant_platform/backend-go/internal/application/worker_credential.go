package application

import (
	"context"
	"fmt"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/infrastructure/llmproxy"
)

// issueAndWriteCredential issues a capability_token for the session and writes
// a token-only mykey.py. Returns the JTI for later revocation. When
// TokenIssuer is nil (unit tests with injected DialWorker), returns "".
func (s *scheduler) issueAndWriteCredential(ctx context.Context, sessionKey string) (string, error) {
	if s.cfg.TokenIssuer == nil {
		return "", nil
	}
	provider, err := s.cfg.LLMProvider.GetDefaultProvider(ctx)
	if err != nil {
		return "", fmt.Errorf("resolve LLM provider: %w", err)
	}
	token, claims, err := s.cfg.TokenIssuer.Issue(llmproxy.CapabilitySpec{
		SessionKey:       sessionKey,
		ProviderID:       provider.ID,
		ProviderRevision: provider.Revision,
		ProviderType:     provider.ProviderType,
		Model:            provider.Model,
		PolicyVersion:    s.cfg.ModelPolicyVersion,
	})
	if err != nil {
		return "", fmt.Errorf("issue capability_token: %w", err)
	}
	if s.cfg.LLMProxyAddr != "" && s.cfg.ConfigRoot != "" {
		files, buildErr := BuildRuntimeConfig(RuntimeConfigInput{
			Generation:        1,
			ProxyBaseURL:      s.cfg.LLMProxyAddr,
			RoutingSnapshotID: fmt.Sprintf("provider-%d-revision-%d", provider.ID, provider.Revision),
			Providers:         []RuntimeProviderBinding{{Provider: provider, Token: token}},
		})
		if buildErr != nil {
			return "", fmt.Errorf("build token-only runtime config: %w", buildErr)
		}
		if err := WriteRuntimeConfigAtomic(s.configDirFor(sessionKey), files); err != nil {
			return "", fmt.Errorf("write token-only runtime config: %w", err)
		}
	}
	return claims.ID, nil
}
