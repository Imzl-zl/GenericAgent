package domain

import (
	"errors"
	"time"
)

// ErrDuplicateInboundMessage indicates the (bot_id, message_id) pair already
// exists. Returned by MessageStore.InsertInboundMessage when the partial
// UNIQUE index rejects a duplicate INSERT. Callers treat this as
// "message already processed" rather than an error.
var ErrDuplicateInboundMessage = errors.New("duplicate inbound message")

// MessageDirection is whether a message was received from or sent to WeChat.
type MessageDirection string

const (
	MessageInbound  MessageDirection = "inbound"
	MessageOutbound MessageDirection = "outbound"
)

// MessageKind mirrors the iLink item_list type IDs (image=1, voice=2, video=3,
// file=4). Text is the platform default when no media is present.
const (
	MessageTypeText  = "text"
	MessageTypeImage = "image"
	MessageTypeVoice = "voice"
	MessageTypeVideo = "video"
	MessageTypeFile  = "file"
)

// Message is a single inbound or outbound WeChat message persisted for
// history, audit, and Web UI rendering. task_id is intentionally a soft
// reference (no FK) so audit records survive task deletion.
type Message struct {
	ID          int64
	UserID      int64
	BotID       int64
	SessionKey  string
	Direction   MessageDirection
	MessageID   string // iLink message_id (inbound) or empty (outbound)
	MessageType string
	Content     string // text body or media caption
	MediaPath   string // relative path under media-dir for media messages
	TaskID      string // soft ref to tasks.id, may be empty
	CreatedAt   time.Time
}
