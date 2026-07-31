package domain

import (
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	IMInboundCoalesceWindowSettingKey = "im_inbound_coalesce_window_ms"
	DefaultIMInboundCoalesceWindowMS  = 2500
	MaxIMInboundCoalesceWindowMS      = 5000

	AgentMaxTurnsSettingKey = "agent_max_turns"
	DefaultAgentMaxTurns    = 80
	MinAgentMaxTurns        = 10
	MaxAgentMaxTurns        = 500

	MaxDocumentPoolSettingsReasonCharacters = 500
)

var ErrDocumentPoolSettingsConflict = errors.New("document pool settings version conflict")

// DocumentPoolSettings is the atomically versioned document execution-pool
// configuration. Runtime consumers must apply a complete snapshot rather than
// reading individual fields independently.
type DocumentPoolSettings struct {
	Enabled               bool      `json:"enabled"`
	MaxActive             int       `json:"max_active"`
	MinReady              int       `json:"min_ready"`
	JobIdleTTLSeconds     int       `json:"job_idle_ttl_seconds"`
	ReadyIdleTTLSeconds   int       `json:"ready_idle_ttl_seconds"`
	GlobalQueueLimit      int       `json:"global_queue_limit"`
	PerTenantQueueLimit   int       `json:"per_tenant_queue_limit"`
	PerTenantActiveLimit  int       `json:"per_tenant_active_limit"`
	JobTimeoutSeconds     int       `json:"job_timeout_seconds"`
	CommandTimeoutSeconds int       `json:"command_timeout_seconds"`
	Version               int64     `json:"version"`
	UpdatedBy             int64     `json:"updated_by"`
	UpdatedAt             time.Time `json:"updated_at"`
	Reason                string    `json:"reason"`
}

// ValidateDocumentPoolSettings enforces both internal coherence and the
// deployment-owned capacity ceiling. The deployment ceiling is never stored in
// the mutable settings row.
func ValidateDocumentPoolSettings(settings DocumentPoolSettings, deploymentMaxActive int) error {
	if deploymentMaxActive <= 0 {
		return fmt.Errorf("deployment max_active must be positive")
	}
	if settings.MaxActive < 0 || settings.MaxActive > deploymentMaxActive {
		return fmt.Errorf("max_active must be between 0 and deployment maximum %d", deploymentMaxActive)
	}
	if settings.Enabled && settings.MaxActive == 0 {
		return fmt.Errorf("enabled document pool requires max_active greater than 0")
	}
	if settings.MinReady < 0 || settings.MinReady > settings.MaxActive {
		return fmt.Errorf("min_ready must be between 0 and max_active")
	}
	if settings.JobIdleTTLSeconds <= 0 {
		return fmt.Errorf("job_idle_ttl_seconds must be positive")
	}
	if settings.ReadyIdleTTLSeconds <= 0 {
		return fmt.Errorf("ready_idle_ttl_seconds must be positive")
	}
	if settings.GlobalQueueLimit <= 0 {
		return fmt.Errorf("global_queue_limit must be positive")
	}
	if settings.PerTenantQueueLimit <= 0 || settings.PerTenantQueueLimit > settings.GlobalQueueLimit {
		return fmt.Errorf("per_tenant_queue_limit must be between 1 and global_queue_limit")
	}
	if settings.PerTenantActiveLimit < 0 || settings.PerTenantActiveLimit > settings.MaxActive {
		return fmt.Errorf("per_tenant_active_limit must be between 0 and max_active")
	}
	if settings.Enabled && settings.PerTenantActiveLimit == 0 {
		return fmt.Errorf("enabled document pool requires per_tenant_active_limit greater than 0")
	}
	if settings.JobTimeoutSeconds <= 0 {
		return fmt.Errorf("job_timeout_seconds must be positive")
	}
	if settings.CommandTimeoutSeconds <= 0 || settings.CommandTimeoutSeconds > settings.JobTimeoutSeconds {
		return fmt.Errorf("command_timeout_seconds must be between 1 and job_timeout_seconds")
	}
	return nil
}

func ValidateDocumentPoolSettingsReason(reason string) error {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return fmt.Errorf("reason is required")
	}
	if utf8.RuneCountInString(reason) > MaxDocumentPoolSettingsReasonCharacters {
		return fmt.Errorf("reason must not exceed %d characters", MaxDocumentPoolSettingsReasonCharacters)
	}
	return nil
}

func ValidateIMInboundCoalesceWindowMS(windowMS int) error {
	if windowMS < 0 || windowMS > MaxIMInboundCoalesceWindowMS {
		return fmt.Errorf("window_ms must be between 0 and %d", MaxIMInboundCoalesceWindowMS)
	}
	return nil
}

func ValidateAgentMaxTurns(maxTurns int) error {
	if maxTurns < MinAgentMaxTurns || maxTurns > MaxAgentMaxTurns {
		return fmt.Errorf("max_turns must be between %d and %d", MinAgentMaxTurns, MaxAgentMaxTurns)
	}
	return nil
}
