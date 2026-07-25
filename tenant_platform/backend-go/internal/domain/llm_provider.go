package domain

import "time"

// LLMProviderType selects which upstream protocol the Worker speaks and which
// variable name the platform writes into mykey.py.
type LLMProviderType string

const (
	// ProviderOpenAICompatible uses NativeOAISession and forwards to
	// upstream /chat/completions.
	ProviderOpenAICompatible LLMProviderType = "openai_compatible"
	// ProviderAnthropicMessages uses NativeClaudeSession and forwards to
	// upstream /v1/messages.
	ProviderAnthropicMessages LLMProviderType = "anthropic_messages"
)

// LLMProvider is an admin-configured upstream LLM. Only one row should be
// default at a time; the platform schedules tasks against the current default.
type LLMProvider struct {
	ID               int64
	Name             string
	ProviderType     LLMProviderType
	BaseURL          string
	Model            string
	APIKeyCiphertext []byte
	APIKeyKeyVersion string
	// APIKey is the decrypted plaintext key. It is populated at runtime by the
	// LLM Proxy and MUST NOT be persisted to the database.
	APIKey    string
	IsDefault bool
	State     string // active | disabled
	CreatedAt time.Time
	UpdatedAt time.Time
}

// IsActive returns true for non-disabled providers.
func (p LLMProvider) IsActive() bool { return p.State != "disabled" }
