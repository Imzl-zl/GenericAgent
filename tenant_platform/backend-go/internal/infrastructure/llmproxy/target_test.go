package llmproxy

import (
	"net/url"
	"testing"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/domain"
)

func TestResolveUpstreamTargetMatchesGANativeURLRules(t *testing.T) {
	tests := []struct {
		name         string
		providerType domain.LLMProviderType
		base         string
		inbound      string
		want         string
	}{
		{
			name: "oai base without version", providerType: domain.ProviderNativeOAI,
			base: "https://api.example.com", inbound: "/v1/chat/completions",
			want: "https://api.example.com/v1/chat/completions",
		},
		{
			name: "oai base already has v1", providerType: domain.ProviderNativeOAI,
			base: "https://api.openai.com/v1", inbound: "/v1/chat/completions",
			want: "https://api.openai.com/v1/chat/completions",
		},
		{
			name: "oai base is complete endpoint", providerType: domain.ProviderNativeOAI,
			base: "https://relay.example/openai/v1/responses", inbound: "/v1/responses",
			want: "https://relay.example/openai/v1/responses",
		},
		{
			name: "claude exact endpoint", providerType: domain.ProviderNativeClaude,
			base: "https://relay.example/custom/messages$", inbound: "/v1/messages?beta=true",
			want: "https://relay.example/custom/messages?beta=true",
		},
		{
			name: "exact endpoint with trailing slash", providerType: domain.ProviderNativeClaude,
			base: "https://relay.example/custom/messages$/", inbound: "/v1/messages",
			want: "https://relay.example/custom/messages",
		},
		{
			name: "merge non-conflicting query", providerType: domain.ProviderNativeClaude,
			base: "https://relay.example/v1?region=us", inbound: "/v1/messages?beta=true",
			want: "https://relay.example/v1/messages?beta=true&region=us",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			inbound, err := url.Parse(test.inbound)
			if err != nil {
				t.Fatal(err)
			}
			provider := testProvider(test.providerType, test.base, "model", "key")
			got, err := ResolveUpstreamTarget(provider, inbound)
			if err != nil {
				t.Fatal(err)
			}
			if got.String() != test.want {
				t.Fatalf("target=%q want=%q", got.String(), test.want)
			}
		})
	}
}

func TestResolveUpstreamTargetRejectsProtocolAndQueryConflicts(t *testing.T) {
	tests := []struct {
		name         string
		providerType domain.LLMProviderType
		base         string
		inbound      string
	}{
		{
			name: "OAI cannot route messages", providerType: domain.ProviderNativeOAI,
			base: "https://api.example.com", inbound: "/v1/messages",
		},
		{
			name: "Claude cannot route responses", providerType: domain.ProviderNativeClaude,
			base: "https://api.example.com", inbound: "/v1/responses",
		},
		{
			name: "conflicting query", providerType: domain.ProviderNativeClaude,
			base: "https://api.example.com/v1?beta=false", inbound: "/v1/messages?beta=true",
		},
		{
			name: "unsupported scheme", providerType: domain.ProviderNativeOAI,
			base: "file:///etc/passwd", inbound: "/v1/chat/completions",
		},
		{
			name: "base credentials", providerType: domain.ProviderNativeOAI,
			base: "https://user:pass@api.example.com", inbound: "/v1/chat/completions",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			inbound, err := url.Parse(test.inbound)
			if err != nil {
				t.Fatal(err)
			}
			provider := testProvider(test.providerType, test.base, "model", "key")
			if target, err := ResolveUpstreamTarget(provider, inbound); err == nil || target != nil {
				t.Fatalf("target=%v err=%v", target, err)
			}
		})
	}
}

// TestResolveUpstreamTargetImageGenerations: 生图路由映射(Phase B 托管形态)。
func TestResolveUpstreamTargetImageGenerations(t *testing.T) {
	tests := []struct {
		name    string
		base    string
		inbound string
		want    string
	}{
		{
			name: "image via base without version",
			base: "https://api.example.com", inbound: "/v1/images/generations",
			want: "https://api.example.com/v1/images/generations",
		},
		{
			name: "image via base with v1",
			base: "https://newapi.example.com/v1", inbound: "/v1/images/generations",
			want: "https://newapi.example.com/v1/images/generations",
		},
		{
			name: "image alias path no v1",
			base: "https://api.example.com/v1", inbound: "/images/generations",
			want: "https://api.example.com/v1/images/generations",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			provider := domain.LLMProvider{ProviderType: domain.ProviderNativeOAI, BaseURL: tt.base, Model: "gpt-image-2"}
			inbound, err := url.Parse(tt.inbound)
			if err != nil {
				t.Fatal(err)
			}
			got, err := ResolveUpstreamTarget(provider, inbound)
			if err != nil {
				t.Fatalf("ResolveUpstreamTarget: %v", err)
			}
			if got.String() != tt.want {
				t.Fatalf("target = %q, want %q", got.String(), tt.want)
			}
		})
	}
}

// TestResolveUpstreamTargetImageRejectedOnClaudeProvider: native_claude
// provider 不接受生图路径(生图走 OAI 协议)。
func TestResolveUpstreamTargetImageRejectedOnClaudeProvider(t *testing.T) {
	provider := domain.LLMProvider{ProviderType: domain.ProviderNativeClaude, BaseURL: "https://api.anthropic.com"}
	inbound, _ := url.Parse("/v1/images/generations")
	if _, err := ResolveUpstreamTarget(provider, inbound); err == nil {
		t.Fatal("expected error for image path on claude provider")
	}
}
