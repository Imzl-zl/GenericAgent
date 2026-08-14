package llmproxy

import (
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/domain"
)

var versionedAPIPath = regexp.MustCompile(`/v\d+(?:/|$)`)

// ResolveUpstreamTarget mirrors GA auto_make_url while enforcing the native
// protocol path selected by the capability-bound Provider.
func ResolveUpstreamTarget(provider domain.LLMProvider, inboundURL *url.URL) (*url.URL, error) {
	if inboundURL == nil {
		return nil, errors.New("inbound URL is required")
	}
	endpoint, err := nativeEndpoint(provider.ProviderType, inboundURL.Path)
	if err != nil {
		return nil, err
	}
	base, err := url.Parse(strings.TrimSpace(provider.BaseURL))
	if err != nil {
		return nil, fmt.Errorf("parse provider base URL: %w", err)
	}
	if base.Scheme != "http" && base.Scheme != "https" {
		return nil, errors.New("provider base URL must use http or https")
	}
	if base.Host == "" || base.User != nil || base.Fragment != "" || base.Opaque != "" {
		return nil, errors.New("provider base URL must be absolute and contain no credentials or fragment")
	}

	escapedPath := strings.TrimRight(base.EscapedPath(), "/")
	base.Path = strings.TrimRight(base.Path, "/")
	exact := strings.HasSuffix(escapedPath, "$")
	if exact {
		base.Path = strings.TrimRight(strings.TrimSuffix(base.Path, "$"), "/")
	} else {
		base.Path = appendNativeEndpoint(base.Path, endpoint)
	}
	base.RawPath = ""
	baseQuery, err := url.ParseQuery(base.RawQuery)
	if err != nil {
		return nil, fmt.Errorf("parse provider base query: %w", err)
	}
	inboundQuery, err := url.ParseQuery(inboundURL.RawQuery)
	if err != nil {
		return nil, fmt.Errorf("parse inbound query: %w", err)
	}
	merged, err := mergeTargetQuery(baseQuery, inboundQuery)
	if err != nil {
		return nil, err
	}
	base.RawQuery = merged.Encode()
	return base, nil
}

func nativeEndpoint(providerType domain.LLMProviderType, inboundPath string) (string, error) {
	path := strings.TrimSuffix(inboundPath, "/")
	switch providerType {
	case domain.ProviderNativeOAI:
		switch path {
		case "/v1/chat/completions", "/chat/completions":
			return "chat/completions", nil
		case "/v1/responses", "/responses":
			return "responses", nil
		case "/v1/images/generations", "/images/generations":
			return "images/generations", nil
		}
	case domain.ProviderNativeClaude:
		if path == "/v1/messages" || path == "/messages" {
			return "messages", nil
		}
	default:
		return "", fmt.Errorf("unsupported provider type %q", providerType)
	}
	return "", fmt.Errorf("path %q is incompatible with provider type %q", inboundPath, providerType)
}

func appendNativeEndpoint(basePath, endpoint string) string {
	trimmed := strings.TrimRight(basePath, "/")
	if strings.HasSuffix(trimmed, endpoint) {
		return trimmed
	}
	if versionedAPIPath.MatchString(trimmed) {
		return joinURLPath(trimmed, endpoint)
	}
	return joinURLPath(joinURLPath(trimmed, "v1"), endpoint)
}

func joinURLPath(basePath, suffix string) string {
	basePath = strings.TrimRight(basePath, "/")
	suffix = strings.Trim(suffix, "/")
	if basePath == "" {
		return "/" + suffix
	}
	return basePath + "/" + suffix
}

func mergeTargetQuery(base, inbound url.Values) (url.Values, error) {
	merged := make(url.Values, len(base)+len(inbound))
	for key, values := range base {
		value, err := singleQueryValue(key, values)
		if err != nil {
			return nil, err
		}
		merged.Set(key, value)
	}
	for key, values := range inbound {
		value, err := singleQueryValue(key, values)
		if err != nil {
			return nil, err
		}
		if existing, present := merged[key]; present && existing[0] != value {
			return nil, fmt.Errorf("conflicting query value for %q", key)
		}
		merged.Set(key, value)
	}
	return merged, nil
}

func singleQueryValue(key string, values []string) (string, error) {
	if len(values) == 0 {
		return "", nil
	}
	for _, value := range values[1:] {
		if value != values[0] {
			return "", fmt.Errorf("conflicting duplicate query values for %q", key)
		}
	}
	return values[0], nil
}
