package domain

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"
)

const MaxExtraSystemPromptBytes = 64 * 1024

// ErrProviderNotFound 目标 LLM provider 不存在(业务拒绝 → 404,
// 见 docs/FAILURE_POLICY.zh-CN.md 错误域分类)。
var ErrProviderNotFound = errors.New("LLM provider not found")

// ErrProviderStateConflict 当前状态不允许该操作(如禁用默认 provider)——409。
var ErrProviderStateConflict = errors.New("provider state does not allow this operation")

// LLMProviderType selects the native GA Session implementation.
type LLMProviderType string

const (
	ProviderNativeOAI    LLMProviderType = "native_oai"
	ProviderNativeClaude LLMProviderType = "native_claude"
)

// ProviderCapability is a capability dimension served by an LLMProvider
// (Phase B 托管形态, 2026-08-14 定稿): chat = 对话(operation llm.chat),
// image = 生图(operation llm.image)。一个 provider 可同时具备两种能力,
// 但 model 单值意味着 image 能力 provider 的 model 是生图模型——实际部署
// 中 image provider 通常是独立 provider。
type ProviderCapability string

const (
	ProviderCapabilityChat  ProviderCapability = "chat"
	ProviderCapabilityImage ProviderCapability = "image"
)

func ValidProviderCapability(c ProviderCapability) bool {
	return c == ProviderCapabilityChat || c == ProviderCapabilityImage
}

type ProviderAuthMode string

const (
	ProviderAuthAuto    ProviderAuthMode = "auto"
	ProviderAuthBearer  ProviderAuthMode = "bearer"
	ProviderAuthXAPIKey ProviderAuthMode = "x_api_key"
)

// GASessionConfig contains only behavior consumed by GA Core.
type GASessionConfig struct {
	ThinkingType         *string  `json:"thinking_type,omitempty"`
	ThinkingBudgetTokens *int     `json:"thinking_budget_tokens,omitempty"`
	ReasoningEffort      *string  `json:"reasoning_effort,omitempty"`
	Temperature          *float64 `json:"temperature,omitempty"`
	MaxTokens            *int     `json:"max_tokens,omitempty"`
	ContextWin           *int     `json:"context_win,omitempty"`
	TrimKeepPrefix       *int     `json:"trim_keep_prefix,omitempty"`
	MaxRetries           *int     `json:"max_retries,omitempty"`
	ReadTimeout          *int     `json:"read_timeout,omitempty"`
	Stream               *bool    `json:"stream,omitempty"`
	APIMode              *string  `json:"api_mode,omitempty"`
	FakeCCSystemPrompt   *bool    `json:"fake_cc_system_prompt,omitempty"`
	UserAgent            *string  `json:"user_agent,omitempty"`
	ServiceTier          *string  `json:"service_tier,omitempty"`
	OmitThinking         *bool    `json:"omit_thinking,omitempty"`
	ExtraSysPrompt       *string  `json:"extra_sys_prompt,omitempty"`
}

func (c GASessionConfig) Validate(providerType LLMProviderType) error {
	if providerType != ProviderNativeOAI && providerType != ProviderNativeClaude {
		return fmt.Errorf("unsupported provider type %q", providerType)
	}
	if err := validateOptionalEnum("thinking_type", c.ThinkingType, "adaptive", "enabled", "disabled"); err != nil {
		return err
	}
	if err := validateOptionalEnum("reasoning_effort", c.ReasoningEffort, "none", "minimal", "low", "medium", "high", "xhigh", "max"); err != nil {
		return err
	}
	if c.ThinkingBudgetTokens != nil && *c.ThinkingBudgetTokens <= 0 {
		return fmt.Errorf("thinking_budget_tokens must be positive")
	}
	if c.ThinkingType != nil && *c.ThinkingType == "enabled" && c.ThinkingBudgetTokens == nil {
		return fmt.Errorf("thinking_budget_tokens is required when thinking_type is enabled")
	}
	if c.Temperature != nil && (*c.Temperature < 0 || *c.Temperature > 2) {
		return fmt.Errorf("temperature must be between 0 and 2")
	}
	if err := validatePositive("max_tokens", c.MaxTokens); err != nil {
		return err
	}
	if err := validatePositive("context_win", c.ContextWin); err != nil {
		return err
	}
	if c.TrimKeepPrefix != nil && *c.TrimKeepPrefix < 0 {
		return fmt.Errorf("trim_keep_prefix must be non-negative")
	}
	if c.MaxRetries != nil && *c.MaxRetries < 0 {
		return fmt.Errorf("max_retries must be non-negative")
	}
	if c.ReadTimeout != nil && *c.ReadTimeout < 5 {
		return fmt.Errorf("read_timeout must be at least 5 seconds")
	}
	if providerType == ProviderNativeClaude {
		if c.APIMode != nil {
			return fmt.Errorf("api_mode is only supported by native_oai")
		}
		if c.ServiceTier != nil {
			return fmt.Errorf("service_tier is only supported by native_oai")
		}
	} else if c.FakeCCSystemPrompt != nil {
		return fmt.Errorf("fake_cc_system_prompt is only supported by native_claude")
	}
	if err := validateOptionalEnum("api_mode", c.APIMode, "chat_completions", "responses"); err != nil {
		return err
	}
	if err := validateOptionalEnum("service_tier", c.ServiceTier, "auto", "default", "priority", "flex"); err != nil {
		return err
	}
	if c.UserAgent != nil && strings.TrimSpace(*c.UserAgent) == "" {
		return fmt.Errorf("user_agent must not be empty when provided")
	}
	if c.ExtraSysPrompt != nil && len([]byte(*c.ExtraSysPrompt)) > MaxExtraSystemPromptBytes {
		return fmt.Errorf("extra_sys_prompt exceeds %d bytes", MaxExtraSystemPromptBytes)
	}
	return nil
}

// ProviderTransportConfig contains only Proxy-to-upstream transport behavior.
type ProviderTransportConfig struct {
	AuthMode                     ProviderAuthMode `json:"auth_mode"`
	ProxyURL                     *string          `json:"proxy_url,omitempty"`
	TLSVerify                    *bool            `json:"tls_verify,omitempty"`
	ConnectTimeoutSeconds        *int             `json:"connect_timeout_seconds,omitempty"`
	ResponseHeaderTimeoutSeconds *int             `json:"response_header_timeout_seconds,omitempty"`
}

func (c ProviderTransportConfig) Validate() error {
	switch c.AuthMode {
	case "", ProviderAuthAuto, ProviderAuthBearer, ProviderAuthXAPIKey:
	default:
		return fmt.Errorf("unsupported auth_mode %q", c.AuthMode)
	}
	if c.ProxyURL != nil {
		parsed, err := url.Parse(*c.ProxyURL)
		if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
			return fmt.Errorf("proxy_url must be an absolute http or https URL")
		}
		if parsed.User != nil || parsed.Fragment != "" || parsed.Opaque != "" {
			return fmt.Errorf("proxy_url must contain no credentials or fragment")
		}
		if (parsed.Path != "" && parsed.Path != "/") || parsed.RawQuery != "" {
			return fmt.Errorf("proxy_url must contain no path or query")
		}
	}
	if err := validatePositive("connect_timeout_seconds", c.ConnectTimeoutSeconds); err != nil {
		return err
	}
	if err := validatePositive("response_header_timeout_seconds", c.ResponseHeaderTimeoutSeconds); err != nil {
		return err
	}
	return nil
}

func (c ProviderTransportConfig) EffectiveAuthMode() ProviderAuthMode {
	if c.AuthMode == "" {
		return ProviderAuthAuto
	}
	return c.AuthMode
}

type LLMProviderState string

const (
	ProviderActive   LLMProviderState = "active"
	ProviderDisabled LLMProviderState = "disabled"
)

func (s LLMProviderState) Valid() bool {
	return s == ProviderActive || s == ProviderDisabled
}

type LLMProviderCreate struct {
	Name             string
	ProviderType     LLMProviderType
	BaseURL          string
	Model            string
	APIKeyCiphertext []byte
	APIKeyKeyVersion string
	SessionConfig    GASessionConfig
	TransportConfig  ProviderTransportConfig
	// Capabilities 能力维度(chat/image); 省略 = [chat](兼容既有 provider)。
	Capabilities []ProviderCapability
}

type LLMProviderUpdate struct {
	LLMProviderCreate
	RotateAPIKey bool
}

// LLMProvider is an administrator-configured upstream LLM.
type LLMProvider struct {
	ID               int64
	Name             string
	ProviderType     LLMProviderType
	BaseURL          string
	Model            string
	APIKeyCiphertext []byte
	APIKeyKeyVersion string
	// APIKey is populated only inside the LLM Proxy and must never be persisted.
	APIKey          string
	SessionConfig   GASessionConfig
	TransportConfig ProviderTransportConfig
	Capabilities    []ProviderCapability
	Revision        int64
	IsDefault       bool
	State           LLMProviderState
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

// EffectiveCapabilities 返回归一化能力列表: 省略 = [chat](存量兼容)。
func (p LLMProvider) EffectiveCapabilities() []ProviderCapability {
	if len(p.Capabilities) == 0 {
		return []ProviderCapability{ProviderCapabilityChat}
	}
	return p.Capabilities
}

// HasCapability 判断 provider 是否具备某能力维度。
func (p LLMProvider) HasCapability(c ProviderCapability) bool {
	for _, have := range p.EffectiveCapabilities() {
		if have == c {
			return true
		}
	}
	return false
}

func (p LLMProvider) IsActive() bool { return p.State == ProviderActive }

func validateOptionalEnum(field string, value *string, allowed ...string) error {
	if value == nil {
		return nil
	}
	for _, candidate := range allowed {
		if *value == candidate {
			return nil
		}
	}
	return fmt.Errorf("%s has unsupported value %q", field, *value)
}

func validatePositive(field string, value *int) error {
	if value != nil && *value <= 0 {
		return fmt.Errorf("%s must be positive", field)
	}
	return nil
}
