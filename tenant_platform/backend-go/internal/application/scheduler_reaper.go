package application

import (
	"context"
	"log/slog"
	"time"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/domain"
)

// reapIdleTasks finalizes running tasks whose last_activity_at is older than
// now-idle. This is the "Worker alive but deadlocked" detector (Temporal
// HeartbeatTimeout pattern). Legitimate long tasks keep last_activity_at fresh
// via RecordChunkEvent (each chunk) and RecordHeartbeat (drain poll), so they
// are NOT reaped. Only called when SchedulerConfig.IdleTimeout > 0.
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
		_ = s.finalizeOrFail(ctx, t, domain.TaskFailed, domain.DeliveryTaskFailed,
			"WORKER_IDLE", "Worker heartbeat went silent; possible deadlock or hung I/O", "")
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
