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
	ListClaimableSessionKeys(ctx context.Context, limit, perUserRunningLimit int) ([]string, error)
	MarkDispatchStarted(ctx context.Context, taskID, platformInstanceID, workerInstanceID string) (domain.Task, error)
	MarkRunning(ctx context.Context, taskID, platformInstanceID string) (domain.Task, error)
	RecordChunkEvent(ctx context.Context, taskID string, byteCount int, digest string) error
	// RecordHeartbeat refreshes tasks.last_activity_at without writing a chunk
	// event. Called when the Worker sends an empty-text Chunk as a heartbeat
	// (see task_drain.HEARTBEAT_INTERVAL_S). Used by the idle reaper.
	RecordHeartbeat(ctx context.Context, taskID string) error
	// CountRunningTasks returns the global number of tasks in starting/running
	// status. Used by the scheduler to enforce MaxRunningTasks.
	CountRunningTasks(ctx context.Context) (int, error)
	// RequeueTask moves a starting task claimed by platformInstanceID back to
	// queued and clears its claim fields. Used when dispatch fails with a
	// transient Runner capacity/ownership error so the task is retried on a
	// later tick instead of being terminalized (审查 C3: 满载保持 queued)。
	RequeueTask(ctx context.Context, taskID, platformInstanceID string) error
	// CountQueuedTasksByRequester returns the number of queued tasks for a
	// given requester. Used by SubmitTask to enforce PerUserQueueLimit.
	CountQueuedTasksByRequester(ctx context.Context, requesterUserID int64) (int, error)
	// ResetWorkspaceForNewSession marks the session for fresh start (/new):
	// 设置 reset_at 并取消该 session 所有 queued 任务(审查 R4-I8)。
	ResetWorkspaceForNewSession(ctx context.Context, sessionKey string) (int, error)
	// SetTaskCapabilityJTIs 持久化任务实际签发的 capability JTI 列表
	// (崩溃恢复时用于撤销, 审查: 撤销必须是持久工作流)。
	SetTaskCapabilityJTIs(ctx context.Context, taskID string, jtis []string) error
}
