package domain

import (
	"encoding/json"
	"time"
)

// LLMProviderType selects which GA Session type to use.
type LLMProviderType string

const (
	// ProviderNativeOAI uses NativeOAISession (OpenAI protocol + native tools).
	ProviderNativeOAI LLMProviderType = "native_oai"
	// ProviderNativeClaude uses NativeClaudeSession (Anthropic protocol + native tools).
	ProviderNativeClaude LLMProviderType = "native_claude"

	// 兼容旧类型（迁移后会被替换）
	ProviderOpenAICompatible  LLMProviderType = "openai_compatible"
	ProviderAnthropicMessages LLMProviderType = "anthropic_messages"
)

// LLMProviderConfig holds GA Core configuration fields (JSONB in database).
type LLMProviderConfig struct {
	// ── 推理 / 思考 ──
	ThinkingType        string `json:"thinking_type,omitempty"`         // adaptive | enabled | disabled
	ThinkingBudgetTokens int    `json:"thinking_budget_tokens,omitempty"` // 仅 thinking_type=enabled 时
	ReasoningEffort     string `json:"reasoning_effort,omitempty"`      // none | minimal | low | medium | high | xhigh

	// ── 采样 ──
	Temperature float64 `json:"temperature,omitempty"` // 默认 1.0
	MaxTokens   int     `json:"max_tokens,omitempty"`  // 默认 8192

	// ── 容量 / 超时 ──
	ContextWin     int `json:"context_win,omitempty"`     // 默认 30000
	MaxRetries     int `json:"max_retries,omitempty"`     // 默认 1
	ConnectTimeout int `json:"connect_timeout,omitempty"` // 秒，默认 5
	ReadTimeout    int `json:"read_timeout,omitempty"`    // 秒，默认 30

	// ── 传输 ──
	Stream  *bool  `json:"stream,omitempty"`   // 默认 true
	APIMode string `json:"api_mode,omitempty"` // chat_completions | responses (仅 NativeOAI)

	// ── NativeClaudeSession 专属 ──
	FakeCCSystemPrompt *bool  `json:"fake_cc_system_prompt,omitempty"` // CC 透传渠道必须 true
	UserAgent          string `json:"user_agent,omitempty"`            // 可选 UA 覆盖

	// ── 其他 ──
	Proxy string `json:"proxy,omitempty"` // HTTP 代理
}

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
	Config    LLMProviderConfig // GA Core 配置（存储为 JSONB）
	IsDefault bool
	State     string // active | disabled
	CreatedAt time.Time
	UpdatedAt time.Time
}

// IsActive returns true for non-disabled providers.
func (p LLMProvider) IsActive() bool { return p.State != "disabled" }

// ConfigJSON returns the config as JSON bytes for storage.
func (p LLMProvider) ConfigJSON() ([]byte, error) {
	return json.Marshal(p.Config)
}
