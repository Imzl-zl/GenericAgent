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
}

// Coordinator is the platform-owned checkpoint coordinator.
type Coordinator interface {
	Prepare(ctx context.Context, request CheckpointPrepareRequest) (CheckpointLease, error)
	Commit(ctx context.Context, ready ReadyCheckpoint) (CommittedCheckpoint, error)
	CurrentRestorePoint(ctx context.Context, workspaceID string) (RestorePoint, bool, error)
	ReadResult(ctx context.Context, ref string, expectedDigest string) (domain.ResultPayload, error)
}
