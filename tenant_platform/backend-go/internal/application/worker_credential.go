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
	token, claims, err := s.cfg.TokenIssuer.Issue(sessionKey, s.cfg.ModelPolicyVersion, string(provider.ProviderType), provider.Model)
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

func buildMyKeyContent(proxyAddr, token string, provider domain.LLMProvider) string {
	base := strings.TrimRight(proxyAddr, "/")
	switch provider.ProviderType {
	case domain.ProviderAnthropicMessages:
		// NativeClaudeSession appends /v1/messages and ?beta=true.
		return fmt.Sprintf(
			"platform_native_claude_config = {\n"+
				"    'name': 'platform-default',\n"+
				"    'apikey': %q,\n"+
				"    'apibase': %q,\n"+
				"    'model': %q,\n"+
				"    'stream': False,\n"+
				"    'read_timeout': 120,\n"+
				"}\n",
			token, base, provider.Model,
		)
	default:
		// OpenAI-compatible: apibase ends in /v1 so /chat/completions is appended.
		return fmt.Sprintf(
			"platform_native_oai_config = {\n"+
				"    'name': 'platform-default',\n"+
				"    'apikey': %q,\n"+
				"    'apibase': %q,\n"+
				"    'model': %q,\n"+
				"    'api_mode': 'chat_completions',\n"+
				"    'stream': False,\n"+
				"    'read_timeout': 120,\n"+
				"}\n",
			token, base+"/v1", provider.Model,
		)
	}
}

// writeTokenOnlyMyKey is kept for tests that do not yet inject a provider.
// Deprecated: use writeProviderMyKey for production code.
func writeTokenOnlyMyKey(configRoot, proxyAddr, token string) error {
	return writeProviderMyKey(configRoot, proxyAddr, token, domain.LLMProvider{
		ProviderType: domain.ProviderOpenAICompatible,
		Model:        "gpt-test",
	})
}
