package transport

import (
	"context"
	"errors"
	"testing"
)

func TestLoopbackSendMessageRecordsMessage(t *testing.T) {
	tr := NewLoopbackTransport()
	if err := tr.SendMessage(context.Background(), "bot-1", "user-1", "hello"); err != nil {
		t.Fatal(err)
	}
	sent := tr.SentMessages()
	if len(sent) != 1 {
		t.Fatalf("expected 1 sent message, got %d", len(sent))
	}
	if sent[0].BotUUID != "bot-1" || sent[0].IlinkUserID != "user-1" || sent[0].Text != "hello" {
		t.Fatalf("unexpected message: %+v", sent[0])
	}
}

func TestLoopbackSendMessageSurfacesInjectedError(t *testing.T) {
	tr := NewLoopbackTransport()
	tr.SetSendError(errors.New("transport down"))
	if err := tr.SendMessage(context.Background(), "bot", "user", "text"); err == nil {
		t.Fatal("expected injected error")
	}
}

func TestLoopbackIdempotencyFirstCallTrueSecondFalse(t *testing.T) {
	tr := NewLoopbackTransport()
	ctx := context.Background()
	first, err := tr.RecordMessageIdempotency(ctx, "bot-1", "msg-1")
	if err != nil {
		t.Fatal(err)
	}
	if !first {
		t.Fatal("first call should return true")
	}
	second, err := tr.RecordMessageIdempotency(ctx, "bot-1", "msg-1")
	if err != nil {
		t.Fatal(err)
	}
	if second {
		t.Fatal("second call should return false (duplicate)")
	}
}

func TestLoopbackIdempotencyDifferentMessagesBothTrue(t *testing.T) {
	tr := NewLoopbackTransport()
	ctx := context.Background()
	first, _ := tr.RecordMessageIdempotency(ctx, "bot-1", "msg-1")
	second, _ := tr.RecordMessageIdempotency(ctx, "bot-1", "msg-2")
	if !first || !second {
		t.Fatal("different messages should both return true")
	}
}

func TestLoopbackIdempotencyRejectsEmptyInputs(t *testing.T) {
	tr := NewLoopbackTransport()
	ctx := context.Background()
	if _, err := tr.RecordMessageIdempotency(ctx, "", "msg"); err == nil {
		t.Fatal("expected error for empty bot uuid")
	}
	if _, err := tr.RecordMessageIdempotency(ctx, "bot", ""); err == nil {
		t.Fatal("expected error for empty message id")
	}
}

func TestLoopbackLastSentMessage(t *testing.T) {
	tr := NewLoopbackTransport()
	ctx := context.Background()
	_, ok := tr.LastSentMessage()
	if ok {
		t.Fatal("expected no messages initially")
	}
	_ = tr.SendMessage(ctx, "bot", "user", "first")
	_ = tr.SendMessage(ctx, "bot", "user", "second")
	last, ok := tr.LastSentMessage()
	if !ok || last.Text != "second" {
		t.Fatalf("expected last message 'second', got %+v ok=%v", last, ok)
	}
}

func TestLoopbackResetClearsState(t *testing.T) {
	tr := NewLoopbackTransport()
	ctx := context.Background()
	_ = tr.SendMessage(ctx, "bot", "user", "text")
	_, _ = tr.RecordMessageIdempotency(ctx, "bot", "msg")
	tr.Reset()
	if len(tr.SentMessages()) != 0 {
		t.Fatal("expected no messages after reset")
	}
	first, _ := tr.RecordMessageIdempotency(ctx, "bot", "msg")
	if !first {
		t.Fatal("expected true after reset (state cleared)")
	}
}
