package domain

import (
	"fmt"
)

const (
	IMInboundCoalesceWindowSettingKey = "im_inbound_coalesce_window_ms"
	// 2500→4000 (2026-08-15 生产实证): 微信"文字+文件"同批发送时文件上传
	// 使两条消息到达 poller 的时间差实测 3752ms(06:57:23.3 / 06:57:27.05),
	// 2500ms 窗口把同一次发送动作拆成两个任务、无附件任务误转旧会话文件;
	// 3500 仍不够(3752>3500), 4000 覆盖实测间隔。代价: 单条消息回复延迟
	// 增加 1.5s(窗口=每条入站消息被持有的时间)。
	DefaultIMInboundCoalesceWindowMS  = 4000
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
