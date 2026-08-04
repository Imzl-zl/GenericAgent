package application

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/domain"
	workerv1 "github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/gen/worker/v1"
	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/infrastructure/postgres"
)

// isTransientRunnerError 判断 dispatch 启动失败是否属于 Runner 容量/所有权
// 类瞬时错误: 此类错误不应终态化任务, 而应退回 queued 等待重试(审查 C3)。
func isTransientRunnerError(err error) bool {
	return errors.Is(err, postgres.ErrRunnerLeaseCapacity) || errors.Is(err, postgres.ErrRunnerLeaseOwned)
}

// finalizeOrFail records a terminal task state + delivery and surfaces any
// persistence failure via log instead of silently dropping it (global rule:
// No Silent Fallbacks). The returned task is the updated row on success, or
// the original task on failure so callers can continue without a panic.
// 审查 R5-Critical-2: 终态化必须由当前 claim owner 在 lease 有效期内执行——
// lease 已被接管/过期时(ErrTaskNotOwned)任务由 RecoverAfterRestart 或新
// owner 处理, 此处仅记 Warn 不得覆盖其状态。
func (s *scheduler) finalizeOrFail(ctx context.Context, task domain.Task, status domain.TaskStatus, deliveryType domain.DeliveryType, code, message, traceID string) domain.Task {
	t, err := s.cfg.Store.CompleteFailedTerminal(ctx, task.ID, s.cfg.PlatformInstanceID, status, deliveryType, code, message, traceID)
	if err != nil {
		if errors.Is(err, postgres.ErrTaskNotOwned) {
			slog.WarnContext(ctx, "scheduler: task claim lost; skip terminalization",
				"task_id", task.ID,
				"session_key", task.SessionKey,
				"target_status", string(status))
			return task
		}
		slog.ErrorContext(ctx, "scheduler: CompleteFailedTerminal failed",
			"task_id", task.ID,
			"session_key", task.SessionKey,
			"target_status", string(status),
			"error", err)
		return task
	}
	return t
}

func (s *scheduler) finalizeTaskDeadline(ctx context.Context, task domain.Task) error {
	cancelCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.CancelWorker(cancelCtx, task); err != nil {
		slog.WarnContext(ctx, "scheduler: deadline cancel failed", "task_id", task.ID, "error", err)
	}
	// 超时后 Worker 内存状态不确定, 不复用(审查: 失败后不复用)。
	s.evictWorkerAfterFailure(task.SessionKey)
	s.finalizeOrFail(ctx, task, domain.TaskFailed, domain.DeliveryTaskFailed,
		"TASK_DEADLINE_EXCEEDED", "task exceeded maximum wall-clock duration", "")
	_ = s.KickSession(ctx, task.SessionKey)
	return context.DeadlineExceeded
}

func workerTaskEnvelope(task domain.Task, runnerGeneration uint64, capabilityJTI string) *workerv1.TaskEnvelope {
	return &workerv1.TaskEnvelope{
		TaskId:            task.ID,
		SessionKey:        task.SessionKey,
		RequesterUserId:   task.RequesterID,
		Source:            task.Source,
		SourceInstanceId:  task.SourceInstanceID,
		MessageId:         task.MessageID,
		Prompt:            task.Prompt,
		PersonaSnapshot:   append([]string(nil), task.PersonaSnapshot...),
		ToolPolicyVersion: task.ToolPolicyVersion,
		CreatedAt:         timestamppb.New(task.CreatedAt),
		RunnerGeneration:  runnerGeneration,
		CapabilityJti:     capabilityJTI,
	}
}

func (s *scheduler) dispatch(ctx context.Context, task domain.Task) (returnErr error) {
	// 进程内 in-flight gate(审查 R4-C1): tick 对 WorkerDispatchStartedAt==nil
	// 的任务每轮都会派发; dispatch 在 MarkDispatchStarted 之前有昂贵的初始化,
	// 冷启动超过一个 tick 间隔时第二个副本会重复创建 Runner/互相销毁
	// Worker。LoadOrStore 保证同一 task 只有一个 dispatch goroutine。
	if _, loaded := s.dispatchInFlight.LoadOrStore(task.ID, struct{}{}); loaded {
		return nil
	}
	defer s.dispatchInFlight.Delete(task.ID)
	defer func() {
		if r := recover(); r != nil {
			returnErr = fmt.Errorf("dispatch panic: %v", r)
			slog.ErrorContext(ctx, "scheduler: dispatch panic",
				"task_id", task.ID, "panic", r)
			_ = s.finalizeOrFail(ctx, task, domain.TaskFailed, domain.DeliveryTaskFailed,
				"DISPATCH_PANIC", fmt.Sprintf("%v", r), "")
		}
	}()
	// 终态后立即撤销本任务实际使用的 credential 集(审查 I9): 任务
	// success/fail/cancel/deadline 后旧 task 的 capability token 不得继续被
	// Runner 复用。集合在 ExecuteTask 前捕获(entry.credentials 在任务执行
	// 期间不会刷新: Worker 拒绝任务期 reload), 不能撤销 defer 时刻的
	// "当前"集合——新任务可能已刷新并替换 entry.credentials。
	// revokeSessionCredentialsIfTerminal 内部检查任务是否已终态,
	// requeue 分支(任务仍 queued)不会撤销。
	var taskCredentialSet workerCredentialSet
	// 审查(第三轮 review C1): 必须用闭包捕获——Go 的 defer 在注册时立即
	// 求值实参, 直接传变量会捕获当时的零值空集, 使撤销在任何路径都无效。
	defer func() {
		s.revokeSessionCredentialsIfTerminal(ctx, task.ID, task.SessionKey, taskCredentialSet)
	}()
	// Re-check under store: may have been cancelled before dispatch.
	cur, err := s.cfg.Store.GetTask(ctx, task.ID)
	if err != nil {
		return err
	}
	if cur.Status.IsTerminal() {
		return nil
	}
	if cur.CancelRequestedAt != nil && cur.WorkerDispatchStartedAt == nil {
		// Cancelled before dispatch: store should already have terminalized if cancel path ran.
		return nil
	}
	if cur.Status != domain.TaskStarting {
		return nil
	}
	// /new 消费标记实时判定(审查 F2): fresh 语义由 workspaces.reset_at 的
	// 当前值决定, 而不是任务提交时的快照——首条 fresh 任务成功清除
	// reset_at 后, 后提交的 queued 任务在此处判定为非 fresh, 正确继承
	// 上一条任务已提交的 checkpoint, 而不是再次空启动。
	fresh, err := s.cfg.Store.WorkspaceIsFresh(ctx, task.SessionKey)
	if err != nil {
		return fmt.Errorf("resolve workspace fresh flag: %w", err)
	}
	task.FreshSession = fresh
	heartbeat, err := s.startDispatchHeartbeat(ctx, task)
	if err != nil {
		latest, getErr := s.cfg.Store.GetTask(ctx, task.ID)
		if getErr == nil && latest.Status.IsTerminal() {
			return nil
		}
		return err
	}
	defer func() {
		if err := heartbeat.Stop(); err != nil {
			// 审查 R5-I1: 任务已退回 queued(容量满/foreign-owner 瞬时错误)——
			// Stop 的 lease 丢失错误是 requeue 的预期结果, 不得终态化任务。
			if heartbeat.requeued.Load() {
				return
			}
			// heartbeat 持续失败(lease 丢失或瞬时 DB 错误重试耗尽, 审查 F5):
			// 放弃本次派发。任务未终态时必须立即终态化并销毁 Worker, 否则
			// 任务卡在 starting 直到 idle reaper(且 dispatch 已无法继续驱动)。
			fbCtx, fbCancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer fbCancel()
			latest, getErr := s.cfg.Store.GetTask(fbCtx, task.ID)
			if getErr == nil && !latest.Status.IsTerminal() {
				_ = s.CancelWorker(fbCtx, latest)
				s.evictWorkerAfterFailure(task.SessionKey)
				_ = s.finalizeOrFail(fbCtx, latest, domain.TaskInterrupted, domain.DeliveryTaskInterrupted,
					"CLAIM_LEASE_LOST", err.Error(), "")
				_ = s.KickSession(fbCtx, task.SessionKey)
			}
			returnErr = fmt.Errorf("claim heartbeat: %w", err)
		}
	}()
	ctx = heartbeat.ctx

	// /new was issued: stop any existing Worker for this session so the
	// next task starts with cleared history and working state.
	if task.FreshSession {
		s.stopSessionWorker(task.SessionKey)
	}

	client, entry, err := s.ensureWorker(ctx, task)
	if err != nil {
		// Runner 容量满或 lease 被其他实例持有: 不是任务失败, 退回 queued
		// 等下一轮 tick 重试(审查 C3: 满载保持 queued, 绝不终态化)。
		if isTransientRunnerError(err) {
			// 审查 R5-I1: 必须在 RequeueTask 之前标记 requeue——RequeueTask
			// 提交后 claim 清空, 若 heartbeat ticker 在 deferred Stop 之前触发
			// HeartbeatClaim, 0 rows 会被误判为 lease 丢失并终态化任务;
			// 标记后 ticker 静默退出且 Stop fallback 跳过终态化。
			heartbeat.markRequeued()
			if requeueErr := s.cfg.Store.RequeueTask(ctx, task.ID, s.cfg.PlatformInstanceID); requeueErr != nil {
				slog.ErrorContext(ctx, "scheduler: requeue task after runner capacity error failed",
					"task_id", task.ID, "error", requeueErr)
			}
			_ = s.KickSession(ctx, task.SessionKey)
			return nil
		}
		s.auditRoutingBinding(ctx, task, entry, "error", "WORKER_CREDENTIAL_PREPARE_FAILED")
		_ = s.finalizeOrFail(ctx, task, domain.TaskFailed, domain.DeliveryTaskFailed,
			"WORKER_START_FAILED", err.Error(), "")
		_ = s.KickSession(ctx, task.SessionKey)
		return err
	}
	s.auditRoutingBinding(ctx, task, entry, "success", "")
	// 捕获本任务使用的 credential 集(撤销 defer 使用; 审查: 必须在任何
	// 可能终态化的分支之前捕获——StartSession/Policy/MarkRunning 失败等
	// 早期终态路径同样需要撤销已签发的 token)。任务执行期间 Worker 拒绝
	// reload, entry.credentials 在 ExecuteTask 期间不会变化。
	taskCredentialSet = entry.credentials
	s.workerCallMu.Lock()
	workerCallLocked := true
	releaseWorkerCall := func() {
		if workerCallLocked {
			workerCallLocked = false
			s.workerCallMu.Unlock()
		}
	}
	defer releaseWorkerCall()
	// Record dispatch intent BEFORE StartSession so cancel-during-StartSession
	// sees WorkerDispatchStartedAt != nil and records a durable cancel request
	// instead of finalizing immediately. fresh_session 在此事务内写回任务行
	// (审查 F2: 与 checkpoint 成功提交时的 reset_at 清除判定一致)。
	cur, err = s.cfg.Store.MarkDispatchStarted(ctx, task.ID, s.cfg.PlatformInstanceID, entry.instID, task.FreshSession)
	if err != nil {
		slog.ErrorContext(ctx, "scheduler: MarkDispatchStarted failed",
			"task_id", task.ID, "error", err)
		// 审查 R4-C2: 派发无法继续且任务未终态——本地 Worker 已创建但可能
		// 绑定错误归属(claim 被接管), 销毁防脏复用; 任务保持 starting 由
		// 下一轮 tick 重新派发(WorkerDispatchStartedAt 仍为 NULL)。
		s.evictWorkerAfterFailure(task.SessionKey)
		return nil
	}
	// 注意: 任务签发的 JTI 已在 issueProviderCapabilitiesWithRuntime 内、
	// token 暴露给 Runner 之前持久化(审查 F1), 此处不再重复写入。

	if _, err := s.cfg.Registry.Resolve(CapabilityVersion, task.ToolPolicyVersion); err != nil {
		_ = s.finalizeOrFail(ctx, task, domain.TaskFailed, domain.DeliveryTaskFailed,
			"POLICY_RESOLVE_FAILED", err.Error(), "")
		return err
	}

	// StartSession happens after MarkDispatchStarted so the durable cancel path
	// can observe and record CancelRequestedAt while StartSession is in flight.
	if err := s.startSessionOnWorker(ctx, task); err != nil {
		_ = s.finalizeOrFail(ctx, task, domain.TaskFailed, domain.DeliveryTaskFailed,
			"WORKER_START_FAILED", err.Error(), "")
		_ = s.KickSession(ctx, task.SessionKey)
		return err
	}

	if _, err := s.cfg.Store.MarkRunning(ctx, task.ID, s.cfg.PlatformInstanceID); err != nil {
		// 审查 R4-C2: dispatch 无法继续且任务未终态。销毁本地 Worker 防脏
		// 复用(任务可能已被接管/终态化, 本地进程不得留给下一任务), 并尽力
		// 终态化防任务卡死; 失败时由 lease 过期恢复路径兜底, 无副作用。
		s.evictWorkerAfterFailure(task.SessionKey)
		_ = s.finalizeOrFail(ctx, task, domain.TaskInterrupted, domain.DeliveryTaskInterrupted,
			"MARK_RUNNING_FAILED", err.Error(), "")
		_ = s.KickSession(ctx, task.SessionKey)
		return err
	}

	// Round-trip durable envelope from PostgreSQL (never scheduler memory).
	taskRow, err := s.cfg.Store.GetTask(ctx, task.ID)
	if err != nil {
		// 审查 R4-C2: 同 MarkRunning 失败路径——销毁 Worker + 尽力终态化。
		s.evictWorkerAfterFailure(task.SessionKey)
		_ = s.finalizeOrFail(ctx, task, domain.TaskInterrupted, domain.DeliveryTaskInterrupted,
			"TASK_STATE_READ_FAILED", err.Error(), "")
		_ = s.KickSession(ctx, task.SessionKey)
		return err
	}
	if taskRow.Status.IsTerminal() {
		return nil
	}
	if taskRow.CancelRequestedAt != nil {
		// 审查: StartSession 已执行但任务在 ExecuteTask 前被取消, Worker
		// 内存状态未提交, 销毁重建而非复用。
		s.evictWorkerAfterFailure(task.SessionKey)
		_, err := s.cfg.Store.CompleteFailedTerminal(ctx, task.ID, s.cfg.PlatformInstanceID, domain.TaskInterrupted, domain.DeliveryTaskInterrupted,
			"TASK_INTERRUPTED", "task interrupted before worker execution", "")
		_ = s.KickSession(ctx, task.SessionKey)
		return err
	}
	req := &workerv1.ExecuteTaskRequest{Task: workerTaskEnvelope(taskRow, entry.runnerGeneration, firstJTI(entry.credentials))}

	taskDeadline := time.Now().Add(s.cfg.MaxTaskWallClock)
	executeCtx, cancelExecute := context.WithTimeout(ctx, s.cfg.MaxTaskWallClock)
	defer cancelExecute()
	events, errs := client.ExecuteTask(executeCtx, req)
	releaseWorkerCall()
	recordChunkWindow := func(byteCount int, digest string) error {
		if byteCount == 0 {
			return nil
		}
		if err := s.cfg.Store.RecordChunkEvent(ctx, task.ID, byteCount, digest); err != nil {
			cancelExecute()
			// chunk 元数据无法持久化: 复用 Worker 会让下一任务的 chunk 序列
			// 出现缺口, 销毁重建(审查: 失败后不复用)。
			s.evictWorkerAfterFailure(task.SessionKey)
			_ = s.finalizeOrFail(ctx, task, domain.TaskFailed, domain.DeliveryTaskFailed,
				"CHUNK_EVENT_FAILED", err.Error(), "")
			_ = s.KickSession(ctx, task.SessionKey)
			return fmt.Errorf("record chunk event: %w", err)
		}
		return nil
	}
	flushChunkWindow := func(batch *chunkEventBatcher) error {
		if byteCount, digest, ok := batch.Flush(); ok {
			return recordChunkWindow(byteCount, digest)
		}
		return nil
	}
	var (
		terminal   *workerv1.Terminal
		streamErr  error
		chunkBatch chunkEventBatcher
	)
	eventsOpen, errsOpen := true, true
	for eventsOpen || errsOpen {
		select {
		case <-executeCtx.Done():
			if !time.Now().Before(taskDeadline) {
				return s.finalizeTaskDeadline(ctx, task)
			}
			// round9 审查: 派发上下文被取消(heartbeat 检测到 lease 丢失/任务被
			// 恢复流程终态化/scheduler 关闭)时, Worker 可能仍在执行任务——先
			// 优雅 CancelWorker(cancelOnce 防重复), 再销毁 Worker 防脏复用;
			// 任务状态由恢复流程/新 owner 管理, 此处不终态化。
			cancelCtx, cancelRPC := context.WithTimeout(context.Background(), 5*time.Second)
			_ = s.CancelWorker(cancelCtx, task)
			cancelRPC()
			s.evictWorkerAfterFailure(task.SessionKey)
			return executeCtx.Err()
		case ev, ok := <-events:
			if !ok {
				eventsOpen = false
				events = nil
				continue
			}
			if ev.IsChunk() && ev.Chunk != nil {
				text := ev.Chunk.GetText()
				// Empty-text Chunk is a Worker heartbeat (see task_drain.HEARTBEAT_INTERVAL_S).
				// Flush any pending chunk metadata window, then refresh
				// last_activity_at without writing a synthetic chunk event.
				if text == "" {
					if err := flushChunkWindow(&chunkBatch); err != nil {
						return err
					}
					if err := s.cfg.Store.RecordHeartbeat(ctx, task.ID); err != nil {
						slog.WarnContext(ctx, "scheduler: record heartbeat failed",
							"task_id", task.ID, "error", err)
					}
					continue
				}
				if byteCount, digest, ok := chunkBatch.Add(text, time.Now()); ok {
					if err := recordChunkWindow(byteCount, digest); err != nil {
						return err
					}
				}
			}
			if ev.IsTerminal() {
				terminal = ev.Terminal
			}
		case err, ok := <-errs:
			if !ok {
				errsOpen = false
				errs = nil
				continue
			}
			if err != nil && streamErr == nil {
				streamErr = err
			}
		}
	}
	if err := flushChunkWindow(&chunkBatch); err != nil {
		return err
	}
	if !time.Now().Before(taskDeadline) {
		return s.finalizeTaskDeadline(ctx, task)
	}
	if streamErr != nil && terminal == nil {
		if errors.Is(streamErr, context.Canceled) {
			slog.InfoContext(ctx, "scheduler: stream cancelled by context",
				"task_id", task.ID)
			return nil
		}
		// 流错误: Worker 状态已推进但未提交, 销毁重建(审查: 失败后不复用)。
		s.evictWorkerAfterFailure(task.SessionKey)
		_ = s.finalizeOrFail(ctx, task, domain.TaskFailed, domain.DeliveryTaskFailed,
			"WORKER_STREAM_ERROR", streamErr.Error(), "")
		_ = s.KickSession(ctx, task.SessionKey)
		return streamErr
	}
	if terminal == nil {
		s.evictWorkerAfterFailure(task.SessionKey)
		_ = s.finalizeOrFail(ctx, task, domain.TaskFailed, domain.DeliveryTaskFailed,
			"MISSING_TERMINAL", "worker stream ended without terminal", "")
		_ = s.KickSession(ctx, task.SessionKey)
		return fmt.Errorf("missing terminal")
	}

	current, err := s.cfg.Store.GetTask(ctx, task.ID)
	if err != nil {
		// 审查 R4-C2: 任务已执行完毕但终态读失败——Worker 内存状态已推进,
		// 销毁防脏复用, 尽力终态化由恢复路径兜底。
		s.evictWorkerAfterFailure(task.SessionKey)
		_ = s.finalizeOrFail(ctx, task, domain.TaskInterrupted, domain.DeliveryTaskInterrupted,
			"TASK_STATE_READ_FAILED", err.Error(), "")
		_ = s.KickSession(ctx, task.SessionKey)
		return err
	}
	if current.CancelRequestedAt != nil {
		s.evictWorkerAfterFailure(task.SessionKey)
		_, err := s.cfg.Store.CompleteFailedTerminal(ctx, task.ID, s.cfg.PlatformInstanceID, domain.TaskInterrupted, domain.DeliveryTaskInterrupted,
			"TASK_INTERRUPTED", "task interrupted after accepted cancellation", "")
		_ = s.KickSession(ctx, task.SessionKey)
		return err
	}
	if !time.Now().Before(taskDeadline) {
		return s.finalizeTaskDeadline(ctx, task)
	}
	switch terminal.GetStatus() {
	case workerv1.TerminalStatus_TASK_SUCCEEDED:
		return s.completeSuccess(ctx, task, terminal)
	case workerv1.TerminalStatus_TASK_CANCELLED:
		s.evictWorkerAfterFailure(task.SessionKey)
		_ = s.finalizeOrFail(ctx, task, domain.TaskCancelled, domain.DeliveryTaskCancelled,
			"TASK_CANCELLED", boundMsg(terminal.GetUserMessage()), terminal.GetError().GetTraceId())
	case workerv1.TerminalStatus_TASK_INTERRUPTED:
		s.evictWorkerAfterFailure(task.SessionKey)
		_ = s.finalizeOrFail(ctx, task, domain.TaskInterrupted, domain.DeliveryTaskInterrupted,
			"TASK_INTERRUPTED", boundMsg(terminal.GetUserMessage()), terminal.GetError().GetTraceId())
	default:
		code := "TASK_FAILED"
		if terminal.GetError() != nil && terminal.GetError().GetCode() != "" {
			code = terminal.GetError().GetCode()
		}
		s.evictWorkerAfterFailure(task.SessionKey)
		_ = s.finalizeOrFail(ctx, task, domain.TaskFailed, domain.DeliveryTaskFailed,
			code, boundMsg(terminal.GetUserMessage()), terminal.GetError().GetTraceId())
	}
	_ = s.KickSession(ctx, task.SessionKey)
	return nil
}

// revokeSessionCredentialsIfTerminal 在任务终态后立即撤销该任务实际使用
// 的 credential 集的全部 JTI(审查 I9)。set 在 ensureWorker 成功后立即捕获
// (覆盖 StartSession/Policy 等早期终态路径), 不读 entry 当前集合, 避免误
// 撤销新任务刷新后的凭证。撤销失败时入 pendingRevocations 由后续
// prepareWorkerEntry/cleanup 重试, 不静默丢弃。
func (s *scheduler) revokeSessionCredentialsIfTerminal(ctx context.Context, taskID, sessionKey string, set workerCredentialSet) {
	latest, err := s.cfg.Store.GetTask(ctx, taskID)
	if err != nil || !latest.Status.IsTerminal() {
		return
	}
	if s.cfg.CapabilityStore == nil || len(set.JTIs) == 0 {
		return
	}
	revokeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), credentialRevokeTimeout)
	defer cancel()
	if err := s.revokeCredentialSet(revokeCtx, set); err != nil {
		slog.WarnContext(ctx, "scheduler: capability revocation failed; queued for retry",
			"task_id", taskID, "count", len(set.JTIs), "error", err)
		s.mu.Lock()
		entry := s.workers[sessionKey]
		s.mu.Unlock()
		if entry != nil {
			entry.lifecycleMu.Lock()
			if s.workerEntryIsCurrent(sessionKey, entry) {
				entry.pendingRevocations = append(entry.pendingRevocations, set)
			}
			entry.lifecycleMu.Unlock()
		}
	}
}

// firstJTI 返回凭证集首个 JTI(空集返回空串), 用于 TaskEnvelope 的
// capability_jti 绑定(方案 §7; Worker 侧 generation 校验为主)。
func firstJTI(set workerCredentialSet) string {
	if len(set.JTIs) == 0 {
		return ""
	}
	return set.JTIs[0]
}
