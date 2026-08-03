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
	// Runner generation fencing(审查 I7): 签发 checkpoint lease 时绑定当前
	// Runner lease generation, Commit 时逐项校验, 旧 generation Runner 无法
	// 提交恢复点。
	lease, err := s.cfg.Coordinator.Prepare(ctx, checkpoint.CheckpointPrepareRequest{
		TaskID:           task.ID,
		WorkspaceID:      task.WorkspaceID,
		SessionKey:       task.SessionKey,
		MaxBundleBytes:   s.cfg.MaxBundleBytes,
		RunnerGeneration: entry.runnerGeneration,
	})
	if err != nil {
		// checkpoint 失败: Worker 内存状态已推进但未持久化, 销毁重建(审查:
		// 失败后不复用已变更 Worker)。
		s.evictWorkerAfterFailureLocked(task.SessionKey, entry)
		_ = s.finalizeOrFail(ctx, task, domain.TaskFailed, domain.DeliveryTaskFailed,
			"CHECKPOINT_PREPARE_FAILED", err.Error(), "")
		return err
	}
	runnerStagingRef, err := s.cfg.Coordinator.RunnerStagingRef(lease.StagingRef)
	if err != nil {
		s.evictWorkerAfterFailureLocked(task.SessionKey, entry)
		_ = s.finalizeOrFail(ctx, task, domain.TaskFailed, domain.DeliveryTaskFailed,
			"CHECKPOINT_PREPARE_FAILED", err.Error(), "")
		return err
	}
	ready, err := entry.client.BeginCheckpoint(ctx, &workerv1.BeginCheckpointRequest{
		TaskId:           task.ID,
		CheckpointToken:  lease.Token,
		StagingRef:       runnerStagingRef,
		MaxBundleBytes:   lease.MaxBundleBytes,
		RunnerGeneration: entry.runnerGeneration,
	})
	if err != nil {
		s.evictWorkerAfterFailureLocked(task.SessionKey, entry)
		_ = s.finalizeOrFail(ctx, task, domain.TaskFailed, domain.DeliveryTaskFailed,
			"BEGIN_CHECKPOINT_FAILED", err.Error(), "")
		return err
	}
	hostStagingRef, err := s.cfg.Coordinator.HostStagingRef(ready.GetStagingRef(), lease.StagingRef)
	if err != nil {
		s.evictWorkerAfterFailureLocked(task.SessionKey, entry)
		_ = s.finalizeOrFail(ctx, task, domain.TaskFailed, domain.DeliveryTaskFailed,
			"BEGIN_CHECKPOINT_FAILED", err.Error(), "")
		return err
	}
	committed, err := s.cfg.Coordinator.Commit(ctx, checkpoint.ReadyCheckpoint{
		TaskID:           task.ID,
		SnapshotID:       lease.SnapshotID,
		CheckpointToken:  lease.Token,
		StagingRef:       hostStagingRef,
		Checksum:         ready.GetChecksum(),
		ResultDigest:     firstNonEmpty(ready.GetResultDigest(), terminal.GetResultDigest()),
		RunnerGeneration: ready.GetRunnerGeneration(),
	})
	if err != nil {
		s.evictWorkerAfterFailureLocked(task.SessionKey, entry)
		_ = s.finalizeOrFail(ctx, task, domain.TaskFailed, domain.DeliveryTaskFailed,
			"CHECKPOINT_COMMIT_FAILED", err.Error(), "")
		return err
	}
	if terminal.GetResultDigest() != "" && committed.ResultDigest != "" && terminal.GetResultDigest() != committed.ResultDigest {
		s.evictWorkerAfterFailureLocked(task.SessionKey, entry)
		_ = s.finalizeOrFail(ctx, task, domain.TaskFailed, domain.DeliveryTaskFailed,
			"RESULT_DIGEST_MISMATCH", "terminal and checkpoint result digests differ", "")
		return fmt.Errorf("result digest mismatch")
	}
	payload, err := s.cfg.Coordinator.ReadResult(ctx, committed.ResultRef, committed.ResultDigest)
	if err != nil {
		s.evictWorkerAfterFailureLocked(task.SessionKey, entry)
		_ = s.finalizeOrFail(ctx, task, domain.TaskFailed, domain.DeliveryTaskFailed,
			"RESULT_READ_FAILED", err.Error(), "")
		return err
	}
	resultBytes := len(payload.Body)
	committedTask, err := s.cfg.Store.CompleteSucceeded(ctx, task.ID, s.cfg.PlatformInstanceID,
		committed.SnapshotID, committed.FileRef, committed.Checksum, committed.ResultRef, committed.ResultDigest, resultBytes)
	if err != nil {
		// 结果不确定: 可能事务已提交但响应丢失, 也可能确实失败。重读任务
		// 状态: 已终态则不覆盖(避免把已提交成功覆盖为失败), 仅销毁 Worker
		// 内存态; 仍 running 则终态化为失败并销毁 Worker, 防止任务永久卡在
		// running 占住串行槽(dispatch goroutine 已结束, 无人会再驱动收尾)。
		latest, getErr := s.cfg.Store.GetTask(ctx, task.ID)
		if getErr == nil && latest.Status.IsTerminal() {
			s.evictWorkerAfterFailureLocked(task.SessionKey, entry)
			_ = s.KickSession(ctx, task.SessionKey)
			return nil
		}
		s.evictWorkerAfterFailureLocked(task.SessionKey, entry)
		_ = s.finalizeOrFail(ctx, task, domain.TaskFailed, domain.DeliveryTaskFailed,
			"CHECKPOINT_COMMIT_FAILED", err.Error(), "")
		_ = s.KickSession(ctx, task.SessionKey)
		return err
	}
	// 提交事务可能把任务终态化为 interrupted(取消在提交前被接受):
	// Worker 内存状态仍是取消任务的未提交 history/working, 必须销毁,
	// 否则下一任务会继承取消任务的脏状态(审查: 成功 checkpoint 与任务成功
	// 原子提交, 取消路径不得复用 Worker)。
	if committedTask.Status != domain.TaskSucceeded {
		s.evictWorkerAfterFailureLocked(task.SessionKey, entry)
		_ = s.KickSession(ctx, task.SessionKey)
		return nil
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
