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

// finalizeIntent 记录一次未能落库的终态化意图(round12 审查 I2): dispatch/
// reaper 等路径终态化写库失败时, 由 tick 的 drainPendingFinalize 每轮重试,
// 任务不会因瞬时 DB 错误永久卡在 starting/running。
type finalizeIntent struct {
	status       domain.TaskStatus
	deliveryType domain.DeliveryType
	code         string
	message      string
	traceID      string
}

// terminateTask 是任务非成功终态收尾的唯一入口(round13 L1 根本性收拢):
// 按固定顺序完成 Worker 销毁(幂等) + 终态化(失败自动注册 pendingFinalize
// 重试) + 会话唤醒。所有非成功终态路径必须经本方法收尾——"终态即销毁"
// 从 20+ 处手写三件套收敛为单一定义, 新分支漏销毁变为不可能。
//
// 顺序约束: destroy 先于 finalize——终态提交前资源(容器/lease/凭据)已释放,
// 同 session 下一任务只能在终态提交后 claim, 替换窗口从结构上不可能出现。
//
// 例外: completeSuccess 成功路径必须两段式(captureTaskDeliverableFiles 依赖
// Worker 存活), 且其失败分支持 entry.lifecycleMu 不可经本方法(锁重入),
// 保持显式 destroyTaskWorkerLocked + finalizeOrFail(顺序同样 destroy 先行)。
func (s *scheduler) terminateTask(ctx context.Context, task domain.Task, status domain.TaskStatus, deliveryType domain.DeliveryType, code, message, traceID string) domain.Task {
	s.destroyTaskWorker(task.SessionKey)
	t := s.finalizeOrFail(ctx, task, status, deliveryType, code, message, traceID)
	_ = s.KickSession(ctx, task.SessionKey)
	return t
}

// finalizeOrFail records a terminal task state + delivery and surfaces any
// persistence failure via log instead of silently dropping it (global rule:
// No Silent Fallbacks). The returned task is the updated row on success, or
// the original task on failure so callers can continue without a panic.
// 审查 R5-Critical-2: 终态化必须由当前 claim owner 在 lease 有效期内执行——
// lease 已被接管/过期时(ErrTaskNotOwned)任务由 RecoverAfterRestart 或新
// owner 处理, 此处仅记 Warn 不得覆盖其状态。
// round12 审查(I2): 非 ErrTaskNotOwned 的写库失败注册到 pendingFinalize,
// 由 tick 每轮重试直至成功——任务不会再因 dispatch 退出而永久卡住。
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
		slog.ErrorContext(ctx, "scheduler: CompleteFailedTerminal failed; scheduling retry",
			"task_id", task.ID,
			"session_key", task.SessionKey,
			"target_status", string(status),
			"error", err)
		s.pendingFinalize.Store(task.ID, finalizeIntent{
			status:       status,
			deliveryType: deliveryType,
			code:         code,
			message:      domain.TruncateUTF8(message, domain.MaxTerminalErrorBytes),
			traceID:      traceID,
		})
		return task
	}
	return t
}

// drainPendingFinalize 每 tick 重试未落库的终态化意图(round12 审查 I2):
// 任务已终态/claim 已不属于本实例 → 删除意图(他方已处理);
// CompleteFailedTerminal 成功或 ErrTaskNotOwned → 删除意图;
// 其余 DB 错误保留, 下轮重试。必须在 tick 的 claim heartbeat 之后调用
// (重试依赖有效 lease)。
func (s *scheduler) drainPendingFinalize(ctx context.Context) {
	var pending []string
	s.pendingFinalize.Range(func(key, _ any) bool {
		pending = append(pending, key.(string))
		return true
	})
	for _, taskID := range pending {
		v, ok := s.pendingFinalize.Load(taskID)
		if !ok {
			continue
		}
		intent := v.(finalizeIntent)
		latest, err := s.cfg.Store.GetTask(ctx, taskID)
		if err != nil {
			// DB 不可用: 下轮重试。
			continue
		}
		if latest.Status.IsTerminal() || latest.ClaimOwner != s.cfg.PlatformInstanceID {
			s.pendingFinalize.Delete(taskID)
			continue
		}
		if _, err := s.cfg.Store.CompleteFailedTerminal(ctx, taskID, s.cfg.PlatformInstanceID,
			intent.status, intent.deliveryType, intent.code, intent.message, intent.traceID); err != nil {
			if errors.Is(err, postgres.ErrTaskNotOwned) {
				s.pendingFinalize.Delete(taskID)
			}
			continue
		}
		s.pendingFinalize.Delete(taskID)
	}
}

func (s *scheduler) finalizeTaskDeadline(ctx context.Context, task domain.Task) error {
	cancelCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.CancelWorker(cancelCtx, task); err != nil {
		slog.WarnContext(ctx, "scheduler: deadline cancel failed", "task_id", task.ID, "error", err)
	}
	// 超时后 Worker 内存状态不确定, 不复用(审查: 失败后不复用)。
	s.terminateTask(ctx, task, domain.TaskFailed, domain.DeliveryTaskFailed,
		"TASK_DEADLINE_EXCEEDED", "task exceeded maximum wall-clock duration", "")
	return context.DeadlineExceeded
}

// toTaskMedia 把路由时 ImportInbound 的附件清单(SessionFileRef, 含
// workspace 相对路径/别名)转为任务媒体清单(2026-08-13 多模态链路定案)。
// 仅入站媒体参与(模型首轮可感知); 出站文件(outputs/)是工具产物, 不注入。
func toTaskMedia(refs []SessionFileRef) []domain.TaskMedia {
	if len(refs) == 0 {
		return nil
	}
	out := make([]domain.TaskMedia, 0, len(refs))
	for _, ref := range refs {
		if ref.Direction != "inbound" {
			continue
		}
		out = append(out, domain.TaskMedia{
			Alias:        ref.Alias,
			OriginalName: ref.OriginalName,
			RelativePath: ref.RelativePath,
			SizeBytes:    ref.SizeBytes,
			// 2026-08-14 审查 I1: 渠道侧准确 MIME(webhook media_items, 飞书
			// image 等无扩展名场景)随任务媒体清单透传——此前在此丢失, 契约
			// 字段空转, Phase C 视频抽帧分派失效。
			ContentType: ref.ContentType,
		})
	}
	return out
}

func workerTaskEnvelope(task domain.Task, runnerGeneration uint64, capabilityJTI string) *workerv1.TaskEnvelope {
	env := &workerv1.TaskEnvelope{
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
	// 2026-08-13 多模态链路: 任务入站媒体结构化下发(tasks.media 持久化 →
	// TaskEnvelope.media 契约 → Worker put_task images)。
	for _, m := range task.Media {
		env.Media = append(env.Media, &workerv1.MediaItem{
			Alias:        m.Alias,
			OriginalName: m.OriginalName,
			RelativePath: m.RelativePath,
			SizeBytes:    m.SizeBytes,
			// 2026-08-14 审查 I1: content_type 随契约下发(与 toTaskMedia 同步补齐)。
			ContentType: m.ContentType,
		})
	}
	return env
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
			// round12 审查(I1 连带): 心跳 defer 先于本闭包执行并已取消
			// heartbeat.ctx, 直接用 ctx 终态化必然失败——panic 路径必须用
			// 独立有界上下文(与 heartbeat 丢失 fallback 同模式)。
			fbCtx, fbCancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer fbCancel()
			_ = s.terminateTask(fbCtx, task, domain.TaskFailed, domain.DeliveryTaskFailed,
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
	fresh, err := s.cfg.Store.WorkspaceIsFresh(ctx, task.SessionKey, task.ConversationKey)
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
				_ = s.terminateTask(fbCtx, latest, domain.TaskInterrupted, domain.DeliveryTaskInterrupted,
					"CLAIM_LEASE_LOST", err.Error(), "")
			}
			returnErr = fmt.Errorf("claim heartbeat: %w", err)
		}
	}()
	ctx = heartbeat.ctx

	client, entry, err := s.createTaskWorker(ctx, task)
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
		_ = s.terminateTask(ctx, task, domain.TaskFailed, domain.DeliveryTaskFailed,
			"WORKER_START_FAILED", err.Error(), "")
		return err
	}
	s.auditRoutingBinding(ctx, task, entry, "success", "")
	// 捕获本任务使用的 credential 集(撤销 defer 使用; 审查: 必须在任何
	// 可能终态化的分支之前捕获——StartSession/Policy/MarkRunning 失败等
	// 早期终态路径同样需要撤销已签发的 token)。任务执行期间 Worker 拒绝
	// reload, entry.credentials 在 ExecuteTask 期间不会变化。
	taskCredentialSet = entry.credentials
	// round12 审查(I1): dispatch 是任务 Worker 的唯一 owner——createTaskWorker
	// 成功后立即注册统一 teardown, 任何退出路径(含 panic/并发终态/策略失败/
	// startSession 错误/无 coordinator)恒销毁 Worker。身份校验变体保证旧任务
	// 收尾不会误毁同 session 新任务的进程(下一任务只能在上一任务终态后 claim)。
	defer s.destroyTaskWorkerEntry(task.SessionKey, entry)
	// round11 审查(C3): 移除全局 workerCallMu。StartSession 与 CancelTask 的
	// 互斥由 per-entry lifecycleMu 提供(同 session), 跨 session 完全并行;
	// 一个卡住的 StartSession 不再阻塞其他工作区的派发与取消。MarkDispatch
	// Started/MarkRunning 是 DB 原子操作, 无需进程内全局串行。
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
		s.destroyTaskWorker(task.SessionKey)
		return nil
	}
	// 注意: 任务签发的 JTI 已在 issueProviderCapabilitiesWithRuntime 内、
	// token 暴露给 Runner 之前持久化(审查 F1), 此处不再重复写入。

	if _, err := s.cfg.Registry.Resolve(CapabilityVersion, task.ToolPolicyVersion); err != nil {
		_ = s.terminateTask(ctx, task, domain.TaskFailed, domain.DeliveryTaskFailed,
			"POLICY_RESOLVE_FAILED", err.Error(), "")
		return err
	}

	// StartSession happens after MarkDispatchStarted so the durable cancel path
	// can observe and record CancelRequestedAt while StartSession is in flight.
	if err := s.startSessionOnWorker(ctx, task); err != nil {
		_ = s.terminateTask(ctx, task, domain.TaskFailed, domain.DeliveryTaskFailed,
			"WORKER_START_FAILED", err.Error(), "")
		return err
	}

	if _, err := s.cfg.Store.MarkRunning(ctx, task.ID, s.cfg.PlatformInstanceID); err != nil {
		// 审查 R4-C2: dispatch 无法继续且任务未终态。销毁本地 Worker 防脏
		// 复用(任务可能已被接管/终态化, 本地进程不得留给下一任务), 并尽力
		// 终态化防任务卡死; 失败时由 lease 过期恢复路径兜底, 无副作用。
		_ = s.terminateTask(ctx, task, domain.TaskInterrupted, domain.DeliveryTaskInterrupted,
			"MARK_RUNNING_FAILED", err.Error(), "")
		return err
	}

	// Round-trip durable envelope from PostgreSQL (never scheduler memory).
	taskRow, err := s.cfg.Store.GetTask(ctx, task.ID)
	if err != nil {
		// 审查 R4-C2: 同 MarkRunning 失败路径——销毁 Worker + 尽力终态化。
		_ = s.terminateTask(ctx, task, domain.TaskInterrupted, domain.DeliveryTaskInterrupted,
			"TASK_STATE_READ_FAILED", err.Error(), "")
		return err
	}
	if taskRow.Status.IsTerminal() {
		return nil
	}
	if taskRow.CancelRequestedAt != nil {
		// 审查: StartSession 已执行但任务在 ExecuteTask 前被取消, Worker
		// 内存状态未提交, 销毁重建而非复用。
		_ = s.terminateTask(ctx, task, domain.TaskInterrupted, domain.DeliveryTaskInterrupted,
			"TASK_INTERRUPTED", "task interrupted before worker execution", "")
		return err
	}
	req := &workerv1.ExecuteTaskRequest{Task: workerTaskEnvelope(taskRow, entry.runnerGeneration, controlJTIFor(entry.credentials))}

	taskDeadline := time.Now().Add(s.cfg.MaxTaskWallClock)
	executeCtx, cancelExecute := context.WithTimeout(ctx, s.cfg.MaxTaskWallClock)
	defer cancelExecute()
	// round11 审查(C3): 标记执行中, CancelWorker 据此决定是否发取消 RPC;
	// dispatch 结束(任意路径)后复位。
	entry.executing.Store(true)
	defer entry.executing.Store(false)
	events, errs := client.ExecuteTask(executeCtx, req)
	recordChunkWindow := func(byteCount int, digest string) error {
		if byteCount == 0 {
			return nil
		}
		if err := s.cfg.Store.RecordChunkEvent(ctx, task.ID, byteCount, digest); err != nil {
			cancelExecute()
			// chunk 元数据无法持久化: 复用 Worker 会让下一任务的 chunk 序列
			// 出现缺口, 销毁重建(审查: 失败后不复用)。
			_ = s.terminateTask(ctx, task, domain.TaskFailed, domain.DeliveryTaskFailed,
				"CHUNK_EVENT_FAILED", err.Error(), "")
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
	// IM 流式转发(IM_STREAMING_DELIVERY §4.2): per-task 缓冲 + 500ms 节流
	// 合并 + 终态 commit/abort。committed 防止失败路径 defer abort 误伤
	// 已 commit 的流。
	forwarder := NewStreamForwarder(s.cfg.Streaming, s.cfg.Bots, s.cfg.RuntimeSettings, taskRow)
	streamCommitted := false
	defer func() {
		if !streamCommitted {
			forwarder.Abort(context.WithoutCancel(ctx))
		}
	}()
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
			s.destroyTaskWorker(task.SessionKey)
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
					// 心跳同时驱动流式缓冲窗口到期 flush(低频 chunk 场景下
					// 文本不会滞留到 Terminal)。
					forwarder.FlushDue(ctx, time.Now())
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
				// 流式转发: 非空 chunk 进 per-task 缓冲(节流合并; 流式片段不写
				// messages 审计, 只记终态——与现 delivery 一致)。
				forwarder.AppendText(ctx, text, time.Now())
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
		_ = s.terminateTask(ctx, task, domain.TaskFailed, domain.DeliveryTaskFailed,
			"WORKER_STREAM_ERROR", streamErr.Error(), "")
		return streamErr
	}
	if terminal == nil {
		_ = s.terminateTask(ctx, task, domain.TaskFailed, domain.DeliveryTaskFailed,
			"MISSING_TERMINAL", "worker stream ended without terminal", "")
		return fmt.Errorf("missing terminal")
	}

	current, err := s.cfg.Store.GetTask(ctx, task.ID)
	if err != nil {
		// 审查 R4-C2: 任务已执行完毕但终态读失败——Worker 内存状态已推进,
		// 销毁防脏复用, 尽力终态化由恢复路径兜底。
		_ = s.terminateTask(ctx, task, domain.TaskInterrupted, domain.DeliveryTaskInterrupted,
			"TASK_STATE_READ_FAILED", err.Error(), "")
		return err
	}
	if current.CancelRequestedAt != nil {
		_ = s.terminateTask(ctx, task, domain.TaskInterrupted, domain.DeliveryTaskInterrupted,
			"TASK_INTERRUPTED", "task interrupted after accepted cancellation", "")
		return err
	}
	if !time.Now().Before(taskDeadline) {
		return s.finalizeTaskDeadline(ctx, task)
	}
	switch terminal.GetStatus() {
	case workerv1.TerminalStatus_TASK_SUCCEEDED:
		// 终态 commit: 流式消息即最终交付(打字机最后一次更新/QQ 流式收尾);
		// commit 成功置位 stream_final_at 抑制 delivery 文本 part(文件照发);
		// 失败/未 open → 无标记, delivery 兜底补发最终结果。
		if forwarder.Commit(ctx, time.Now()) {
			if _, err := s.cfg.Store.MarkTaskStreamFinal(ctx, task.ID); err != nil {
				slog.WarnContext(ctx, "scheduler: mark stream final failed; delivery will resend text",
					"task_id", task.ID, "error", err)
			}
			streamCommitted = true
		}
		return s.completeSuccess(ctx, task, terminal)
	case workerv1.TerminalStatus_TASK_CANCELLED:
		_ = s.terminateTask(ctx, task, domain.TaskCancelled, domain.DeliveryTaskCancelled,
			"TASK_CANCELLED", domain.TruncateUTF8(terminal.GetUserMessage(), domain.MaxTerminalErrorBytes), terminal.GetError().GetTraceId())
	case workerv1.TerminalStatus_TASK_INTERRUPTED:
		_ = s.terminateTask(ctx, task, domain.TaskInterrupted, domain.DeliveryTaskInterrupted,
			"TASK_INTERRUPTED", domain.TruncateUTF8(terminal.GetUserMessage(), domain.MaxTerminalErrorBytes), terminal.GetError().GetTraceId())
	default:
		code := "TASK_FAILED"
		if terminal.GetError() != nil && terminal.GetError().GetCode() != "" {
			code = terminal.GetError().GetCode()
		}
		_ = s.terminateTask(ctx, task, domain.TaskFailed, domain.DeliveryTaskFailed,
			code, domain.TruncateUTF8(terminal.GetUserMessage(), domain.MaxTerminalErrorBytes), terminal.GetError().GetTraceId())
	}
	return nil
}

// revokeSessionCredentialsIfTerminal 在任务终态后立即撤销该任务实际使用
// 的 credential 集的全部 JTI(审查 I9)。set 在 createTaskWorker 成功后立即捕获
// (覆盖 StartSession/Policy 等早期终态路径), 不读 entry 当前集合, 避免误
// 撤销新任务轮换后的凭证。撤销失败记录日志——恢复路径
// (RecoverAfterRestart 按 tasks.capability_jtis)与 TTL 过期兜底, 不静默丢弃。
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
		slog.WarnContext(ctx, "scheduler: capability revocation failed",
			"task_id", taskID, "count", len(set.JTIs), "error", err)
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

// controlJTIPrefix 标记 control capability 的 JTI 值(round11 审查 I4):
// Worker 只持有 JTI 值(无完整 JWT, 无法解析 claims), 用前缀区分用途;
// 真实性仍由凭据集合成员检查保证。
// 契约约定(跨语言单一真值源): worker.proto TaskEnvelope.capability_jti。
const controlJTIPrefix = "ctrl:"

// controlJTIFor 返回控制 RPC 使用的 capability JTI(round11 审查 I4):
// 优先独立签发的 control token; loopback/测试无 control token 时回退
// firstJTI(空凭据集下为空, Worker 端空 JTI 仅限无凭据清理路径)。
func controlJTIFor(set workerCredentialSet) string {
	if set.ControlJTI != "" {
		return set.ControlJTI
	}
	return firstJTI(set)
}
