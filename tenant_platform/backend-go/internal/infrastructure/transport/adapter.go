// Package transport defines the transport-agnostic BotTransportAdapter interface
// that decouples the Router from any specific IM SDK (iLink, mock, etc.).
package transport

import "context"

// BotTransportAdapter is the port for sending replies and recording message
// idempotency. The real iLink adapter (Slice 3b) and the LoopbackTransport
// mock both implement this interface.
//
// The adapter NEVER carries a real API key or upstream LLM credential
// (spec §3.3). It only holds per-bot transport state (cursor, reconnect).
type BotTransportAdapter interface {
	// SendMessage delivers a text reply to the bound user via the bot.
	// clientID 是稳定幂等键(round9 审查: 重试投递同一内容时保持同 id, 供
	// 远端去重); 无幂等键的调用(命令回复等)传空字符串。
	SendMessage(ctx context.Context, botUUID, ilinkUserID, text, clientID string) error
	// SendFile delivers a file reply to the bound user via the bot.
	// fileName 是用户可见的显示文件名(审查 R5-I10): 不得从 filePath 的
	// basename 推导——快照临时文件名含 marker hash 前缀, 会暴露给用户。
	SendFile(ctx context.Context, botUUID, ilinkUserID, filePath, fileName, clientID string) error

	// CheckMessageIdempotency 只读检查 (botUUID, messageID) 是否已成功处理过
	// (Round8 审查: 与 Mark 拆分, 避免失败路径提前消费消息)。
	CheckMessageIdempotency(ctx context.Context, botUUID, messageID string) (bool, error)
	// MarkMessageIdempotency 在消息成功处理后标记, 后续重复投递被拒绝。
	MarkMessageIdempotency(ctx context.Context, botUUID, messageID string) error
}
