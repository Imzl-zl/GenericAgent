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
)

// finalizeOrFail records a terminal task state + delivery and surfaces any
// persistence failure via log instead of silently dropping it (global rule:
// No Silent Fallbacks). The returned task is the updated row on success, or
// the original task on failure so callers can continue without a panic.
func (s *scheduler) finalizeOrFail(ctx context.Context, task domain.Task, status domain.TaskStatus, deliveryType domain.DeliveryType, code, message, traceID string) domain.Task {
	t, err := s.cfg.Store.CompleteFailedTerminal(ctx, task.ID, status, deliveryType, code, message, traceID)
	if err != nil {
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
	s.finalizeOrFail(ctx, task, domain.TaskFailed, domain.DeliveryTaskFailed,
		"TASK_DEADLINE_EXCEEDED", "task exceeded maximum wall-clock duration", "")
	_ = s.KickSession(ctx, task.SessionKey)
	return context.DeadlineExceeded
}

func (s *scheduler) dispatch(ctx context.Context, task domain.Task) (returnErr error) {
	defer func() {
		if r := recover(); r != nil {
			returnErr = fmt.Errorf("dispatch panic: %v", r)
			slog.ErrorContext(ctx, "scheduler: dispatch panic",
				"task_id", task.ID, "panic", r)
			_ = s.finalizeOrFail(ctx, task, domain.TaskFailed, domain.DeliveryTaskFailed,
				"DISPATCH_PANIC", fmt.Sprintf("%v", r), "")
		}
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
			cancelCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			_ = s.CancelWorker(cancelCtx, task)
			cancel()
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
		s.auditRoutingBinding(ctx, task, entry, "error", "WORKER_CREDENTIAL_PREPARE_FAILED")
		_ = s.finalizeOrFail(ctx, task, domain.TaskFailed, domain.DeliveryTaskFailed,
			"WORKER_START_FAILED", err.Error(), "")
		_ = s.KickSession(ctx, task.SessionKey)
		return err
	}
	s.auditRoutingBinding(ctx, task, entry, "success", "")

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
	// instead of finalizing immediately.
	cur, err = s.cfg.Store.MarkDispatchStarted(ctx, task.ID, s.cfg.PlatformInstanceID, entry.instID)
	if err != nil {
		slog.ErrorContext(ctx, "scheduler: MarkDispatchStarted failed",
			"task_id", task.ID, "error", err)
		return nil
	}

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
		return err
	}

	// Round-trip durable envelope from PostgreSQL (never scheduler memory).
	taskRow, err := s.cfg.Store.GetTask(ctx, task.ID)
	if err != nil {
		return err
	}
	if taskRow.Status.IsTerminal() {
		return nil
	}
	if taskRow.CancelRequestedAt != nil {
		_, err := s.cfg.Store.CompleteFailedTerminal(ctx, task.ID, domain.TaskInterrupted, domain.DeliveryTaskInterrupted,
			"TASK_INTERRUPTED", "task interrupted before worker execution", "")
		_ = s.KickSession(ctx, task.SessionKey)
		return err
	}
	req := &workerv1.ExecuteTaskRequest{
		Task: &workerv1.TaskEnvelope{
			TaskId:            taskRow.ID,
			SessionKey:        taskRow.SessionKey,
			RequesterUserId:   taskRow.RequesterID,
			Source:            taskRow.Source,
			SourceInstanceId:  taskRow.SourceInstanceID,
			MessageId:         taskRow.MessageID,
			Prompt:            taskRow.Prompt,
			PersonaSnapshot:   append([]string(nil), taskRow.PersonaSnapshot...),
			ToolPolicyVersion: taskRow.ToolPolicyVersion,
			CreatedAt:         timestamppb.New(taskRow.CreatedAt),
		},
	}

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
		_ = s.finalizeOrFail(ctx, task, domain.TaskFailed, domain.DeliveryTaskFailed,
			"WORKER_STREAM_ERROR", streamErr.Error(), "")
		_ = s.KickSession(ctx, task.SessionKey)
		return streamErr
	}
	if terminal == nil {
		_ = s.finalizeOrFail(ctx, task, domain.TaskFailed, domain.DeliveryTaskFailed,
			"MISSING_TERMINAL", "worker stream ended without terminal", "")
		_ = s.KickSession(ctx, task.SessionKey)
		return fmt.Errorf("missing terminal")
	}

	current, err := s.cfg.Store.GetTask(ctx, task.ID)
	if err != nil {
		return err
	}
	if current.CancelRequestedAt != nil {
		_, err := s.cfg.Store.CompleteFailedTerminal(ctx, task.ID, domain.TaskInterrupted, domain.DeliveryTaskInterrupted,
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
		_ = s.finalizeOrFail(ctx, task, domain.TaskCancelled, domain.DeliveryTaskCancelled,
			"TASK_CANCELLED", boundMsg(terminal.GetUserMessage()), terminal.GetError().GetTraceId())
	case workerv1.TerminalStatus_TASK_INTERRUPTED:
		_ = s.finalizeOrFail(ctx, task, domain.TaskInterrupted, domain.DeliveryTaskInterrupted,
			"TASK_INTERRUPTED", boundMsg(terminal.GetUserMessage()), terminal.GetError().GetTraceId())
	default:
		code := "TASK_FAILED"
		if terminal.GetError() != nil && terminal.GetError().GetCode() != "" {
			code = terminal.GetError().GetCode()
		}
		_ = s.finalizeOrFail(ctx, task, domain.TaskFailed, domain.DeliveryTaskFailed,
			code, boundMsg(terminal.GetUserMessage()), terminal.GetError().GetTraceId())
	}
	_ = s.KickSession(ctx, task.SessionKey)
	return nil
}
