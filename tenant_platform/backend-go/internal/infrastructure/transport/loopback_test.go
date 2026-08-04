package transport

import (
	"context"
	"errors"
	"testing"
)

func TestLoopbackSendMessageRecordsMessage(t *testing.T) {
	tr := NewLoopbackTransport()
	if err := tr.SendMessage(context.Background(), "bot-1", "user-1", "hello", ""); err != nil {
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
	if err := tr.SendMessage(context.Background(), "bot", "user", "text", ""); err == nil {
		t.Fatal("expected injected error")
	}
}

func TestLoopbackIdempotencyCheckDoesNotMark(t *testing.T) {
	tr := NewLoopbackTransport()
	ctx := context.Background()
	seen, err := tr.CheckMessageIdempotency(ctx, "bot-1", "msg-1")
	if err != nil || seen {
		t.Fatalf("unseen message: seen=%v err=%v", seen, err)
	}
	// Check 只读: 再次检查仍 unseen。
	seen2, err := tr.CheckMessageIdempotency(ctx, "bot-1", "msg-1")
	if err != nil || seen2 {
		t.Fatalf("check must not mark: seen=%v err=%v", seen2, err)
	}
	if err := tr.MarkMessageIdempotency(ctx, "bot-1", "msg-1"); err != nil {
		t.Fatal(err)
	}
	seen3, err := tr.CheckMessageIdempotency(ctx, "bot-1", "msg-1")
	if err != nil || !seen3 {
		t.Fatalf("marked message must be seen: seen=%v err=%v", seen3, err)
	}
}

func TestLoopbackIdempotencyDifferentMessagesIndependent(t *testing.T) {
	tr := NewLoopbackTransport()
	ctx := context.Background()
	_ = tr.MarkMessageIdempotency(ctx, "bot-1", "msg-1")
	seen, _ := tr.CheckMessageIdempotency(ctx, "bot-1", "msg-2")
	if seen {
		t.Fatal("different messages must be independent")
	}
}

func TestLoopbackIdempotencyRejectsEmptyInputs(t *testing.T) {
	tr := NewLoopbackTransport()
	ctx := context.Background()
	if _, err := tr.CheckMessageIdempotency(ctx, "", "msg"); err == nil {
		t.Fatal("expected error for empty bot uuid")
	}
	if _, err := tr.CheckMessageIdempotency(ctx, "bot", ""); err == nil {
		t.Fatal("expected error for empty message id")
	}
	if err := tr.MarkMessageIdempotency(ctx, "", "msg"); err == nil {
		t.Fatal("expected error for empty bot uuid on mark")
	}
	if err := tr.MarkMessageIdempotency(ctx, "bot", ""); err == nil {
		t.Fatal("expected error for empty message id on mark")
	}
}

func TestLoopbackLastSentMessage(t *testing.T) {
	tr := NewLoopbackTransport()
	ctx := context.Background()
	_, ok := tr.LastSentMessage()
	if ok {
		t.Fatal("expected no messages initially")
	}
	_ = tr.SendMessage(ctx, "bot", "user", "first", "")
	_ = tr.SendMessage(ctx, "bot", "user", "second", "")
	last, ok := tr.LastSentMessage()
	if !ok || last.Text != "second" {
		t.Fatalf("expected last message 'second', got %+v ok=%v", last, ok)
	}
}

func TestLoopbackResetClearsState(t *testing.T) {
	tr := NewLoopbackTransport()
	ctx := context.Background()
	_ = tr.SendMessage(ctx, "bot", "user", "text", "")
	_ = tr.MarkMessageIdempotency(ctx, "bot", "msg")
	tr.Reset()
	if len(tr.SentMessages()) != 0 {
		t.Fatal("expected no messages after reset")
	}
	seen, _ := tr.CheckMessageIdempotency(ctx, "bot", "msg")
	if seen {
		t.Fatal("expected unseen after reset (state cleared)")
	}
}
