package llmproxy

import (
	"net/http"
	"testing"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/domain"
)

func TestHeadersAllowGANativeMetadataAndReplaceCredentials(t *testing.T) {
	inbound := http.Header{
		"Authorization":               []string{"Bearer capability-token"},
		"X-Api-Key":                   []string{"worker-supplied-key"},
		"Cookie":                      []string{"session=secret"},
		"Forwarded":                   []string{"for=127.0.0.1"},
		"X-Forwarded-For":             []string{"127.0.0.1"},
		"Content-Type":                []string{"application/json"},
		"Accept":                      []string{"text/event-stream"},
		"User-Agent":                  []string{"claude-cli/2.1.152"},
		"Anthropic-Version":           []string{"2023-06-01"},
		"Anthropic-Beta":              []string{"prompt-caching-2024-07-31", "context-1m-2025-08-07"},
		"X-Claude-Code-Session-Id":    []string{"session-id"},
		"X-Stainless-Runtime":         []string{"node"},
		"X-Stainless-Timeout":         []string{"600"},
		"Originator":                  []string{"codex_exec"},
		"X-Unrecognized":              []string{"must-not-pass"},
		"OpenAI-Beta":                 []string{"assistants=v2"},
		"X-Stainless-Custom-Metadata": []string{"preserve"},
	}
	out := http.Header{
		"Authorization": []string{"Bearer stale"},
		"X-Existing":    []string{"must-be-cleared"},
	}
	provider := testProvider(domain.ProviderNativeOAI, "https://api.example.com", "model", "")
	SanitizeAndInjectHeaders(out, inbound, provider, "real-upstream-key")

	if got := out.Get("Authorization"); got != "Bearer real-upstream-key" {
		t.Fatalf("authorization=%q", got)
	}
	for _, name := range []string{"X-Api-Key", "Cookie", "Forwarded", "X-Forwarded-For", "X-Unrecognized", "X-Existing"} {
		if values := out.Values(name); len(values) != 0 {
			t.Fatalf("%s leaked: %v", name, values)
		}
	}
	for name, want := range map[string]string{
		"Content-Type": "application/json", "Accept": "text/event-stream",
		"User-Agent": "claude-cli/2.1.152", "Anthropic-Version": "2023-06-01",
		"X-Claude-Code-Session-Id": "session-id", "X-Stainless-Runtime": "node",
		"X-Stainless-Timeout": "600", "Originator": "codex_exec",
		"OpenAI-Beta": "assistants=v2", "X-Stainless-Custom-Metadata": "preserve",
	} {
		if got := out.Get(name); got != want {
			t.Fatalf("%s=%q want=%q", name, got, want)
		}
	}
	if got := out.Values("Anthropic-Beta"); len(got) != 2 {
		t.Fatalf("Anthropic-Beta=%v", got)
	}
}

func TestHeadersInjectClaudeAuthModes(t *testing.T) {
	tests := []struct {
		name       string
		mode       domain.ProviderAuthMode
		key        string
		wantHeader string
		wantValue  string
	}{
		{name: "auto Anthropic key", mode: domain.ProviderAuthAuto, key: "sk-ant-real", wantHeader: "X-Api-Key", wantValue: "sk-ant-real"},
		{name: "auto OAuth token", mode: domain.ProviderAuthAuto, key: "oauth-real", wantHeader: "Authorization", wantValue: "Bearer oauth-real"},
		{name: "explicit bearer", mode: domain.ProviderAuthBearer, key: "sk-ant-real", wantHeader: "Authorization", wantValue: "Bearer sk-ant-real"},
		{name: "explicit x-api-key", mode: domain.ProviderAuthXAPIKey, key: "oauth-real", wantHeader: "X-Api-Key", wantValue: "oauth-real"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			provider := testProvider(domain.ProviderNativeClaude, "https://api.anthropic.com", "model", "")
			provider.TransportConfig.AuthMode = test.mode
			out := make(http.Header)
			SanitizeAndInjectHeaders(out, http.Header{
				"Authorization": []string{"Bearer capability"},
				"X-Api-Key":     []string{"worker-key"},
			}, provider, test.key)
			if got := out.Get(test.wantHeader); got != test.wantValue {
				t.Fatalf("%s=%q want=%q", test.wantHeader, got, test.wantValue)
			}
			other := "Authorization"
			if test.wantHeader == other {
				other = "X-Api-Key"
			}
			if got := out.Get(other); got != "" {
				t.Fatalf("unexpected %s=%q", other, got)
			}
		})
	}
}

func TestHeadersRemoveConnectionDeclaredHopByHopValues(t *testing.T) {
	provider := testProvider(domain.ProviderNativeOAI, "https://api.example.com", "model", "")
	out := make(http.Header)
	SanitizeAndInjectHeaders(out, http.Header{
		"Connection":          []string{"User-Agent, X-Stainless-Runtime"},
		"User-Agent":          []string{"must-not-pass"},
		"X-Stainless-Runtime": []string{"must-not-pass"},
		"Content-Type":        []string{"application/json"},
	}, provider, "real-key")
	if out.Get("User-Agent") != "" || out.Get("X-Stainless-Runtime") != "" {
		t.Fatalf("Connection-declared headers leaked: %v", out)
	}
	if out.Get("Content-Type") != "application/json" {
		t.Fatalf("Content-Type=%q", out.Get("Content-Type"))
	}
}

func TestHeadersDoNotInjectEmptyCredential(t *testing.T) {
	provider := testProvider(domain.ProviderNativeOAI, "https://api.example.com", "model", "")
	out := http.Header{"Authorization": []string{"Bearer stale"}}
	SanitizeAndInjectHeaders(out, http.Header{"Authorization": []string{"Bearer capability"}}, provider, "")
	if out.Get("Authorization") != "" || out.Get("X-Api-Key") != "" {
		t.Fatalf("empty credential injected: %v", out)
	}
}
