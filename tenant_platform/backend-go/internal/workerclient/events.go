package workerclient

import (
	"errors"
	"fmt"

	workerv1 "github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/gen/worker/v1"
)

// Kind classifies a typed WorkerEvent for the scheduler without protobuf oneof handling.
type Kind int

const (
	KindUnknown Kind = iota
	KindChunk
	KindToolProgress
	KindTerminal
)

// WorkerEvent is a typed Go wrapper around the display stream.
// CheckpointReady is intentionally NOT a stream kind: checkpoint is a separate unary RPC.
type WorkerEvent struct {
	Kind         Kind
	Chunk        *workerv1.Chunk
	ToolProgress *workerv1.ToolProgress
	Terminal     *workerv1.Terminal
}

// IsChunk reports whether the event is a text chunk.
func (e WorkerEvent) IsChunk() bool { return e.Kind == KindChunk && e.Chunk != nil }

// IsToolProgress reports whether the event is tool progress.
func (e WorkerEvent) IsToolProgress() bool {
	return e.Kind == KindToolProgress && e.ToolProgress != nil
}

// IsTerminal reports whether the event is a terminal status.
func (e WorkerEvent) IsTerminal() bool { return e.Kind == KindTerminal && e.Terminal != nil }

// IsCheckpoint always returns false. Display stream events never carry checkpoint payloads;
// BeginCheckpoint is the only source of CheckpointReady.
func (e WorkerEvent) IsCheckpoint() bool { return false }

// MalformedEventError is returned when a display event cannot be converted safely.
type MalformedEventError struct {
	Reason string
}

func (e *MalformedEventError) Error() string {
	if e == nil {
		return "malformed worker event"
	}
	return fmt.Sprintf("malformed worker event: %s", e.Reason)
}

// IsMalformedEventError reports whether err is (or wraps) a MalformedEventError.
func IsMalformedEventError(err error) bool {
	var me *MalformedEventError
	return errors.As(err, &me)
}

// TransportError wraps a stream/transport failure (disconnect, unavailable, etc.).
// The client never retries; the manager/scheduler owns retry policy.
type TransportError struct {
	Op  string
	Err error
}

func (e *TransportError) Error() string {
	if e == nil {
		return "worker transport error"
	}
	if e.Op == "" {
		return fmt.Sprintf("worker transport error: %v", e.Err)
	}
	return fmt.Sprintf("worker transport error during %s: %v", e.Op, e.Err)
}

func (e *TransportError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

// IsTransportError reports whether err is (or wraps) a TransportError.
func IsTransportError(err error) bool {
	var te *TransportError
	return errors.As(err, &te)
}

// IsContextError reports whether err is context cancellation or deadline.
func IsContextError(err error) bool {
	return errors.Is(err, contextCanceled) || errors.Is(err, contextDeadline)
}

// ConvertEvent maps a generated protobuf WorkerEvent into the typed wrapper.
// Unknown/empty oneof values and invalid terminals become MalformedEventError.
func ConvertEvent(ev *workerv1.WorkerEvent) (WorkerEvent, error) {
	if ev == nil {
		return WorkerEvent{}, &MalformedEventError{Reason: "nil event"}
	}
	switch p := ev.GetPayload().(type) {
	case *workerv1.WorkerEvent_Chunk:
		if p.Chunk == nil {
			return WorkerEvent{}, &MalformedEventError{Reason: "nil chunk payload"}
		}
		return WorkerEvent{Kind: KindChunk, Chunk: p.Chunk}, nil
	case *workerv1.WorkerEvent_ToolProgress:
		if p.ToolProgress == nil {
			return WorkerEvent{}, &MalformedEventError{Reason: "nil tool_progress payload"}
		}
		return WorkerEvent{Kind: KindToolProgress, ToolProgress: p.ToolProgress}, nil
	case *workerv1.WorkerEvent_Terminal:
		if p.Terminal == nil {
			return WorkerEvent{}, &MalformedEventError{Reason: "nil terminal payload"}
		}
		if p.Terminal.GetStatus() == workerv1.TerminalStatus_TERMINAL_STATUS_UNSPECIFIED {
			return WorkerEvent{}, &MalformedEventError{Reason: "terminal status unspecified"}
		}
		return WorkerEvent{Kind: KindTerminal, Terminal: p.Terminal}, nil
	case nil:
		return WorkerEvent{}, &MalformedEventError{Reason: "empty payload oneof"}
	default:
		return WorkerEvent{}, &MalformedEventError{
			Reason: fmt.Sprintf("unknown display oneof type %T", p),
		}
	}
}
