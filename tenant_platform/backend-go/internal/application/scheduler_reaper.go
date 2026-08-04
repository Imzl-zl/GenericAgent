package application

import (
	"context"
	"log/slog"
	"time"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/domain"
)

func (s *scheduler) cleanupExpiredCapabilityRevocations(ctx context.Context, now time.Time) error {
	interval := s.cfg.RevocationCleanupInterval
	if s.cfg.CapabilityStore == nil || interval <= 0 {
		return nil
	}
	now = now.UTC()
	if !s.lastRevocationCleanup.IsZero() && now.Before(s.lastRevocationCleanup.Add(interval)) {
		return nil
	}
	if _, err := s.cfg.CapabilityStore.DeleteExpiredCapabilityRevocations(ctx, now); err != nil {
		return err
	}
	s.lastRevocationCleanup = now
	return nil
}

// reapIdleTasks finalizes running tasks whose last_activity_at is older than
// now-idle. This is the "Worker alive but deadlocked" detector (Temporal
// HeartbeatTimeout pattern). Legitimate long tasks keep last_activity_at fresh
// via windowed RecordChunkEvent writes and RecordHeartbeat (drain poll), so
// they are NOT reaped. Only called when SchedulerConfig.IdleTimeout > 0.
// 审查 C1/I8: Worker 心跳只在推进窗口内发出(agent 最近有 display 事件),
// agent 卡死时心跳停发 → last_activity_at 陈旧 → 本函数按 idle 阈值收割。
func (s *scheduler) reapIdleTasks(ctx context.Context, owned []domain.Task, idle time.Duration) error {
	cutoff := time.Now().UTC().Add(-idle)
	for _, t := range owned {
		if !shouldReapIdleTask(t, cutoff) {
			continue
		}
		slog.ErrorContext(ctx, "scheduler: reaping idle task (Worker alive but deadlocked)",
			"task_id", t.ID,
			"session_key", t.SessionKey,
			"last_activity_at", formatActivityTime(t.LastActivityAt, t.WorkerDispatchStartedAt),
			"idle_threshold_seconds", int(idle.Seconds()))
		// 先取消 Worker 再终态化(审查): 否则串行槽被释放后下一任务即可
		// claim, 而旧任务仍在 Worker 内执行/写 memory/temp, 违反同一
		// runner_key 串行不变量。
		cancelCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		if err := s.CancelWorker(cancelCtx, t); err != nil {
			slog.WarnContext(ctx, "scheduler: idle reap cancel failed", "task_id", t.ID, "error", err)
		}
		cancel()
		_ = s.finalizeOrFail(ctx, t, domain.TaskFailed, domain.DeliveryTaskFailed,
			"WORKER_IDLE", "Worker heartbeat went silent; possible deadlock or hung I/O", "")
		// 审查: idle 任务被收割后 Worker 内存状态未提交, 不复用——销毁重建,
		// 否则下一任务会继承未提交的 history/working 或与仍在运行的旧任务重叠。
		s.evictWorkerAfterFailure(t.SessionKey)
		_ = s.KickSession(ctx, t.SessionKey)
	}
	return nil
}

// shouldReapIdleTask returns true when a running task is considered idle
// beyond the cutoff and should be reaped.
//
// Cold-start path (LastActivityAt is zero): uses WorkerDispatchStartedAt as
// the initial activity baseline (Temporal HeartbeatTimeout pattern: workflow
// start time is the initial heartbeat). If dispatch happened within the idle
// window, the Worker gets grace for its first LLM call. If dispatch happened
// > idle ago with zero activity, the Worker is truly stuck (e.g. GIL deadlock,
// internal infinite loop before first chunk) — the http.Client timeout and
// max_turns cannot catch a hang that never reaches the LLM call.
func shouldReapIdleTask(t domain.Task, cutoff time.Time) bool {
	if t.Status != domain.TaskRunning {
		return false
	}
	if t.LastActivityAt.IsZero() {
		if t.WorkerDispatchStartedAt == nil {
			return false
		}
		return !t.WorkerDispatchStartedAt.After(cutoff)
	}
	return !t.LastActivityAt.After(cutoff)
}

// formatActivityTime renders the activity timestamp for logging. When
// LastActivityAt is zero (cold-started task reaped via dispatch-time
// baseline), falls back to WorkerDispatchStartedAt so the log shows a
// meaningful time instead of the zero value.
func formatActivityTime(lastActivity time.Time, dispatchStarted *time.Time) string {
	if !lastActivity.IsZero() {
		return lastActivity.UTC().Format(time.RFC3339)
	}
	if dispatchStarted != nil {
		return "dispatch:" + dispatchStarted.UTC().Format(time.RFC3339)
	}
	return "unknown"
}

// evictIdleWorkers tears down resident Worker processes whose session has no
// active owned task and whose lastUsedAt is older than WorkerIdleTTL. This is
// the WORKER_IDLE_TIMEOUT behavior from architecture §8.3: idle Workers hold
// memory (Python process + GA history) but no model concurrency, so they are
// reclaimed after a grace window. Session continuity is preserved because the
// next task cold-starts a Worker from the last committed snapshot
// (StartSessionRequest.SnapshotId).
//
// Safety: sessions present in `owned` (starting/running on this instance) are
// never evicted. The lastUsedAt freshness check additionally protects the
// microsecond window between ensureWorker and MarkDispatchStarted in dispatch,
// since TTL is minutes while that window is not.
func (s *scheduler) evictIdleWorkers(owned []domain.Task) {
	if s.cfg.WorkerIdleTTL <= 0 {
		return
	}
	cutoff := time.Now().UTC().Add(-s.cfg.WorkerIdleTTL)
	active := make(map[string]struct{}, len(owned))
	for _, task := range owned {
		active[task.SessionKey] = struct{}{}
	}
	s.mu.Lock()
	candidates := make([]*workerEntry, 0, len(s.workers))
	for sessionKey, entry := range s.workers {
		if _, busy := active[sessionKey]; !busy {
			candidates = append(candidates, entry)
		}
	}
	s.mu.Unlock()

	for _, entry := range candidates {
		entry.lifecycleMu.Lock()
		if !s.workerEntryIsCurrent(entry.sessionKey, entry) || entry.lastUsedAt.After(cutoff) {
			entry.lifecycleMu.Unlock()
			continue
		}
		s.removeWorkerEntry(entry.sessionKey, entry)
		slog.Info("scheduler: evicting idle worker",
			"session_key", entry.sessionKey,
			"worker_instance_id", entry.instID,
			"idle_ttl_seconds", int(s.cfg.WorkerIdleTTL.Seconds()),
			"last_used_at", entry.lastUsedAt.UTC().Format(time.RFC3339))
		s.cleanupWorkerEntryBestEffort(context.Background(), entry)
		entry.lifecycleMu.Unlock()
	}
}
