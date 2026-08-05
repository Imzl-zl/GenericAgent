import io

path = "tenant_platform/backend-go/internal/application/scheduler_dispatch.go"
src = open(path, encoding="utf-8").read()

def rep(old, new):
    global src
    assert src.count(old) == 1, "not unique/found: " + old[:60]
    src = src.replace(old, new)

# 1. deadline
rep("""	s.destroyTaskWorker(task.SessionKey)
	s.finalizeOrFail(ctx, task, domain.TaskFailed, domain.DeliveryTaskFailed,
		"TASK_DEADLINE_EXCEEDED", "task exceeded maximum wall-clock duration", "")
	_ = s.KickSession(ctx, task.SessionKey)
	return context.DeadlineExceeded""",
"""	s.terminateTask(ctx, task, domain.TaskFailed, domain.DeliveryTaskFailed,
		"TASK_DEADLINE_EXCEEDED", "task exceeded maximum wall-clock duration", "")
	return context.DeadlineExceeded""")

# 2. panic
rep("""			fbCtx, fbCancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer fbCancel()
			_ = s.finalizeOrFail(fbCtx, task, domain.TaskFailed, domain.DeliveryTaskFailed,
				"DISPATCH_PANIC", fmt.Sprintf("%v", r), "")""",
"""			fbCtx, fbCancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer fbCancel()
			_ = s.terminateTask(fbCtx, task, domain.TaskFailed, domain.DeliveryTaskFailed,
				"DISPATCH_PANIC", fmt.Sprintf("%v", r), "")""")

# 3. heartbeat fallback
rep("""			if getErr == nil && !latest.Status.IsTerminal() {
				_ = s.CancelWorker(fbCtx, latest)
				s.destroyTaskWorker(task.SessionKey)
				_ = s.finalizeOrFail(fbCtx, latest, domain.TaskInterrupted, domain.DeliveryTaskInterrupted,
					"CLAIM_LEASE_LOST", err.Error(), "")
				_ = s.KickSession(fbCtx, task.SessionKey)
			}""",
"""			if getErr == nil && !latest.Status.IsTerminal() {
				_ = s.CancelWorker(fbCtx, latest)
				_ = s.terminateTask(fbCtx, latest, domain.TaskInterrupted, domain.DeliveryTaskInterrupted,
					"CLAIM_LEASE_LOST", err.Error(), "")
			}""")

# 4. policy
rep("""	if _, err := s.cfg.Registry.Resolve(CapabilityVersion, task.ToolPolicyVersion); err != nil {
		_ = s.finalizeOrFail(ctx, task, domain.TaskFailed, domain.DeliveryTaskFailed,
			"POLICY_RESOLVE_FAILED", err.Error(), "")
		return err
	}""",
"""	if _, err := s.cfg.Registry.Resolve(CapabilityVersion, task.ToolPolicyVersion); err != nil {
		_ = s.terminateTask(ctx, task, domain.TaskFailed, domain.DeliveryTaskFailed,
			"POLICY_RESOLVE_FAILED", err.Error(), "")
		return err
	}""")

# 5. startSession
rep("""	if err := s.startSessionOnWorker(ctx, task); err != nil {
		_ = s.finalizeOrFail(ctx, task, domain.TaskFailed, domain.DeliveryTaskFailed,
			"WORKER_START_FAILED", err.Error(), "")
		_ = s.KickSession(ctx, task.SessionKey)
		return err
	}""",
"""	if err := s.startSessionOnWorker(ctx, task); err != nil {
		_ = s.terminateTask(ctx, task, domain.TaskFailed, domain.DeliveryTaskFailed,
			"WORKER_START_FAILED", err.Error(), "")
		return err
	}""")

# 6. MarkRunning
rep("""		s.destroyTaskWorker(task.SessionKey)
		_ = s.finalizeOrFail(ctx, task, domain.TaskInterrupted, domain.DeliveryTaskInterrupted,
			"MARK_RUNNING_FAILED", err.Error(), "")
		_ = s.KickSession(ctx, task.SessionKey)
		return err""",
"""		_ = s.terminateTask(ctx, task, domain.TaskInterrupted, domain.DeliveryTaskInterrupted,
			"MARK_RUNNING_FAILED", err.Error(), "")
		return err""")

# 7. TASK_STATE_READ_FAILED #1
rep("""		s.destroyTaskWorker(task.SessionKey)
		_ = s.finalizeOrFail(ctx, task, domain.TaskInterrupted, domain.DeliveryTaskInterrupted,
			"TASK_STATE_READ_FAILED", err.Error(), "")
		_ = s.KickSession(ctx, task.SessionKey)
		return err
	}
	if taskRow.Status.IsTerminal() {""",
"""		_ = s.terminateTask(ctx, task, domain.TaskInterrupted, domain.DeliveryTaskInterrupted,
			"TASK_STATE_READ_FAILED", err.Error(), "")
		return err
	}
	if taskRow.Status.IsTerminal() {""")

# 8. CancelRequestedAt #1
rep("""		s.destroyTaskWorker(task.SessionKey)
		_, err := s.cfg.Store.CompleteFailedTerminal(ctx, task.ID, s.cfg.PlatformInstanceID, domain.TaskInterrupted, domain.DeliveryTaskInterrupted,
			"TASK_INTERRUPTED", "task interrupted before worker execution", "")
		_ = s.KickSession(ctx, task.SessionKey)
		return err""",
"""		_ = s.terminateTask(ctx, task, domain.TaskInterrupted, domain.DeliveryTaskInterrupted,
			"TASK_INTERRUPTED", "task interrupted before worker execution", "")
		return err""")

# 9. CHUNK_EVENT_FAILED
rep("""			s.destroyTaskWorker(task.SessionKey)
			_ = s.finalizeOrFail(ctx, task, domain.TaskFailed, domain.DeliveryTaskFailed,
				"CHUNK_EVENT_FAILED", err.Error(), "")
			_ = s.KickSession(ctx, task.SessionKey)
			return fmt.Errorf("record chunk event: %w", err)""",
"""			_ = s.terminateTask(ctx, task, domain.TaskFailed, domain.DeliveryTaskFailed,
				"CHUNK_EVENT_FAILED", err.Error(), "")
			return fmt.Errorf("record chunk event: %w", err)""")

# 10. streamErr
rep("""		s.destroyTaskWorker(task.SessionKey)
		_ = s.finalizeOrFail(ctx, task, domain.TaskFailed, domain.DeliveryTaskFailed,
			"WORKER_STREAM_ERROR", streamErr.Error(), "")
		_ = s.KickSession(ctx, task.SessionKey)
		return streamErr""",
"""		_ = s.terminateTask(ctx, task, domain.TaskFailed, domain.DeliveryTaskFailed,
			"WORKER_STREAM_ERROR", streamErr.Error(), "")
		return streamErr""")

# 11. MISSING_TERMINAL
rep("""		s.destroyTaskWorker(task.SessionKey)
		_ = s.finalizeOrFail(ctx, task, domain.TaskFailed, domain.DeliveryTaskFailed,
			"MISSING_TERMINAL", "worker stream ended without terminal", "")
		_ = s.KickSession(ctx, task.SessionKey)
		return fmt.Errorf("missing terminal")""",
"""		_ = s.terminateTask(ctx, task, domain.TaskFailed, domain.DeliveryTaskFailed,
			"MISSING_TERMINAL", "worker stream ended without terminal", "")
		return fmt.Errorf("missing terminal")""")

# 12. TASK_STATE_READ_FAILED #2
rep("""		s.destroyTaskWorker(task.SessionKey)
		_ = s.finalizeOrFail(ctx, task, domain.TaskInterrupted, domain.DeliveryTaskInterrupted,
			"TASK_STATE_READ_FAILED", err.Error(), "")
		_ = s.KickSession(ctx, task.SessionKey)
		return err
	}
	if current.CancelRequestedAt != nil {""",
"""		_ = s.terminateTask(ctx, task, domain.TaskInterrupted, domain.DeliveryTaskInterrupted,
			"TASK_STATE_READ_FAILED", err.Error(), "")
		return err
	}
	if current.CancelRequestedAt != nil {""")

# 13. CancelRequestedAt #2
rep("""		s.destroyTaskWorker(task.SessionKey)
		_, err := s.cfg.Store.CompleteFailedTerminal(ctx, task.ID, s.cfg.PlatformInstanceID, domain.TaskInterrupted, domain.DeliveryTaskInterrupted,
			"TASK_INTERRUPTED", "task interrupted after accepted cancellation", "")
		_ = s.KickSession(ctx, task.SessionKey)
		return err""",
"""		_ = s.terminateTask(ctx, task, domain.TaskInterrupted, domain.DeliveryTaskInterrupted,
			"TASK_INTERRUPTED", "task interrupted after accepted cancellation", "")
		return err""")

# 14. terminal switch
rep("""	case workerv1.TerminalStatus_TASK_CANCELLED:
		s.destroyTaskWorker(task.SessionKey)
		_ = s.finalizeOrFail(ctx, task, domain.TaskCancelled, domain.DeliveryTaskCancelled,
			"TASK_CANCELLED", boundMsg(terminal.GetUserMessage()), terminal.GetError().GetTraceId())
	case workerv1.TerminalStatus_TASK_INTERRUPTED:
		s.destroyTaskWorker(task.SessionKey)
		_ = s.finalizeOrFail(ctx, task, domain.TaskInterrupted, domain.DeliveryTaskInterrupted,
			"TASK_INTERRUPTED", boundMsg(terminal.GetUserMessage()), terminal.GetError().GetTraceId())
	default:
		code := "TASK_FAILED"
		if terminal.GetError() != nil && terminal.GetError().GetCode() != "" {
			code = terminal.GetError().GetCode()
		}
		s.destroyTaskWorker(task.SessionKey)
		_ = s.finalizeOrFail(ctx, task, domain.TaskFailed, domain.DeliveryTaskFailed,
			code, boundMsg(terminal.GetUserMessage()), terminal.GetError().GetTraceId())
	}
	_ = s.KickSession(ctx, task.SessionKey)
	return nil
}""",
"""	case workerv1.TerminalStatus_TASK_CANCELLED:
		_ = s.terminateTask(ctx, task, domain.TaskCancelled, domain.DeliveryTaskCancelled,
			"TASK_CANCELLED", boundMsg(terminal.GetUserMessage()), terminal.GetError().GetTraceId())
	case workerv1.TerminalStatus_TASK_INTERRUPTED:
		_ = s.terminateTask(ctx, task, domain.TaskInterrupted, domain.DeliveryTaskInterrupted,
			"TASK_INTERRUPTED", boundMsg(terminal.GetUserMessage()), terminal.GetError().GetTraceId())
	default:
		code := "TASK_FAILED"
		if terminal.GetError() != nil && terminal.GetError().GetCode() != "" {
			code = terminal.GetError().GetCode()
		}
		_ = s.terminateTask(ctx, task, domain.TaskFailed, domain.DeliveryTaskFailed,
			code, boundMsg(terminal.GetUserMessage()), terminal.GetError().GetTraceId())
	}
	return nil
}""")

open(path, "w", encoding="utf-8", newline="").write(src)
print("dispatch done")
