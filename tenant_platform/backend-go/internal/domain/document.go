package domain

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

var (
	ErrDocumentGlobalQueueFull     = errors.New("document global queue limit reached")
	ErrDocumentWorkspaceQueueFull  = errors.New("document workspace queue limit reached")
	ErrDocumentPoolDisabled        = errors.New("document pool is disabled")
	ErrDocumentIdempotencyConflict = errors.New("document idempotency payload conflict")
	ErrDocumentFenceLost           = errors.New("document owner, generation, or lease fence lost")
	ErrDocumentCommandsNotClosed   = errors.New("document job commands are not closed")
	ErrDocumentCommandsClosed      = errors.New("document job commands are closed")
	ErrDocumentCommandsPending     = errors.New("document job has unfinished commands")
	ErrDocumentCommandsFailed      = errors.New("document job has failed or expired commands")
	ErrDocumentUnauthorized        = errors.New("document requester is not authorized for workspace")
	ErrDocumentTaskInactive        = errors.New("document tool task is not active")
	ErrDocumentJobNotFound         = errors.New("document job not found for task")
	ErrDocumentCommandNotFound     = errors.New("document command not found for request")
	ErrDocumentArtifactNotFound    = errors.New("document artifact not found")
	ErrDocumentJobState            = errors.New("document job state does not allow operation")
)

type DocumentJobStatus string

const (
	DocumentJobQueued    DocumentJobStatus = "queued"
	DocumentJobStarting  DocumentJobStatus = "starting"
	DocumentJobRunning   DocumentJobStatus = "running"
	DocumentJobSucceeded DocumentJobStatus = "succeeded"
	DocumentJobFailed    DocumentJobStatus = "failed"
	DocumentJobCancelled DocumentJobStatus = "cancelled"
	DocumentJobExpired   DocumentJobStatus = "expired"
)

func (s DocumentJobStatus) IsValid() bool {
	switch s {
	case DocumentJobQueued, DocumentJobStarting, DocumentJobRunning,
		DocumentJobSucceeded, DocumentJobFailed, DocumentJobCancelled, DocumentJobExpired:
		return true
	default:
		return false
	}
}

func (s DocumentJobStatus) IsTerminal() bool {
	switch s {
	case DocumentJobSucceeded, DocumentJobFailed, DocumentJobCancelled, DocumentJobExpired:
		return true
	default:
		return false
	}
}

type DocumentCommandStatus string

const (
	DocumentCommandPending   DocumentCommandStatus = "pending"
	DocumentCommandExecuting DocumentCommandStatus = "executing"
	DocumentCommandSucceeded DocumentCommandStatus = "succeeded"
	DocumentCommandFailed    DocumentCommandStatus = "failed"
	DocumentCommandExpired   DocumentCommandStatus = "expired"
)

func (s DocumentCommandStatus) IsValid() bool {
	switch s {
	case DocumentCommandPending, DocumentCommandExecuting, DocumentCommandSucceeded,
		DocumentCommandFailed, DocumentCommandExpired:
		return true
	default:
		return false
	}
}

type DocumentInstanceStatus string

const (
	DocumentInstanceReady      DocumentInstanceStatus = "ready"
	DocumentInstanceAllocated  DocumentInstanceStatus = "allocated"
	DocumentInstanceCreating   DocumentInstanceStatus = "creating"
	DocumentInstanceRunning    DocumentInstanceStatus = "running"
	DocumentInstanceDestroying DocumentInstanceStatus = "destroying"
	DocumentInstanceDestroyed  DocumentInstanceStatus = "destroyed"
	DocumentInstanceLost       DocumentInstanceStatus = "lost"
)

func (s DocumentInstanceStatus) IsValid() bool {
	switch s {
	case DocumentInstanceReady, DocumentInstanceAllocated, DocumentInstanceCreating,
		DocumentInstanceRunning, DocumentInstanceDestroying, DocumentInstanceDestroyed,
		DocumentInstanceLost:
		return true
	default:
		return false
	}
}

type SubmitDocumentJobCommand struct {
	WorkspaceID     string
	RequesterUserID int64
	IdempotencyKey  string
	Payload         json.RawMessage
}

type DocumentOperationRequest struct {
	SchemaVersion int             `json:"schema_version"`
	Operation     string          `json:"operation"`
	Parameters    json.RawMessage `json:"parameters"`
}

type SubmitDocumentCommand struct {
	JobID           string
	CommandID       string
	RequesterUserID int64
	Operation       DocumentOperationRequest
}

type DocumentToolTaskScope struct {
	TaskID      string
	SessionKey  string
	WorkspaceID string
}

type SubmitDocumentToolCommand struct {
	Scope     DocumentToolTaskScope
	RequestID string
	Operation DocumentOperationRequest
}

type DocumentToolSubmission struct {
	Job     DocumentJob
	Command DocumentCommand
}

type DocumentToolStatus struct {
	Job     DocumentJob
	Command *DocumentCommand
}

type DocumentJob struct {
	ID                   string
	WorkspaceID          string
	RequesterUserID      int64
	IdempotencyKey       string
	Payload              json.RawMessage
	Status               DocumentJobStatus
	InstanceID           string
	ClaimOwner           string
	Generation           int64
	ClaimLeaseUntil      *time.Time
	ClaimedAt            *time.Time
	LastActivityAt       time.Time
	TerminalErrorCode    string
	TerminalErrorMessage string
	CreatedAt            time.Time
	UpdatedAt            time.Time
	StartedAt            *time.Time
	TerminalAt           *time.Time
	CommandsClosedAt     *time.Time
}

type DocumentCommand struct {
	ID          string
	JobID       string
	CommandID   string
	Payload     json.RawMessage
	Status      DocumentCommandStatus
	Generation  int64
	CreatedAt   time.Time
	UpdatedAt   time.Time
	StartedAt   *time.Time
	CompletedAt *time.Time
}

const (
	MaxDocumentArtifactBytes = 8 * 1024 * 1024
	DocumentDOCXMediaType    = "application/vnd.openxmlformats-officedocument.wordprocessingml.document"
)

func ValidateDocumentArtifactMetadata(fileName, mediaType string) error {
	if fileName == "" || strings.TrimSpace(fileName) != fileName || len([]byte(fileName)) > 255 || !utf8.ValidString(fileName) || fileName == "." || fileName == ".." || strings.ContainsAny(fileName, `/\\`) {
		return fmt.Errorf("artifact file name is invalid")
	}
	for _, r := range fileName {
		if unicode.IsControl(r) {
			return fmt.Errorf("artifact file name is invalid")
		}
	}
	if len(fileName) < len(".docx") || !strings.EqualFold(fileName[len(fileName)-len(".docx"):], ".docx") {
		return fmt.Errorf("artifact file name must end with .docx")
	}
	if mediaType != DocumentDOCXMediaType || !utf8.ValidString(mediaType) {
		return fmt.Errorf("artifact media type is invalid")
	}
	return nil
}

type DocumentArtifact struct {
	ID        string
	JobID     string
	CommandID string
	FileName  string
	MediaType string
	Content   []byte
	SizeBytes int64
	SHA256    string
	CreatedAt time.Time
}

type CompleteDocumentArtifactCommand struct {
	JobID      string
	CommandID  string
	Owner      string
	Generation int64
	FileName   string
	MediaType  string
	Content    []byte
}

type DocumentInstance struct {
	ID             string
	InstanceName   string
	SlotPath       string
	Status         DocumentInstanceStatus
	AllocatedJobID string
	CreatedAt      time.Time
	UpdatedAt      time.Time
	ReadyAt        *time.Time
	AllocatedAt    *time.Time
	DestroyAt      *time.Time
}

type DocumentClaim struct {
	Job      DocumentJob
	Instance DocumentInstance
}

type DocumentPoolStatus struct {
	JobsQueued          int        `json:"jobs_queued"`
	JobsStarting        int        `json:"jobs_starting"`
	JobsRunning         int        `json:"jobs_running"`
	InstancesCreating   int        `json:"instances_creating"`
	InstancesReady      int        `json:"instances_ready"`
	InstancesAllocated  int        `json:"instances_allocated"`
	InstancesRunning    int        `json:"instances_running"`
	InstancesDestroying int        `json:"instances_destroying"`
	InstancesLost       int        `json:"instances_lost"`
	CommandsPending     int        `json:"commands_pending"`
	CommandsExecuting   int        `json:"commands_executing"`
	OldestQueuedAt      *time.Time `json:"oldest_queued_at,omitempty"`
	ObservedAt          time.Time  `json:"observed_at"`
}

type DocumentSweepResult struct {
	QueuedExpired   int
	CommandsExpired int
	JobsFailed      int
}
