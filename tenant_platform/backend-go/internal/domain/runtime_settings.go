package domain

import "fmt"

const (
	IMInboundCoalesceWindowSettingKey = "im_inbound_coalesce_window_ms"
	DefaultIMInboundCoalesceWindowMS  = 2500
	MaxIMInboundCoalesceWindowMS      = 5000

	AgentMaxTurnsSettingKey = "agent_max_turns"
	DefaultAgentMaxTurns    = 80
	MinAgentMaxTurns        = 10
	MaxAgentMaxTurns        = 500
)

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
