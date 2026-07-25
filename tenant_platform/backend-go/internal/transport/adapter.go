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
	SendMessage(ctx context.Context, botUUID, ilinkUserID, text string) error

	// RecordMessageIdempotency records (botUUID, messageID) and returns true
	// if this is the first time the message has been seen. Returns false for
	// duplicates (spec §6.1 step 1: idempotent message recording).
	RecordMessageIdempotency(ctx context.Context, botUUID, messageID string) (bool, error)
}
