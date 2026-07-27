package llmproxy

import (
	"net/http"
	"strings"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/domain"
)

var nativeRequestHeaderAllowlist = map[string]struct{}{
	"Accept-Encoding": {},
	"Accept":          {},
	"Anthropic-Beta":  {},
	"Anthropic-Dangerous-Direct-Browser-Access": {},
	"Anthropic-Version":                         {},
	"Content-Type":                              {},
	"Originator":                                {},
	"Openai-Beta":                               {},
	"User-Agent":                                {},
	"X-App":                                     {},
	"X-Claude-Code-Session-Id":                  {},
	"X-Stainless-Arch":                          {},
	"X-Stainless-Lang":                          {},
	"X-Stainless-Os":                            {},
	"X-Stainless-Package-Version":               {},
	"X-Stainless-Retry-Count":                   {},
	"X-Stainless-Runtime":                       {},
	"X-Stainless-Runtime-Version":               {},
	"X-Stainless-Timeout":                       {},
}

// SanitizeAndInjectHeaders rebuilds the upstream request headers from a
// positive allowlist, then injects the real Provider credential last.
func SanitizeAndInjectHeaders(
	out http.Header,
	inbound http.Header,
	provider domain.LLMProvider,
	realKey string,
) {
	if out == nil {
		return
	}
	clear(out)
	declaredHopByHop := connectionDeclaredHeaders(inbound)
	for name, values := range inbound {
		canonical := http.CanonicalHeaderKey(name)
		if _, removed := declaredHopByHop[canonical]; removed || !nativeRequestHeaderAllowed(canonical) {
			continue
		}
		for _, value := range values {
			out.Add(canonical, value)
		}
	}
	if _, present := out["User-Agent"]; !present {
		out["User-Agent"] = []string{""}
	}
	injectProviderCredential(out, provider, realKey)
}

func nativeRequestHeaderAllowed(canonical string) bool {
	if _, allowed := nativeRequestHeaderAllowlist[canonical]; allowed {
		return true
	}
	return strings.HasPrefix(canonical, "X-Stainless-") && len(canonical) > len("X-Stainless-")
}

func connectionDeclaredHeaders(headers http.Header) map[string]struct{} {
	declared := make(map[string]struct{})
	for name, values := range headers {
		if http.CanonicalHeaderKey(name) != "Connection" {
			continue
		}
		for _, value := range values {
			for _, token := range strings.Split(value, ",") {
				if canonical := http.CanonicalHeaderKey(strings.TrimSpace(token)); canonical != "" {
					declared[canonical] = struct{}{}
				}
			}
		}
	}
	return declared
}

func injectProviderCredential(headers http.Header, provider domain.LLMProvider, realKey string) {
	if realKey == "" {
		return
	}
	mode := provider.TransportConfig.EffectiveAuthMode()
	if mode == domain.ProviderAuthAuto {
		if provider.ProviderType == domain.ProviderNativeClaude && strings.HasPrefix(realKey, "sk-ant-") {
			mode = domain.ProviderAuthXAPIKey
		} else {
			mode = domain.ProviderAuthBearer
		}
	}
	switch mode {
	case domain.ProviderAuthBearer:
		headers.Set("Authorization", "Bearer "+realKey)
	case domain.ProviderAuthXAPIKey:
		headers.Set("X-Api-Key", realKey)
	}
}
