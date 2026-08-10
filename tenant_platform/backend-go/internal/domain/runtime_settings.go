package domain

import (
	"fmt"
)

const (
	IMInboundCoalesceWindowSettingKey = "im_inbound_coalesce_window_ms"
	DefaultIMInboundCoalesceWindowMS  = 2500
	MaxIMInboundCoalesceWindowMS      = 5000

	AgentMaxTurnsSettingKey = "agent_max_turns"
	DefaultAgentMaxTurns    = 80
	MinAgentMaxTurns        = 10
	MaxAgentMaxTurns        = 500

	// IMStreamingModeSettingKey 是 IM 流式输出开关(off|final_only|streaming),
	// 默认 streaming(设计: 私聊默认开, 群聊由转发判定收敛)。
	IMStreamingModeSettingKey = "im_streaming_mode"
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

// ValidateIMStreamingMode 校验 im_streaming_mode 枚举值。
func ValidateIMStreamingMode(m string) error {
	if !ValidIMStreamingMode(IMStreamingMode(m)) {
		return fmt.Errorf("im_streaming_mode must be one of off|final_only|streaming, got %q", m)
	}
	return nil
}
