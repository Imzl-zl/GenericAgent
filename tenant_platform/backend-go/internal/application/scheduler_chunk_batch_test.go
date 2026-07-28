package application

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
	"time"
)

func TestChunkEventBatcherFlushesWhenByteWindowIsFull(t *testing.T) {
	var batcher chunkEventBatcher
	now := time.Unix(1700000000, 0)
	prefix := "abc"
	body := prefix + strings.Repeat("x", chunkEventBatchMaxBytes-len(prefix))
	if bc, digest, ok := batcher.Add(body, now); !ok {
		t.Fatal("expected flush when byte window is full")
	} else {
		wantDigestBytes := sha256.Sum256([]byte(body))
		wantDigest := "sha256:" + hex.EncodeToString(wantDigestBytes[:])
		if bc != len(body) {
			t.Fatalf("byteCount=%d want=%d", bc, len(body))
		}
		if digest != wantDigest {
			t.Fatalf("digest=%q want=%q", digest, wantDigest)
		}
	}
	if bc, _, ok := batcher.Flush(); ok || bc != 0 {
		t.Fatalf("batcher should be empty after flush: ok=%v bytes=%d", ok, bc)
	}
}

func TestChunkEventBatcherFlushesWhenTimeWindowExpires(t *testing.T) {
	var batcher chunkEventBatcher
	t0 := time.Unix(1700000000, 0)
	if bc, _, ok := batcher.Add("hello", t0); ok || bc != 0 {
		t.Fatalf("unexpected early flush: ok=%v bytes=%d", ok, bc)
	}
	text := "world"
	bc, digest, ok := batcher.Add(text, t0.Add(chunkEventBatchMaxDelay))
	if !ok {
		t.Fatal("expected flush when time window expires")
	}
	wantBody := "hello" + text
	wantDigestBytes := sha256.Sum256([]byte(wantBody))
	wantDigest := "sha256:" + hex.EncodeToString(wantDigestBytes[:])
	if bc != len(wantBody) {
		t.Fatalf("byteCount=%d want=%d", bc, len(wantBody))
	}
	if digest != wantDigest {
		t.Fatalf("digest=%q want=%q", digest, wantDigest)
	}
}

func TestChunkEventBatcherManualFlushKeepsShortTasksObservable(t *testing.T) {
	var batcher chunkEventBatcher
	t0 := time.Unix(1700000000, 0)
	if _, _, ok := batcher.Add("short", t0); ok {
		t.Fatal("short task should stay buffered until final flush")
	}
	bc, digest, ok := batcher.Flush()
	if !ok {
		t.Fatal("expected final flush to emit buffered chunk metadata")
	}
	wantDigestBytes := sha256.Sum256([]byte("short"))
	wantDigest := "sha256:" + hex.EncodeToString(wantDigestBytes[:])
	if bc != len("short") {
		t.Fatalf("byteCount=%d want=%d", bc, len("short"))
	}
	if digest != wantDigest {
		t.Fatalf("digest=%q want=%q", digest, wantDigest)
	}
}
