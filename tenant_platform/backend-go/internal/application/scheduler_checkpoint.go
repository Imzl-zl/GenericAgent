package application

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

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
		s.destroyTaskWorkerLocked(task.SessionKey, entry)
		_ = s.finalizeOrFail(ctx, task, domain.TaskFailed, domain.DeliveryTaskFailed,
			"CHECKPOINT_PREPARE_FAILED", err.Error(), "")
		return err
	}
	runnerStagingRef, err := s.cfg.Coordinator.RunnerStagingRef(lease.StagingRef)
	if err != nil {
		s.destroyTaskWorkerLocked(task.SessionKey, entry)
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
		// 审查 R5-I8: 绑定当前 task 的 capability JTI, Worker 校验其在会话
		// 活跃凭据集中——终态撤销后旧 JTI 无法再发起 checkpoint。
		CapabilityJti: controlJTIFor(entry.credentials),
	})
	if err != nil {
		s.destroyTaskWorkerLocked(task.SessionKey, entry)
		_ = s.finalizeOrFail(ctx, task, domain.TaskFailed, domain.DeliveryTaskFailed,
			"BEGIN_CHECKPOINT_FAILED", err.Error(), "")
		return err
	}
	hostStagingRef, err := s.cfg.Coordinator.HostStagingRef(ready.GetStagingRef(), lease.StagingRef)
	if err != nil {
		s.destroyTaskWorkerLocked(task.SessionKey, entry)
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
		s.destroyTaskWorkerLocked(task.SessionKey, entry)
		_ = s.finalizeOrFail(ctx, task, domain.TaskFailed, domain.DeliveryTaskFailed,
			"CHECKPOINT_COMMIT_FAILED", err.Error(), "")
		return err
	}
	if terminal.GetResultDigest() != "" && committed.ResultDigest != "" && terminal.GetResultDigest() != committed.ResultDigest {
		s.destroyTaskWorkerLocked(task.SessionKey, entry)
		_ = s.finalizeOrFail(ctx, task, domain.TaskFailed, domain.DeliveryTaskFailed,
			"RESULT_DIGEST_MISMATCH", "terminal and checkpoint result digests differ", "")
		return fmt.Errorf("result digest mismatch")
	}
	payload, err := s.cfg.Coordinator.ReadResult(ctx, committed.ResultRef, committed.ResultDigest)
	if err != nil {
		s.destroyTaskWorkerLocked(task.SessionKey, entry)
		_ = s.finalizeOrFail(ctx, task, domain.TaskFailed, domain.DeliveryTaskFailed,
			"RESULT_READ_FAILED", err.Error(), "")
		return err
	}
	resultBytes := len(payload.Body)
	// 审查 R5-I3: 成功事务前捕获 [FILE:...] 输出文件的安全快照——内容必须
	// 绑定到任务完成时刻(Worker 仍持有串行槽), 否则同 Runner 下一条任务
	// 可能覆盖/删除同名输出, 异步 delivery 会交付错误内容。捕获失败
	// fail-closed: 声明了文件却无法交付的任务不得标记成功。
	deliveryFiles, err := captureTaskDeliverableFiles(ctx, s.cfg.SessionFiles, task.SessionKey, string(payload.Body))
	if err != nil {
		s.destroyTaskWorkerLocked(task.SessionKey, entry)
		_ = s.finalizeOrFail(ctx, task, domain.TaskFailed, domain.DeliveryTaskFailed,
			"DELIVERY_FILE_CAPTURE_FAILED", err.Error(), "")
		_ = s.KickSession(ctx, task.SessionKey)
		return err
	}
	committedTask, err := s.cfg.Store.CompleteSucceeded(ctx, task.ID, s.cfg.PlatformInstanceID,
		committed.SnapshotID, committed.FileRef, committed.Checksum, committed.ResultRef, committed.ResultDigest, resultBytes, deliveryFiles)
	if err != nil {
		// 结果不确定: 可能事务已提交但响应丢失, 也可能确实失败。重读任务
		// 状态: 已终态则不覆盖(避免把已提交成功覆盖为失败), 仅销毁 Worker
		// 内存态; 仍 running 则终态化为失败并销毁 Worker, 防止任务永久卡在
		// running 占住串行槽(dispatch goroutine 已结束, 无人会再驱动收尾)。
		latest, getErr := s.cfg.Store.GetTask(ctx, task.ID)
		if getErr == nil && latest.Status.IsTerminal() {
			// 事务实际已提交: committed/result 文件是恢复点, 不得删除。
			s.destroyTaskWorkerLocked(task.SessionKey, entry)
			_ = s.KickSession(ctx, task.SessionKey)
			return nil
		}
		// round10 审查(B9a): 提交失败且任务未终态——已物化的 committed/result
		// 文件不被任何恢复指针引用, 必须清理, 否则重复故障永久占用宿主磁盘。
		// round11 审查(C2)收紧: 仅当错误证明事务确定回滚(非
		// ErrCommitOutcomeUnknown)时才能删除文件——提交结果不确定
		// (网络/超时/重读失败)时, 文件可能已被 DB 引用, 删除会破坏恢复点;
		// 此时保留文件并交给 ReconcileOrphanCommittedFiles 按 DB 引用对账回收。
		if !errors.Is(err, postgres.ErrCommitOutcomeUnknown) {
			if s.cfg.Coordinator != nil {
				if cleanupErr := s.cfg.Coordinator.CleanupCommittedFiles(ctx, committed); cleanupErr != nil {
					slog.ErrorContext(ctx, "scheduler: cleanup orphan committed files failed",
						"task_id", task.ID, "snapshot_id", committed.SnapshotID, "error", cleanupErr)
				}
			}
		} else {
			slog.WarnContext(ctx, "scheduler: commit outcome unknown; deferring committed file cleanup to reconciliation",
				"task_id", task.ID, "snapshot_id", committed.SnapshotID, "error", err)
		}
		s.destroyTaskWorkerLocked(task.SessionKey, entry)
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
		s.destroyTaskWorkerLocked(task.SessionKey, entry)
		_ = s.KickSession(ctx, task.SessionKey)
		return nil
	}
	// 决策 D2.1: 任务终态即销毁 Worker——成功任务同样不保留进程, 下一
	// 任务从 checkpoint 冷启动全新 Worker。
	s.destroyTaskWorkerLocked(task.SessionKey, entry)
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
