package llmproxy

import (
	"context"
	"fmt"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/domain"
)

// ProviderSource returns the currently active LLM provider. The platform
// implements this with its store + cipher; the proxy never holds real keys
// statically.
type ProviderSource interface {
	GetDefaultProvider(ctx context.Context) (domain.LLMProvider, error)
}

// ProviderTypePath returns the expected upstream handler path for a provider
// type. The Worker calls the proxy using the same path it would use for the
// real upstream, so the proxy only has to validate + forward.
func ProviderTypePath(t domain.LLMProviderType) (string, error) {
	switch t {
	case domain.ProviderOpenAICompatible:
		return "/chat/completions", nil
	case domain.ProviderAnthropicMessages:
		return "/v1/messages", nil
	default:
		return "", fmt.Errorf("unsupported provider type: %s", t)
	}
}
