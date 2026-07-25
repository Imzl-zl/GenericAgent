package llmproxy

import (
	"context"
	"errors"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/domain"
)

// fakeCipher is a test cipher that treats the ciphertext as plaintext. It
// validates the expected key version for tests that exercise version handling.
type fakeCipher struct {
	wantVersion int
}

func (c *fakeCipher) Decrypt(ciphertext []byte, keyVersion int) ([]byte, error) {
	if c.wantVersion != 0 && keyVersion != c.wantVersion {
		return nil, errors.New("key version mismatch")
	}
	return ciphertext, nil
}

// fakeProviderSource returns a fixed provider for handler tests.
type fakeProviderSource struct {
	provider domain.LLMProvider
	err      error
}

func (s *fakeProviderSource) GetDefaultProvider(ctx context.Context) (domain.LLMProvider, error) {
	_ = ctx
	return s.provider, s.err
}

func testProvider(providerType domain.LLMProviderType, baseURL, model, key string) domain.LLMProvider {
	return domain.LLMProvider{
		ProviderType:     providerType,
		BaseURL:          baseURL,
		Model:            model,
		APIKeyCiphertext: []byte(key),
		APIKeyKeyVersion: "1",
		APIKey:           key,
		IsDefault:        true,
		State:            "active",
	}
}
