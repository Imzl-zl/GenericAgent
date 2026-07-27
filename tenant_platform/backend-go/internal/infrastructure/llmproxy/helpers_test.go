package llmproxy

import (
	"context"
	"errors"
	"github.com/jackc/pgx/v5"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/domain"
)

const testUpstreamKey = "real-upstream-key-do-not-leak"

// fakeCipher is a test cipher that treats the ciphertext as plaintext. It
// validates the expected key version for tests that exercise version handling.
type fakeCipher struct {
	wantVersion int
}

func (c *fakeCipher) Decrypt(ciphertext []byte, keyVersion int) ([]byte, error) {
	if c.wantVersion != 0 && keyVersion != c.wantVersion {
		return nil, errors.New("key version mismatch")
	}
	return append([]byte(nil), ciphertext...), nil
}

// fakeProviderSource returns one Provider by its durable ID.
type fakeProviderSource struct {
	provider    domain.LLMProvider
	err         error
	requestedID int64
}

func (s *fakeProviderSource) GetProvider(ctx context.Context, id int64) (domain.LLMProvider, error) {
	_ = ctx
	s.requestedID = id
	if s.err != nil {
		return domain.LLMProvider{}, s.err
	}
	if s.provider.ID != id {
		return domain.LLMProvider{}, pgx.ErrNoRows
	}
	return s.provider, nil
}

func testProvider(providerType domain.LLMProviderType, baseURL, model, key string) domain.LLMProvider {
	return domain.LLMProvider{
		ID:               1,
		ProviderType:     providerType,
		BaseURL:          baseURL,
		Model:            model,
		APIKeyCiphertext: []byte(key),
		APIKeyKeyVersion: "1",
		APIKey:           key,
		Revision:         1,
		IsDefault:        true,
		State:            "active",
	}
}
