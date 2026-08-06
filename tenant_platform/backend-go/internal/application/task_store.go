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
	// IsApprovedTeamMember 报告 userID 是否为 teamID 的已批准成员
	// (审查 I-4: 团队 session 的任务提交/访问校验)。
	IsApprovedTeamMember(ctx context.Context, teamID string, userID int64) (bool, error)
	// IsApprovedUser 报告 userID 是否为 approved 状态用户
	// (审查 I-4: 任务提交门禁与 capability 在线校验一致——pending 用户
	// 提交的任务执行时必然被拒, 提交即拒绝避免无效任务)。
	IsApprovedUser(ctx context.Context, userID int64) (bool, error)
	SubmitTask(ctx context.Context, cmd domain.SubmitTaskCommand) (domain.Task, error)
	// SubmitTaskWithInboundMessage 同一事务内提交入站消息行与任务
	// (round10 审查 B7)。
	SubmitTaskWithInboundMessage(ctx context.Context, cmd domain.SubmitTaskCommand, msg domain.Message) (domain.Task, domain.Message, error)
	GetTask(ctx context.Context, taskID string) (domain.Task, error)
	CancelTask(ctx context.Context, taskID string, requesterUserID int64) (domain.Task, bool, error)
	ClaimNextTask(ctx context.Context, sessionKey, platformInstanceID string, claimLease time.Duration) (domain.Task, bool, error)
	RecoverAfterRestart(ctx context.Context, platformInstanceID string) (int, error)
	CompleteSucceeded(ctx context.Context, taskID, platformInstanceID, snapshotID, fileRef, checksum, resultRef, resultDigest string, resultBytes int, deliveryFiles []domain.DeliveryFile) (domain.Task, error)
	CompleteFailedTerminal(ctx context.Context, taskID, owner string, status domain.TaskStatus, deliveryType domain.DeliveryType, code, message, traceID string) (domain.Task, error)
	ListOwnedActiveTasks(ctx context.Context, platformInstanceID string) ([]domain.Task, error)
	HeartbeatClaim(ctx context.Context, taskID, platformInstanceID string, claimLease time.Duration) error
	ListClaimableSessionKeys(ctx context.Context, limit, perUserRunningLimit int) ([]string, error)
	MarkDispatchStarted(ctx context.Context, taskID, platformInstanceID, workerInstanceID string, freshSession bool) (domain.Task, error)
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
	// workspace 行锁下设置 reset_at、终态化未派发任务并对已派发任务写入
	// durable cancel request(审查 F3: 与 claim 竞态闭合)。
	ResetWorkspaceForNewSession(ctx context.Context, sessionKey string) (int, error)
	// WorkspaceIsFresh 返回 workspaces.reset_at 是否仍待消费(/new 后首个
	// 成功任务之前)。dispatch 时实时判定, 而非沿用提交时快照——多条 queued
	// 任务共享同一 fresh 快照会让第二条任务错误地空启动(审查 F2)。
	WorkspaceIsFresh(ctx context.Context, sessionKey string) (bool, error)
	// SetTaskCapabilityJTIs 持久化任务实际签发的 capability JTI 列表
	// (崩溃恢复时用于撤销, 审查: 撤销必须是持久工作流)。
	// platformInstanceID 与任务活跃 claim 绑定(审查 R5-I2): 任务已终态/被
	// 接管/lease 过期时拒绝持久化——不得把新签发的 JTI 挂到无法被撤销的行上。
	SetTaskCapabilityJTIs(ctx context.Context, taskID, platformInstanceID string, jtis []string) error
}
