package llmproxy

import (
	"context"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/domain"
)

// ProviderSource resolves the exact Provider bound into a capability token.
// The proxy never falls back to the mutable default Provider.
type ProviderSource interface {
	GetProvider(ctx context.Context, id int64) (domain.LLMProvider, error)
}
