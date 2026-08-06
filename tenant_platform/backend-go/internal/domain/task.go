// Package domain holds platform task and result types owned by the control plane.
package domain

import (
	"errors"
	"time"
)

// SubmitTaskCommand is the validated carrier for enqueuing a task.
// MessageID becomes message_idempotency_key; callers cannot supply a second dedupe value.
type SubmitTaskCommand struct {
	SessionKey        string
	RequesterUserID   int64
	Source            string
	SourceInstanceID  string
	MessageID         string
	Prompt            string
	PersonaSnapshot   []string
	ToolPolicyVersion string
}

// TaskStatus is the durable task lifecycle state.
type TaskStatus string

const (
	TaskQueued      TaskStatus = "queued"
	TaskStarting    TaskStatus = "starting"
	TaskRunning     TaskStatus = "running"
	TaskSucceeded   TaskStatus = "succeeded"
	TaskFailed      TaskStatus = "failed"
	TaskCancelled   TaskStatus = "cancelled"
	TaskInterrupted TaskStatus = "interrupted"
)

// Source values are the protocol-level origins allowed for task submission
// (architecture §4.2: wechat|web).
const (
	SourceWechat = "wechat"
	SourceWeb    = "web"
)

// IsValidSource reports whether s is an allowed task source.
func IsValidSource(s string) bool {
	return s == SourceWechat || s == SourceWeb
}

// Task is the durable task envelope stored in PostgreSQL.
type Task struct {
	ID                    string
	SessionKey            string
	WorkspaceID           string
	RequesterID           int64
	Source                string
	SourceInstanceID      string
	MessageID             string
	MessageIdempotencyKey string
	Prompt                string
	PersonaSnapshot       []string
	ToolPolicyVersion     string
	ClaimOwner            string
	ClaimLeaseUntil       time.Time
	SessionSequence       int64
	Status                TaskStatus
	SnapshotID            string
	SnapshotChecksum      string
	ResultRef             string
	ResultDigest          string
	WorkerInstanceID      string
	// LastActivityAt is updated on every chunk event and Worker heartbeat
	// (drain_display_queue empty poll). Reaper uses it to detect "Worker alive
	// but deadlocked" (LLM HTTP call hung, GIL deadlock) — the scenario gRPC
	// stream errors + heartbeat lease loss cannot catch. Pattern: Temporal
	// HeartbeatTimeout / Kubernetes liveness probe.
	LastActivityAt time.Time
	// FreshSession is set when /new was issued since the last committed
	// snapshot. The scheduler stops the old Worker so the next task starts
	// with cleared history and working state (spec §7 /new).
	FreshSession bool
	// WorkerDispatchStartedAt is set when the scheduler records dispatch intent.
	WorkerDispatchStartedAt *time.Time
	CancelRequestedAt       *time.Time
	TerminalErrorCode       string
	TerminalErrorMessage    string
	TerminalErrorTraceID    string
	CreatedAt               time.Time
	UpdatedAt               time.Time
	StartedAt               *time.Time
	SucceededAt             *time.Time
	TerminalAt              *time.Time
	PromptBytes             int
	PersonaBytes            int
}

// ResultPayload is the bounded committed result body with its opaque ref and digest.
type ResultPayload struct {
	Ref    string
	Digest string
	Body   []byte
}

// Terminal states never re-enter claim.
func (s TaskStatus) IsTerminal() bool {
	switch s {
	case TaskSucceeded, TaskFailed, TaskCancelled, TaskInterrupted:
		return true
	default:
		return false
	}
}

// ErrPerUserQueueFull signals that a requester has reached the per-user
// queued-task cap. Defined in domain so both application and postgres layers
// can return/test against the same sentinel without import cycles.
var ErrPerUserQueueFull = errors.New("per-user queue limit reached")

// ErrSessionAccessDenied signals that a requester may not submit to the
// session (personal mismatch / not an approved team member / unapproved
// user / malformed session key). Defined in domain so the application layer
// can wrap it and the API layer can map it to 403 without import cycles.
var ErrSessionAccessDenied = errors.New("session access denied")

// ErrWorkspaceNotFound signals that no workspace row exists for the session
// key. Defined in domain so the postgres store can wrap it and the API layer
// can map it to 404 without import cycles.
var ErrWorkspaceNotFound = errors.New("workspace not found")

// ErrLeaseExpired signals that a claim heartbeat updated 0 rows because the
// caller no longer owns the task (lease expired or was stolen by recovery).
// Defined in domain so both the application (scheduler tick) and the postgres
// store can reference it without an import cycle.
var ErrLeaseExpired = errors.New("claim lease expired or lost")
