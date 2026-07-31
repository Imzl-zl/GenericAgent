package domain

import (
	"strings"
	"testing"
)

func validDocumentPoolSettings() DocumentPoolSettings {
	return DocumentPoolSettings{
		Enabled:               true,
		MaxActive:             2,
		MinReady:              1,
		JobIdleTTLSeconds:     600,
		ReadyIdleTTLSeconds:   300,
		GlobalQueueLimit:      100,
		PerTenantQueueLimit:   20,
		PerTenantActiveLimit:  1,
		JobTimeoutSeconds:     3600,
		CommandTimeoutSeconds: 300,
	}
}

func TestValidateDocumentPoolSettingsAcceptsCoherentValues(t *testing.T) {
	if err := ValidateDocumentPoolSettings(validDocumentPoolSettings(), 4); err != nil {
		t.Fatalf("valid settings rejected: %v", err)
	}
}

func TestValidateDocumentPoolSettingsRejectsInvalidRangesAndDeploymentLimit(t *testing.T) {
	tests := map[string]func(*DocumentPoolSettings){
		"enabled without active capacity": func(s *DocumentPoolSettings) { s.MaxActive = 0 },
		"deployment hard max":             func(s *DocumentPoolSettings) { s.MaxActive = 5 },
		"negative ready capacity":         func(s *DocumentPoolSettings) { s.MinReady = -1 },
		"ready exceeds active":            func(s *DocumentPoolSettings) { s.MinReady = 3 },
		"nonpositive job idle ttl":        func(s *DocumentPoolSettings) { s.JobIdleTTLSeconds = 0 },
		"nonpositive ready idle ttl":      func(s *DocumentPoolSettings) { s.ReadyIdleTTLSeconds = 0 },
		"nonpositive global queue":        func(s *DocumentPoolSettings) { s.GlobalQueueLimit = 0 },
		"tenant queue exceeds global":     func(s *DocumentPoolSettings) { s.PerTenantQueueLimit = 101 },
		"tenant active exceeds active":    func(s *DocumentPoolSettings) { s.PerTenantActiveLimit = 3 },
		"nonpositive job timeout":         func(s *DocumentPoolSettings) { s.JobTimeoutSeconds = 0 },
		"command timeout exceeds job":     func(s *DocumentPoolSettings) { s.CommandTimeoutSeconds = 3601 },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			settings := validDocumentPoolSettings()
			mutate(&settings)
			if err := ValidateDocumentPoolSettings(settings, 4); err == nil {
				t.Fatal("expected validation error")
			}
		})
	}
}

func TestValidateDocumentPoolSettingsAllowsDisabledZeroCapacity(t *testing.T) {
	settings := validDocumentPoolSettings()
	settings.Enabled = false
	settings.MaxActive = 0
	settings.MinReady = 0
	settings.PerTenantActiveLimit = 0
	if err := ValidateDocumentPoolSettings(settings, 4); err != nil {
		t.Fatalf("disabled zero-capacity settings rejected: %v", err)
	}
}

func TestValidateDocumentPoolSettingsReasonUsesCharacterLimit(t *testing.T) {
	if err := ValidateDocumentPoolSettingsReason(strings.Repeat("文", 500)); err != nil {
		t.Fatalf("500-character reason rejected: %v", err)
	}
	for name, reason := range map[string]string{
		"blank":    " \t\n ",
		"too long": strings.Repeat("文", 501),
	} {
		t.Run(name, func(t *testing.T) {
			if err := ValidateDocumentPoolSettingsReason(reason); err == nil {
				t.Fatal("expected validation error")
			}
		})
	}
}
