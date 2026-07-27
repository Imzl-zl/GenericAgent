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
