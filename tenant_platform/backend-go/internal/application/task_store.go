package application

import (
	"context"
	"time"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/domain"
)

// TaskStore is the persistence port required by the application layer.
// *postgres.Store implements it implicitly; unit tests may inject a fake
// instead of hitting a real database (plan Task 5 Step 5: "Unit tests use an
// in-process gRPC test service and a real temporary-filesystem coordinator").
type TaskStore interface {
	SubmitTask(ctx context.Context, cmd domain.SubmitTaskCommand) (domain.Task, error)
	GetTask(ctx context.Context, taskID string) (domain.Task, error)
	CancelTask(ctx context.Context, taskID string, requesterUserID int64) (domain.Task, bool, error)
	ClaimNextTask(ctx context.Context, sessionKey, platformInstanceID string, claimLease time.Duration) (domain.Task, bool, error)
	RecoverAfterRestart(ctx context.Context, platformInstanceID string) (int, error)
	CompleteSucceeded(ctx context.Context, taskID, platformInstanceID, snapshotID, fileRef, checksum, resultRef, resultDigest string, resultBytes int) (domain.Task, error)
	CompleteFailedTerminal(ctx context.Context, taskID string, status domain.TaskStatus, deliveryType domain.DeliveryType, code, message, traceID string) (domain.Task, error)
	ListOwnedActiveTasks(ctx context.Context, platformInstanceID string) ([]domain.Task, error)
	HeartbeatClaim(ctx context.Context, taskID, platformInstanceID string, claimLease time.Duration) error
	ListClaimableSessionKeys(ctx context.Context, limit int) ([]string, error)
	MarkDispatchStarted(ctx context.Context, taskID, platformInstanceID, workerInstanceID string) (domain.Task, error)
	MarkRunning(ctx context.Context, taskID, platformInstanceID string) (domain.Task, error)
	RecordChunkEvent(ctx context.Context, taskID string, byteCount int, digest string) error
	// CountRunningTasks returns the global number of tasks in starting/running
	// status. Used by the scheduler to enforce MaxRunningTasks.
	CountRunningTasks(ctx context.Context) (int, error)
	// CountQueuedTasksByRequester returns the number of queued tasks for a
	// given requester. Used by SubmitTask to enforce PerUserQueueLimit.
	CountQueuedTasksByRequester(ctx context.Context, requesterUserID int64) (int, error)
}
