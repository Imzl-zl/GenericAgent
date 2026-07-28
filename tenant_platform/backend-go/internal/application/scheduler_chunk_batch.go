package application

import (
	"crypto/sha256"
	"encoding/hex"
	"hash"
	"time"
)

const (
	// Persist only windowed chunk metadata to PostgreSQL. The user-visible stream
	// remains real-time; this batcher only reduces DB/event amplification from
	// the legacy agent's ~30-byte display chunks.
	chunkEventBatchMaxBytes = 2 * 1024
	chunkEventBatchMaxDelay = time.Second
)

type chunkEventBatcher struct {
	pendingBytes    int
	hasher          hash.Hash
	windowStartedAt time.Time
}

func (b *chunkEventBatcher) Add(text string, now time.Time) (int, string, bool) {
	if text == "" {
		return 0, "", false
	}
	if b.hasher == nil {
		b.hasher = sha256.New()
		b.windowStartedAt = now
	}
	raw := []byte(text)
	_, _ = b.hasher.Write(raw)
	b.pendingBytes += len(raw)
	if b.pendingBytes < chunkEventBatchMaxBytes && now.Sub(b.windowStartedAt) < chunkEventBatchMaxDelay {
		return 0, "", false
	}
	return b.Flush()
}

func (b *chunkEventBatcher) Flush() (int, string, bool) {
	if b.pendingBytes == 0 || b.hasher == nil {
		return 0, "", false
	}
	byteCount := b.pendingBytes
	digest := "sha256:" + hex.EncodeToString(b.hasher.Sum(nil))
	b.pendingBytes = 0
	b.hasher = nil
	b.windowStartedAt = time.Time{}
	return byteCount, digest, true
}
