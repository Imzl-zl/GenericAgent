package transport

import (
	"context"
	"fmt"
	"sync"
)

// SentMessage records a single outbound message for test assertions.
type SentMessage struct {
	BotUUID     string
	ChannelAccountID string
	Text        string
	ClientID    string
}

type SentFile struct {
	BotUUID     string
	ChannelAccountID string
	FilePath    string
	FileName    string
	ClientID    string
}

// LoopbackTransport is an in-memory BotTransportAdapter for tests and dev
// loopback. It records sent messages and tracks message idempotency without
// any real IM SDK. Safe for concurrent use.
type LoopbackTransport struct {
	mu        sync.Mutex
	sent      []SentMessage
	sentFiles []SentFile
	streams   []StreamOp
	seen      map[string]bool // key = botUUID + "|" + messageID
	sendErr   error
}

// NewLoopbackTransport constructs an empty loopback transport.
func NewLoopbackTransport() *LoopbackTransport {
	return &LoopbackTransport{seen: make(map[string]bool)}
}

// SetSendError injects an error for the next SendMessage calls (test helper).
func (t *LoopbackTransport) SetSendError(err error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.sendErr = err
}

// SendMessage records the message in the sent slice. Returns sendErr if set.
func (t *LoopbackTransport) SendMessage(_ context.Context, botUUID, channelAccountID, text, clientID string) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.sendErr != nil {
		return t.sendErr
	}
	t.sent = append(t.sent, SentMessage{BotUUID: botUUID, ChannelAccountID: channelAccountID, Text: text, ClientID: clientID})
	return nil
}

func (t *LoopbackTransport) SendFile(_ context.Context, botUUID, channelAccountID, filePath, fileName, clientID string) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.sendErr != nil {
		return t.sendErr
	}
	t.sentFiles = append(t.sentFiles, SentFile{BotUUID: botUUID, ChannelAccountID: channelAccountID, FilePath: filePath, FileName: fileName, ClientID: clientID})
	return nil
}

// Sent returns a snapshot of recorded outbound messages (test helper).
func (t *LoopbackTransport) Sent() []SentMessage {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]SentMessage, len(t.sent))
	copy(out, t.sent)
	return out
}

// CheckMessageIdempotency 只读检查 (botUUID, messageID) 是否已标记(Round8:
// 处理失败路径不写入, 保证 Poller 重试可重新处理)。
func (t *LoopbackTransport) CheckMessageIdempotency(_ context.Context, botUUID, messageID string) (bool, error) {
	if botUUID == "" || messageID == "" {
		return false, fmt.Errorf("bot uuid and message id are required")
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.seen[botUUID+"|"+messageID], nil
}

// MarkMessageIdempotency 标记消息已成功处理(幂等)。
func (t *LoopbackTransport) MarkMessageIdempotency(_ context.Context, botUUID, messageID string) error {
	if botUUID == "" || messageID == "" {
		return fmt.Errorf("bot uuid and message id are required")
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.seen[botUUID+"|"+messageID] = true
	return nil
}

// SentMessages returns a copy of all sent messages (test helper).
func (t *LoopbackTransport) SentMessages() []SentMessage {	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]SentMessage, len(t.sent))
	copy(out, t.sent)
	return out
}

// LastSentMessage returns the most recent sent message, or empty if none.
func (t *LoopbackTransport) LastSentMessage() (SentMessage, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(t.sent) == 0 {
		return SentMessage{}, false
	}
	return t.sent[len(t.sent)-1], true
}

func (t *LoopbackTransport) SentFiles() []SentFile {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]SentFile, len(t.sentFiles))
	copy(out, t.sentFiles)
	return out
}

// StreamOp records one streaming operation for test assertions.
type StreamOp struct {
	BotUUID string
	Target  string
	ClientID string
	Op      string // open | append | commit | abort
	Text    string // append payload; empty otherwise
}

// SentStreams returns a snapshot of all streaming ops (test helper).
func (t *LoopbackTransport) SentStreams() []StreamOp {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]StreamOp, len(t.streams))
	copy(out, t.streams)
	return out
}

// loopbackStreamReply is the in-memory StreamReply for tests: records every
// op and tracks open/closed lifecycle.
type loopbackStreamReply struct {
	t        *LoopbackTransport
	botUUID  string
	target   string
	clientID string
}

func (r *loopbackStreamReply) Append(_ context.Context, text string) error {
	r.t.recordStream(StreamOp{BotUUID: r.botUUID, Target: r.target, ClientID: r.clientID, Op: "append", Text: text})
	return nil
}

func (r *loopbackStreamReply) Commit(_ context.Context) error {
	r.t.recordStream(StreamOp{BotUUID: r.botUUID, Target: r.target, ClientID: r.clientID, Op: "commit"})
	return nil
}

func (r *loopbackStreamReply) Abort(_ context.Context) error {
	r.t.recordStream(StreamOp{BotUUID: r.botUUID, Target: r.target, ClientID: r.clientID, Op: "abort"})
	return nil
}

func (t *LoopbackTransport) recordStream(op StreamOp) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.streams = append(t.streams, op)
}

// BeginReply implements StreamingSender: opens a loopback stream record.
// The first op (open) is recorded, then a live handle is returned.
func (t *LoopbackTransport) BeginReply(_ context.Context, botUUID, target, clientID, firstText string) (StreamReply, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.sendErr != nil {
		return nil, t.sendErr
	}
	op := StreamOp{BotUUID: botUUID, Target: target, ClientID: clientID, Op: "open", Text: firstText}
	t.streams = append(t.streams, op)
	return &loopbackStreamReply{t: t, botUUID: botUUID, target: target, clientID: clientID}, nil
}

// Reset clears all state (test helper).
func (t *LoopbackTransport) Reset() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.sent = nil
	t.sentFiles = nil
	t.streams = nil
	t.seen = make(map[string]bool)
	t.sendErr = nil
}
