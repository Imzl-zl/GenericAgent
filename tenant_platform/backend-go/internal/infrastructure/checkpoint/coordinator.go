// Package checkpoint owns Prepare/Commit/ReadResult for workspace snapshots.
package checkpoint

import (
	"context"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/domain"
)

// CheckpointPrepareRequest creates writing snapshot metadata before Worker BeginCheckpoint.
type CheckpointPrepareRequest struct {
	TaskID         string
	WorkspaceID    string
	SessionKey     string
	MaxBundleBytes uint64
	// RunnerGeneration 是签发时当前 Runner lease generation(方案 §7 fencing,
	// 审查 I7): 写入 snapshot 行, Commit/CompleteSucceeded 校验。
	RunnerGeneration uint64
}

// CheckpointLease is returned to the scheduler for BeginCheckpoint.
type CheckpointLease struct {
	SnapshotID     string
	Token          string
	StagingRef     string
	MaxBundleBytes uint64
}

// ReadyCheckpoint is the Worker-produced staging bundle metadata.
type ReadyCheckpoint struct {
	TaskID          string
	SnapshotID      string
	CheckpointToken string
	StagingRef      string
	Checksum        string
	ResultDigest    string
	// RunnerGeneration 是 Worker 回显的 Runner lease generation(与 Prepare
	// 时写入 snapshot 行的一致才可提交, 审查 I7)。
	RunnerGeneration uint64
}

// CommittedCheckpoint is the immutable committed bundle reference.
type CommittedCheckpoint struct {
	SnapshotID   string
	FileRef      string
	Checksum     string
	ResultRef    string
	ResultDigest string
}

// RestorePoint is the latest committed workspace state consumable by a new Worker.
type RestorePoint struct {
	SnapshotID  string
	SnapshotRef string
	Checksum    string
	// MaxBundleBytes 是恢复快照的硬性大小上限(写入时按 Prepare 校验)。
	// Runner 恢复时按此限长读取(审查 R4-I6)。
	MaxBundleBytes int64
}

// Coordinator is the platform-owned checkpoint coordinator.
type Coordinator interface {
	Prepare(ctx context.Context, request CheckpointPrepareRequest) (CheckpointLease, error)
	Commit(ctx context.Context, ready ReadyCheckpoint) (CommittedCheckpoint, error)
	CurrentRestorePoint(ctx context.Context, workspaceID string) (RestorePoint, bool, error)
	ReadResult(ctx context.Context, ref string, expectedDigest string) (domain.ResultPayload, error)
	// SweepExpiredCheckpoints 定期清理 checkpoint lease 已过期的 writing
	// snapshot(置为 quarantined)并删除其宿主 staging 文件(审查 R4-I12)。
	SweepExpiredCheckpoints(ctx context.Context) (int, error)
	// CleanupCommittedFiles 删除 Commit 已物化但 DB 提交失败的 committed/
	// results 文件(round10 审查 B9a): Commit 写文件与 CompleteSucceeded
	// 事务不是原子的, 事务失败后这些文件不被任何恢复指针引用, 必须清理,
	// 否则重复故障会永久占用宿主磁盘。
	CleanupCommittedFiles(ctx context.Context, committed CommittedCheckpoint) error
	// RunnerStagingRef 映射宿主 staging 路径为容器内路径(方案 §7:
	// Worker 只接受 runtime root 内的 staging_ref)。Local 实现原样返回。
	RunnerStagingRef(hostRef string) (string, error)
	// HostStagingRef 校验 Worker 返回的容器内 staging ref 与期望宿主 ref
	// 指向同一 token, 返回宿主 ref 供 Commit 校验(DB 记录为宿主路径)。
	HostStagingRef(runnerRef, expectedHostRef string) (string, error)
}
