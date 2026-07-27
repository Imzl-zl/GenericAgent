package application

import (
	"context"
	"fmt"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/domain"
	workerv1 "github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/gen/worker/v1"
	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/infrastructure/checkpoint"
	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/infrastructure/postgres"
)

func (s *scheduler) completeSuccess(ctx context.Context, task domain.Task, terminal *workerv1.Terminal) error {
	if s.cfg.Coordinator == nil {
		_ = s.finalizeOrFail(ctx, task, domain.TaskFailed, domain.DeliveryTaskFailed,
			"NO_COORDINATOR", "checkpoint coordinator not configured", "")
		return fmt.Errorf("no coordinator")
	}
	lease, err := s.cfg.Coordinator.Prepare(ctx, checkpoint.CheckpointPrepareRequest{
		TaskID:         task.ID,
		WorkspaceID:    task.WorkspaceID,
		SessionKey:     task.SessionKey,
		MaxBundleBytes: s.cfg.MaxBundleBytes,
	})
	if err != nil {
		_ = s.finalizeOrFail(ctx, task, domain.TaskFailed, domain.DeliveryTaskFailed,
			"CHECKPOINT_PREPARE_FAILED", err.Error(), "")
		return err
	}
	s.mu.Lock()
	entry := s.workers[task.SessionKey]
	s.mu.Unlock()
	if entry == nil {
		err := fmt.Errorf("no worker for session %s", task.SessionKey)
		_ = s.finalizeOrFail(ctx, task, domain.TaskFailed, domain.DeliveryTaskFailed,
			"CHECKPOINT_WORKER_MISSING", err.Error(), "")
		_ = s.KickSession(ctx, task.SessionKey)
		return err
	}
	entry.lifecycleMu.Lock()
	defer entry.lifecycleMu.Unlock()
	if !s.workerEntryIsCurrent(task.SessionKey, entry) {
		err := fmt.Errorf("worker replaced for session %s", task.SessionKey)
		_ = s.finalizeOrFail(ctx, task, domain.TaskFailed, domain.DeliveryTaskFailed,
			"CHECKPOINT_WORKER_MISSING", err.Error(), "")
		_ = s.KickSession(ctx, task.SessionKey)
		return err
	}
	ready, err := entry.client.BeginCheckpoint(ctx, &workerv1.BeginCheckpointRequest{
		TaskId:          task.ID,
		CheckpointToken: lease.Token,
		StagingRef:      lease.StagingRef,
		MaxBundleBytes:  lease.MaxBundleBytes,
	})
	if err != nil {
		_ = s.finalizeOrFail(ctx, task, domain.TaskFailed, domain.DeliveryTaskFailed,
			"BEGIN_CHECKPOINT_FAILED", err.Error(), "")
		return err
	}
	committed, err := s.cfg.Coordinator.Commit(ctx, checkpoint.ReadyCheckpoint{
		TaskID:          task.ID,
		SnapshotID:      lease.SnapshotID,
		CheckpointToken: lease.Token,
		StagingRef:      ready.GetStagingRef(),
		Checksum:        ready.GetChecksum(),
		ResultDigest:    firstNonEmpty(ready.GetResultDigest(), terminal.GetResultDigest()),
	})
	if err != nil {
		_ = s.finalizeOrFail(ctx, task, domain.TaskFailed, domain.DeliveryTaskFailed,
			"CHECKPOINT_COMMIT_FAILED", err.Error(), "")
		return err
	}
	if terminal.GetResultDigest() != "" && committed.ResultDigest != "" && terminal.GetResultDigest() != committed.ResultDigest {
		_ = s.finalizeOrFail(ctx, task, domain.TaskFailed, domain.DeliveryTaskFailed,
			"RESULT_DIGEST_MISMATCH", "terminal and checkpoint result digests differ", "")
		return fmt.Errorf("result digest mismatch")
	}
	payload, err := s.cfg.Coordinator.ReadResult(ctx, committed.ResultRef, committed.ResultDigest)
	if err != nil {
		_ = s.finalizeOrFail(ctx, task, domain.TaskFailed, domain.DeliveryTaskFailed,
			"RESULT_READ_FAILED", err.Error(), "")
		return err
	}
	resultBytes := len(payload.Body)
	if _, err := s.cfg.Store.CompleteSucceeded(ctx, task.ID, s.cfg.PlatformInstanceID,
		committed.SnapshotID, committed.FileRef, committed.Checksum, committed.ResultRef, committed.ResultDigest, resultBytes); err != nil {
		return err
	}
	_ = s.KickSession(ctx, task.SessionKey)
	return nil
}

func boundMsg(message string) string {
	if len(message) > postgres.MaxTerminalErrorBytes {
		return message[:postgres.MaxTerminalErrorBytes]
	}
	return message
}

func firstNonEmpty(first, second string) string {
	if first != "" {
		return first
	}
	return second
}
