package transport

import "context"

// StreamingSender is an OPTIONAL transport capability: channels that support
// streaming replies (message-edit typewriter / native streaming API) implement
// it. Non-streaming channels (iLink WeChat, DingTalk v1) only implement
// BotTransportAdapter and keep the final-only delivery path.
//
// Design truth: tenant_platform/docs/IM_STREAMING_DELIVERY.zh-CN.md §4.2.
type StreamingSender interface {
	// BeginReply opens one streaming reply to the channel conversation.
	// botUUID identifies the channel config instance; target is the reply
	// destination (conversation_id; wechat-style single-bucket channels pass
	// the account id); clientID is the stable idempotency key (same contract
	// as BotTransportAdapter.SendMessage).
	BeginReply(ctx context.Context, botUUID, target, clientID string) (StreamReply, error)
}

// StreamReply is one open streaming reply handle. Implementations must be
// safe for sequential use: Append is called from the scheduler dispatch loop
// (throttled/merged), Commit/Abort exactly once at terminal.
type StreamReply interface {
	// Append delivers an incremental text fragment (already merged by the
	// scheduler's 500ms throttle window; no further batching expected).
	Append(ctx context.Context, text string) error
	// Commit finalizes the stream at terminal success: Feishu performs the
	// last PUT (typewriter final state), QQ closes the native streaming
	// message. After Commit the stream is closed.
	Commit(ctx context.Context) error
	// Abort drops the stream at failure: Feishu may rewrite the placeholder
	// with an "interrupted" hint. After Abort the stream is closed.
	Abort(ctx context.Context) error
}
