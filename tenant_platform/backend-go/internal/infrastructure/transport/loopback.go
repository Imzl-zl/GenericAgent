package transport

import (
	"context"
	"fmt"
	"sync"
)

// SentMessage records a single outbound message for test assertions.
type SentMessage struct {
	BotUUID     string
	IlinkUserID string
	Text        string
}

type SentFile struct {
	BotUUID     string
	IlinkUserID string
	FilePath    string
}

// LoopbackTransport is an in-memory BotTransportAdapter for tests and dev
// loopback. It records sent messages and tracks message idempotency without
// any real IM SDK. Safe for concurrent use.
type LoopbackTransport struct {
	mu        sync.Mutex
	sent      []SentMessage
	sentFiles []SentFile
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
func (t *LoopbackTransport) SendMessage(_ context.Context, botUUID, ilinkUserID, text string) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.sendErr != nil {
		return t.sendErr
	}
	t.sent = append(t.sent, SentMessage{BotUUID: botUUID, IlinkUserID: ilinkUserID, Text: text})
	return nil
}

func (t *LoopbackTransport) SendFile(_ context.Context, botUUID, ilinkUserID, filePath string) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.sendErr != nil {
		return t.sendErr
	}
	t.sentFiles = append(t.sentFiles, SentFile{BotUUID: botUUID, IlinkUserID: ilinkUserID, FilePath: filePath})
	return nil
}

// RecordMessageIdempotency returns true the first time (botUUID, messageID) is
// seen; false on subsequent calls with the same pair.
func (t *LoopbackTransport) RecordMessageIdempotency(_ context.Context, botUUID, messageID string) (bool, error) {
	if botUUID == "" || messageID == "" {
		return false, fmt.Errorf("bot uuid and message id are required")
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	key := botUUID + "|" + messageID
	if t.seen[key] {
		return false, nil
	}
	t.seen[key] = true
	return true, nil
}

// SentMessages returns a copy of all sent messages (test helper).
func (t *LoopbackTransport) SentMessages() []SentMessage {
	t.mu.Lock()
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

// Reset clears all state (test helper).
func (t *LoopbackTransport) Reset() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.sent = nil
	t.sentFiles = nil
	t.seen = make(map[string]bool)
	t.sendErr = nil
}
