// Package domain holds platform task and result types owned by the control plane.
package domain

import "time"

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
