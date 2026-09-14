package domain

import (
	"encoding/json"
	"testing"
)

func TestGASessionConfigPreservesExplicitZero(t *testing.T) {
	zeroInt := 0
	zeroFloat := 0.0
	input := GASessionConfig{
		MaxRetries:  &zeroInt,
		Temperature: &zeroFloat,
	}

	raw, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}

	var got GASessionConfig
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if got.MaxRetries == nil || *got.MaxRetries != 0 {
		t.Fatalf("max_retries = %v, want explicit zero", got.MaxRetries)
	}
	if got.Temperature == nil || *got.Temperature != 0 {
		t.Fatalf("temperature = %v, want explicit zero", got.Temperature)
	}
}

func TestGASessionConfigRejectsResponsesForClaude(t *testing.T) {
	mode := "responses"
	config := GASessionConfig{APIMode: &mode}

	if err := config.Validate(ProviderNativeClaude); err == nil {
		t.Fatal("expected responses mode to be rejected for native_claude")
	}
}

func TestGASessionConfigRequiresThinkingBudgetWhenEnabled(t *testing.T) {
	thinkingType := "enabled"
	config := GASessionConfig{ThinkingType: &thinkingType}

	if err := config.Validate(ProviderNativeClaude); err == nil {
		t.Fatal("expected enabled thinking without a budget to fail")
	}
}

func TestProviderTransportConfigRejectsNonPositiveTimeout(t *testing.T) {
	zero := 0
	config := ProviderTransportConfig{
		AuthMode:              ProviderAuthAuto,
		ConnectTimeoutSeconds: &zero,
	}

	if err := config.Validate(); err == nil {
		t.Fatal("expected zero connect timeout to fail")
	}
}

func TestProviderCapabilityAliasAndNormalization(t *testing.T) {
	// image 是 image.generate 的别名(0058 以来存量行/既有客户端都写 image),
	// 归一化后两个方向都必须成立, 否则存量 provider 会在细分后被误判为"无生图能力"。
	legacy := LLMProvider{Capabilities: []ProviderCapability{ProviderCapabilityImage}}
	if !legacy.HasCapability(ProviderCapabilityImageGenerate) {
		t.Fatalf("legacy image provider must satisfy image.generate")
	}
	explicit := LLMProvider{Capabilities: []ProviderCapability{ProviderCapabilityImageGenerate}}
	if !explicit.HasCapability(ProviderCapabilityImage) {
		t.Fatalf("explicit image.generate provider must satisfy the image alias")
	}
	if IsImageProviderCapability(ProviderCapabilityChat) {
		t.Fatalf("chat is not an image capability")
	}
	if !IsImageProviderCapability(ProviderCapabilityImage) || !IsImageProviderCapability(ProviderCapabilityImageEdit) {
		t.Fatalf("image and image.edit are image capabilities")
	}
}

func TestEffectiveCapabilitiesDedupesAlias(t *testing.T) {
	p := LLMProvider{Capabilities: []ProviderCapability{
		ProviderCapabilityImage, ProviderCapabilityImageGenerate, ProviderCapabilityImageEdit,
	}}
	got := p.EffectiveCapabilities()
	if len(got) != 2 {
		t.Fatalf("alias must be folded, got %v", got)
	}
	if got[0] != ProviderCapabilityImageGenerate || got[1] != ProviderCapabilityImageEdit {
		t.Fatalf("unexpected normalized order: %v", got)
	}
}

func TestImageCapabilityOperations(t *testing.T) {
	cases := map[ProviderCapability][]string{
		ProviderCapabilityChat:          nil,
		ProviderCapabilityImage:         {"generate"},
		ProviderCapabilityImageGenerate: {"generate"},
		ProviderCapabilityImageEdit:     {"edit"},
	}
	for cap, want := range cases {
		got := ImageCapabilityOperations(cap)
		if len(got) != len(want) || (len(want) == 1 && got[0] != want[0]) {
			t.Fatalf("ImageCapabilityOperations(%q) = %v, want %v", cap, got, want)
		}
	}
}

func TestValidProviderCapabilityRejectsUnknown(t *testing.T) {
	for _, bad := range []ProviderCapability{"", "images", "image.generate.edit", "chat.image"} {
		if ValidProviderCapability(bad) {
			t.Fatalf("%q must be rejected", bad)
		}
	}
	for _, ok := range []ProviderCapability{ProviderCapabilityChat, ProviderCapabilityImage,
		ProviderCapabilityImageGenerate, ProviderCapabilityImageEdit} {
		if !ValidProviderCapability(ok) {
			t.Fatalf("%q must be accepted", ok)
		}
	}
}
