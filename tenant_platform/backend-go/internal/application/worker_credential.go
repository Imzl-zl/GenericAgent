package application

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/domain"
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
	// Map database provider_type to Worker-expected type for JWT token
	workerProviderType := providerTypeForWorker(provider.ProviderType)
	token, claims, err := s.cfg.TokenIssuer.Issue(sessionKey, s.cfg.ModelPolicyVersion, workerProviderType, provider.Model)
	if err != nil {
		return "", fmt.Errorf("issue capability_token: %w", err)
	}
	if s.cfg.LLMProxyAddr != "" && s.cfg.ConfigRoot != "" {
		configDir := s.configDirFor(sessionKey)
		if err := writeProviderMyKey(configDir, s.cfg.LLMProxyAddr, token, provider); err != nil {
			return "", fmt.Errorf("write token-only mykey.py: %w", err)
		}
	}
	return claims.Jti, nil
}

// writeProviderMyKey writes a mykey.py containing ONLY the capability_token
// and Proxy URL — no real upstream key. The variable name and apibase are
// chosen so GA Core instantiates the correct session class for the provider
// type. The platform ALWAYS overwrites this file; a user-provided mykey.py is
// ignored and replaced (security red line: never trust a tenant-provided key).
func writeProviderMyKey(configRoot, proxyAddr, token string, provider domain.LLMProvider) error {
	if err := os.MkdirAll(configRoot, 0o755); err != nil {
		return err
	}
	path := filepath.Join(configRoot, "mykey.py")
	content := buildMyKeyContent(proxyAddr, token, provider)
	return os.WriteFile(path, []byte(content), 0o600)
}

// defaultReadTimeoutSecs 是 mykey.py read_timeout 的默认值。
// 必须 < llmproxy.defaultUpstreamTimeout(150s)，否则 GA 还在等响应时 Proxy 先超时，
// 浪费上游配额。120s = 150 - 30s 网络/TLS buffer。
const defaultReadTimeoutSecs = 120

// buildMyKeyContent 生成 token-only mykey.py 内容。apikey 是 capability_token，
// apibase 是 LLM Proxy 地址——real key 永远不离开 Proxy（安全红线）。
// 变量名含 'native'+'claude'/'oai'，GA 据此选择 session 类（agentmain.py）。
// LLM Proxy 非流式转发（一次性 read/write），强制 stream=False；
// 其余字段从 provider.Config 完整写出，对齐 mykey_template.py。
func buildMyKeyContent(proxyAddr, token string, provider domain.LLMProvider) string {
	base := strings.TrimRight(proxyAddr, "/")
	varName, apibase := platformSessionVar(provider.ProviderType, base)
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("%s = {\n", varName))
	writeAuthFields(&sb, token, apibase, provider.Model)
	sb.WriteString("    'stream': False,\n")
	writeConfigFields(&sb, provider.SessionConfig)
	sb.WriteString("}\n")
	return sb.String()
}

// platformSessionVar 返回 mykey.py 变量名和 apibase。GA 按变量名关键字选 session 类：
//
//	'native'+'claude' → NativeClaudeSession（apibase 不带 /v1，GA 自动补 /v1/messages?beta=true）
//	'native'+'oai'    → NativeOAISession（apibase 带 /v1，GA 自动补 /chat/completions）
func platformSessionVar(t domain.LLMProviderType, proxyBase string) (varName, apibase string) {
	switch t {
	case domain.ProviderNativeClaude:
		return "platform_native_claude_config", proxyBase
	default:
		return "platform_native_oai_config", proxyBase + "/v1"
	}
}

func writeAuthFields(sb *strings.Builder, token, apibase, model string) {
	sb.WriteString("    'name': 'platform-default',\n")
	sb.WriteString(fmt.Sprintf("    'apikey': %q,\n", token))
	sb.WriteString(fmt.Sprintf("    'apibase': %q,\n", apibase))
	sb.WriteString(fmt.Sprintf("    'model': %q,\n", model))
}

// writeConfigFields writes only GA-owned session behavior. Transport fields
// stay in the Proxy and are intentionally absent from Worker configuration.
func writeConfigFields(sb *strings.Builder, cfg domain.GASessionConfig) {
	writeStringPtrField(sb, "thinking_type", cfg.ThinkingType)
	writeIntPtrField(sb, "thinking_budget_tokens", cfg.ThinkingBudgetTokens)
	writeStringPtrField(sb, "reasoning_effort", cfg.ReasoningEffort)
	writeFloatPtrField(sb, "temperature", cfg.Temperature)
	writeIntPtrField(sb, "max_tokens", cfg.MaxTokens)
	writeIntPtrField(sb, "context_win", cfg.ContextWin)
	writeIntPtrField(sb, "trim_keep_prefix", cfg.TrimKeepPrefix)
	writeIntPtrField(sb, "max_retries", cfg.MaxRetries)
	writeReadTimeoutField(sb, cfg.ReadTimeout)
	writeStringPtrField(sb, "api_mode", cfg.APIMode)
	writeBoolPtrField(sb, "fake_cc_system_prompt", cfg.FakeCCSystemPrompt)
	writeStringPtrField(sb, "user_agent", cfg.UserAgent)
	writeStringPtrField(sb, "service_tier", cfg.ServiceTier)
	writeBoolPtrField(sb, "omit_thinking", cfg.OmitThinking)
	writeStringPtrField(sb, "extra_sys_prompt", cfg.ExtraSysPrompt)
}

func writeStringPtrField(sb *strings.Builder, key string, value *string) {
	if value != nil {
		sb.WriteString(fmt.Sprintf("    '%s': %q,\n", key, *value))
	}
}

func writeIntPtrField(sb *strings.Builder, key string, value *int) {
	if value != nil {
		sb.WriteString(fmt.Sprintf("    '%s': %d,\n", key, *value))
	}
}

func writeFloatPtrField(sb *strings.Builder, key string, value *float64) {
	if value != nil {
		sb.WriteString(fmt.Sprintf("    '%s': %g,\n", key, *value))
	}
}

func writeBoolPtrField(sb *strings.Builder, key string, value *bool) {
	if value == nil {
		return
	}
	pythonValue := "False"
	if *value {
		pythonValue = "True"
	}
	sb.WriteString(fmt.Sprintf("    '%s': %s,\n", key, pythonValue))
}

func writeReadTimeoutField(sb *strings.Builder, value *int) {
	readTimeout := defaultReadTimeoutSecs
	if value != nil {
		readTimeout = *value
	}
	sb.WriteString(fmt.Sprintf("    'read_timeout': %d,\n", readTimeout))
}

// providerTypeForWorker maps database provider_type to Worker-expected type.
// Database uses native_oai/native_claude, but Worker expects openai_compatible/anthropic_messages.
func providerTypeForWorker(dbType domain.LLMProviderType) string {
	switch dbType {
	case domain.ProviderNativeOAI:
		return "openai_compatible"
	case domain.ProviderNativeClaude:
		return "anthropic_messages"
	default:
		// Fallback: use database type as-is (for legacy types)
		return string(dbType)
	}
}

// writeTokenOnlyMyKey is kept for tests that do not yet inject a provider.
// Deprecated: use writeProviderMyKey for production code.
func writeTokenOnlyMyKey(configRoot, proxyAddr, token string) error {
	return writeProviderMyKey(configRoot, proxyAddr, token, domain.LLMProvider{
		ProviderType: domain.ProviderNativeOAI,
		Model:        "gpt-test",
	})
}
