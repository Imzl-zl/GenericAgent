package application

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/checkpoint"
	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/domain"
	workerv1 "github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/gen/worker/v1"
	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/llmproxy"
	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/policy"
	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/postgres"
	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/worker"
	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/workerclient"
)

// Scheduler is the P0 single-session claim/dispatch loop.
type Scheduler interface {
	Run(ctx context.Context) error
	KickSession(ctx context.Context, sessionKey string) error
	Recover(ctx context.Context, platformInstanceID string) error
	CancelWorker(ctx context.Context, task domain.Task) error
}

// SchedulerConfig carries process-lifetime identity and lease.
type SchedulerConfig struct {
	PlatformInstanceID string
	ClaimLease         time.Duration
	PollInterval       time.Duration
	Store              TaskStore
	Registry           policy.Registry
	Coordinator        checkpoint.Coordinator
	// Runtime creates a Worker instance for a session. Required.
	Runtime worker.WorkerRuntime
	// ConfigRoot is where the platform writes the token-only mykey.py for the
	// Worker. It may be global in loopback dev mode or session-scoped in
	// container mode.
	ConfigRoot string
	// SessionScopedConfig, when true, writes mykey.py under
	// ConfigRoot/<session-key> and passes that path as ConfigDir to the runtime.
	// Required for container mode so each container mounts only its own config.
	SessionScopedConfig bool
	// RuntimeRoot is the parent directory for checkpoint/runtime data.
	RuntimeRoot string
	// Optional injected Worker factory for unit tests. Deprecated: prefer
	// passing a worker.StaticRuntime as Runtime. When set and Runtime is nil,
	// the scheduler wraps it in a static runtime.
	DialWorker func(ctx context.Context, sessionKey string) (workerclient.WorkerClient, func(), error)
	// LLM Proxy capability_token issuance. Required for real Worker path: the
	// platform issues a short-lived, session-bound token and writes a token-only
	// mykey.py (no real upstream key).
	TokenIssuer        *llmproxy.Issuer
	TokenRevoker       TokenRevoker
	LLMProxyAddr       string // e.g. "http://127.0.0.1:8081"
	ModelPolicyVersion string
	// LLMProvider is the admin-configured upstream provider. Required when
	// TokenIssuer is set so the scheduler can stamp provider/model into the
	// capability_token and write the matching mykey.py variable.
	LLMProvider LLMProviderSource
	// MaxBundleBytes for checkpoint prepare.
	MaxBundleBytes uint64
	// MaxRunningTasks caps the global number of simultaneously starting/running
	// tasks. Zero disables the check (dev/test only). Production should set
	// this to a value derived from host capacity testing.
	MaxRunningTasks int
	// PerTenantRunningLimit caps the number of simultaneously starting/running
	// tasks per requester (across all their sessions). Zero disables the check.
	PerTenantRunningLimit int
	// TaskTimeoutSeconds is passed to the Worker as RuntimePolicy.TaskTimeoutSeconds
	// for its internal soft timer (e.g. cancelling a single hung LLM call). The
	// platform does NOT use it as a hard wall-clock kill switch — legitimate
	// tasks may run many times longer than a single LLM call. Stuck-task
	// detection relies on gRPC stream errors and heartbeat lease loss instead.
	// Zero disables the Worker soft timer (dev/test).
	TaskTimeoutSeconds int
	// IdleTimeout enables Temporal-HeartbeatTimeout-style idle detection.
	// When a running task's last_activity_at is older than now()-IdleTimeout,
	// the reaper finalizes it as failed (WORKER_IDLE). This catches "Worker
	// alive but deadlocked" (LLM HTTP call hung, GIL deadlock, infinite loop)
	// — the scenario gRPC stream errors + heartbeat lease loss cannot catch.
	// Worker keeps last_activity_at fresh via chunk events + drain poll
	// heartbeats. Zero disables idle reaping (dev/test only).
	IdleTimeout time.Duration
}

// LLMProviderSource returns the platform's current default LLM provider.
type LLMProviderSource interface {
	GetDefaultProvider(ctx context.Context) (domain.LLMProvider, error)
}

const (
	defaultMaxTurns           = 6
	defaultMaxHistoryBytes    = 256 * 1024
	defaultMaxWorkingBytes    = 64 * 1024
	defaultMaxOutputBytes     = 256 * 1024
	defaultWorkerShutdownSecs = 5

	workerShutdownTimeout = defaultWorkerShutdownSecs * time.Second
)

// ErrLeaseExpired is re-exported from domain for callers in the application
// layer. The postgres store returns this when a heartbeat updates 0 rows.
var ErrLeaseExpired = domain.ErrLeaseExpired

type cancelCall struct {
	once sync.Once
	err  error
}

type dispatchHeartbeat struct {
	ctx      context.Context
	cancel   context.CancelFunc
	stop     chan struct{}
	done     chan struct{}
	stopOnce sync.Once
	mu       sync.Mutex
	err      error
}

func (h *dispatchHeartbeat) setError(err error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.err == nil {
		h.err = err
	}
}

func (h *dispatchHeartbeat) Stop() error {
	h.stopOnce.Do(func() { close(h.stop) })
	<-h.done
	h.cancel()
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.err
}

// workerEntry holds a dedicated Worker process bound to one session_key.
type workerEntry struct {
	client     workerclient.WorkerClient
	cleanup    func()
	instID     string
	sessionKey string
	jti        string // capability_token JTI; revoked on Worker cleanup
	startOnce  sync.Once
	startErr   error
	started    bool
}

// startSession invokes StartSession on the worker exactly once. Subsequent
// calls return the cached result. This is called AFTER MarkDispatchStarted so
// that cancel-during-StartSession sees WorkerDispatchStartedAt != nil and
// records a durable cancel request instead of finalizing immediately.
func (e *workerEntry) startSession(ctx context.Context, req *workerv1.StartSessionRequest) error {
	e.startOnce.Do(func() {
		if _, err := e.client.StartSession(ctx, req); err != nil {
			e.startErr = err
			return
		}
		e.started = true
	})
	return e.startErr
}

type scheduler struct {
	cfg          SchedulerConfig
	mu           sync.Mutex
	workerCallMu sync.Mutex
	wake         chan struct{}
	workers      map[string]*workerEntry // session_key -> dedicated worker
	cancelOnce   sync.Map                // taskID -> *cancelCall
}

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

// NewScheduler validates config and constructs the scheduler.
func NewScheduler(cfg SchedulerConfig) (Scheduler, error) {
	if strings.TrimSpace(cfg.PlatformInstanceID) == "" {
		return nil, fmt.Errorf("SchedulerConfig.PlatformInstanceID is required")
	}
	if cfg.ClaimLease <= 0 {
		return nil, fmt.Errorf("SchedulerConfig.ClaimLease must be positive")
	}
	if cfg.Store == nil || cfg.Registry == nil {
		return nil, fmt.Errorf("store and registry are required")
	}
	if cfg.Runtime == nil && cfg.DialWorker != nil {
		cfg.Runtime = worker.NewStaticRuntime(cfg.DialWorker)
	}
	if cfg.Runtime == nil {
		return nil, fmt.Errorf("SchedulerConfig.Runtime is required")
	}
	// Real Worker path (no injected DialWorker) MUST go through the LLM Proxy:
	// token issuer + revoker + Proxy address + config root for token-only
	// mykey.py. This is the spec §7.1 security red line.
	if cfg.DialWorker == nil {
		if cfg.TokenIssuer == nil {
			return nil, fmt.Errorf("SchedulerConfig.TokenIssuer is required for real Worker path")
		}
		if cfg.TokenRevoker == nil {
			return nil, fmt.Errorf("SchedulerConfig.TokenRevoker is required for real Worker path")
		}
		if strings.TrimSpace(cfg.LLMProxyAddr) == "" {
			return nil, fmt.Errorf("SchedulerConfig.LLMProxyAddr is required for real Worker path")
		}
		if strings.TrimSpace(cfg.ConfigRoot) == "" {
			return nil, fmt.Errorf("SchedulerConfig.ConfigRoot is required for real Worker path")
		}
		if cfg.LLMProvider == nil {
			return nil, fmt.Errorf("SchedulerConfig.LLMProvider is required for real Worker path")
		}
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = time.Second
	}
	if cfg.MaxBundleBytes == 0 {
		cfg.MaxBundleBytes = 2 * 1024 * 1024
	}
	return &scheduler{
		cfg:     cfg,
		wake:    make(chan struct{}, 1),
		workers: make(map[string]*workerEntry),
	}, nil
}

func (s *scheduler) KickSession(ctx context.Context, sessionKey string) error {
	_ = ctx
	_ = sessionKey
	select {
	case s.wake <- struct{}{}:
	default:
	}
	return nil
}

// reapIdleTasks finalizes running tasks whose last_activity_at is older than
// now-idle. This is the "Worker alive but deadlocked" detector (Temporal
// HeartbeatTimeout pattern). Legitimate long tasks keep last_activity_at fresh
// via RecordChunkEvent (each chunk) and RecordHeartbeat (drain poll), so they
// are NOT reaped. Only called when SchedulerConfig.IdleTimeout > 0.
func (s *scheduler) reapIdleTasks(ctx context.Context, owned []domain.Task, idle time.Duration) error {
	cutoff := time.Now().UTC().Add(-idle)
	for _, t := range owned {
		if t.Status != domain.TaskRunning {
			continue
		}
		if t.LastActivityAt.IsZero() {
			// Cold-started task that has not produced any activity yet; skip
			// (give Worker a chance to send first chunk/heartbeat).
			continue
		}
		if t.LastActivityAt.After(cutoff) {
			continue
		}
		slog.ErrorContext(ctx, "scheduler: reaping idle task (Worker alive but deadlocked)",
			"task_id", t.ID,
			"session_key", t.SessionKey,
			"last_activity_at", t.LastActivityAt.UTC().Format(time.RFC3339),
			"idle_threshold_seconds", int(idle.Seconds()))
		_ = s.finalizeOrFail(ctx, t, domain.TaskFailed, domain.DeliveryTaskFailed,
			"WORKER_IDLE", "Worker heartbeat went silent; possible deadlock or hung I/O", "")
		_ = s.KickSession(ctx, t.SessionKey)
	}
	return nil
}

func (s *scheduler) Recover(ctx context.Context, platformInstanceID string) error {
	if platformInstanceID == "" {
		platformInstanceID = s.cfg.PlatformInstanceID
	}
	_, err := s.cfg.Store.RecoverAfterRestart(ctx, platformInstanceID)
	return err
}

func (s *scheduler) Run(ctx context.Context) error {
	ticker := time.NewTicker(s.cfg.PollInterval)
	defer ticker.Stop()
	for {
		if err := s.tick(ctx); err != nil && !errors.Is(err, context.Canceled) {
			// Keep running; surface via stderr-like return only on ctx done.
			_ = err
		}
		select {
		case <-ctx.Done():
			shutCtx, shutCancel := context.WithTimeout(context.Background(), workerShutdownTimeout*3)
			defer shutCancel()
			s.shutdownAllWorkers(shutCtx)
			return ctx.Err()
		case <-s.wake:
		case <-ticker.C:
		}
	}
}

func (s *scheduler) tick(ctx context.Context) error {
	// Recover newly expired foreign-owner work opportunistically.
	if _, err := s.cfg.Store.RecoverAfterRestart(ctx, s.cfg.PlatformInstanceID); err != nil {
		return err
	}
	// Heartbeat owned claims.
	owned, err := s.cfg.Store.ListOwnedActiveTasks(ctx, s.cfg.PlatformInstanceID)
	if err != nil {
		return err
	}
	for _, t := range owned {
		hbErr := s.cfg.Store.HeartbeatClaim(ctx, t.ID, s.cfg.PlatformInstanceID, s.cfg.ClaimLease)
		switch {
		case hbErr == nil:
			// ok
		case errors.Is(hbErr, domain.ErrLeaseExpired):
			// Lease lost (expired or stolen by RecoverAfterRestart on another
			// instance). The dispatch goroutine — if any — will observe ctx
			// cancel via the heartbeat's own context and exit. Finalize the
			// task so the running slot is released; otherwise it would block
			// MaxRunningTasks forever.
			slog.ErrorContext(ctx, "scheduler: heartbeat lost lease; finalizing task",
				"task_id", t.ID,
				"session_key", t.SessionKey,
				"status", string(t.Status))
			_ = s.finalizeOrFail(ctx, t, domain.TaskFailed, domain.DeliveryTaskFailed,
				"LEASE_EXPIRED", "claim lease expired or lost during heartbeat", "")
			_ = s.KickSession(ctx, t.SessionKey)
			continue
		default:
			// DB connectivity error; don't finalize — retry next tick.
			slog.ErrorContext(ctx, "scheduler: heartbeat failed (transient)",
				"task_id", t.ID,
				"session_key", t.SessionKey,
				"error", hbErr)
		}
		// Drive cancel if requested and dispatch started.
		if t.CancelRequestedAt != nil && t.WorkerDispatchStartedAt != nil {
			s.maybeCancelWorker(ctx, t)
		}
	}
	// Stuck-task reaping uses idle detection (Temporal HeartbeatTimeout pattern):
	//   1. gRPC stream error/close — Worker process crashed or network died.
	//      The dispatch loop's streamErr path finalizes the task immediately.
	//   2. Heartbeat lease loss — dispatch goroutine died (panic, Goexit) so
	//      HeartbeatClaim stopped being called. The tick loop's ErrLeaseExpired
	//      path finalizes the task on the next tick.
	//   3. Idle timeout — Worker alive but deadlocked (LLM HTTP call hung, GIL
	//      deadlock, infinite loop). Reaper checks last_activity_at (updated by
	//      RecordChunkEvent on every chunk and RecordHeartbeat on drain poll)
	//      against TASK_IDLE_TIMEOUT_SECONDS. Legitimate long tasks keep
	//      producing chunks or heartbeats, so they are not reaped.
	if s.cfg.IdleTimeout > 0 {
		if err := s.reapIdleTasks(ctx, owned, s.cfg.IdleTimeout); err != nil {
			slog.ErrorContext(ctx, "scheduler: reap idle tasks failed", "error", err)
		}
	}
	// If we already own a non-terminal starting/running task, continue dispatch if needed.
	for _, t := range owned {
		if t.Status == domain.TaskStarting && t.WorkerDispatchStartedAt == nil {
			if err := s.dispatch(ctx, t); err != nil {
				return err
			}
			return nil
		}
		if t.Status == domain.TaskStarting && t.WorkerDispatchStartedAt != nil {
			// In-flight; wait for completion path (dispatch holds the stream).
			return nil
		}
		if t.Status == domain.TaskRunning {
			return nil
		}
	}
	keys, err := s.cfg.Store.ListClaimableSessionKeys(ctx, 16, s.cfg.PerTenantRunningLimit)
	if err != nil {
		return err
	}
	if len(keys) == 0 {
		return nil
	}
	// Global running cap: don't claim new tasks when at capacity. The check
	// happens after ListClaimableSessionKeys so the cap doesn't mask session
	// discovery errors. Zero means disabled (dev/test).
	if s.cfg.MaxRunningTasks > 0 {
		running, err := s.cfg.Store.CountRunningTasks(ctx)
		if err != nil {
			return err
		}
		if running >= s.cfg.MaxRunningTasks {
			return nil
		}
	}
	for _, sk := range keys {
		task, ok, err := s.cfg.Store.ClaimNextTask(ctx, sk, s.cfg.PlatformInstanceID, s.cfg.ClaimLease)
		if err != nil {
			return err
		}
		if !ok {
			continue
		}
		return s.dispatch(ctx, task)
	}
	return nil
}

func (s *scheduler) maybeCancelWorker(ctx context.Context, task domain.Task) {
	_ = s.CancelWorker(ctx, task)
}

func (s *scheduler) CancelWorker(ctx context.Context, task domain.Task) error {
	value, _ := s.cancelOnce.LoadOrStore(task.ID, &cancelCall{})
	call := value.(*cancelCall)
	call.once.Do(func() {
		s.workerCallMu.Lock()
		defer s.workerCallMu.Unlock()
		s.mu.Lock()
		entry := s.workers[task.SessionKey]
		s.mu.Unlock()
		if entry == nil {
			call.err = fmt.Errorf("no active worker for session %s task %s", task.SessionKey, task.ID)
			return
		}
		call.err = entry.client.CancelTask(ctx, task.ID)
	})
	return call.err
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
		_ = s.finalizeOrFail(ctx, task, domain.TaskFailed, domain.DeliveryTaskFailed,
			"WORKER_START_FAILED", err.Error(), "")
		_ = s.KickSession(ctx, task.SessionKey)
		return err
	}

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

	// Hard wall-clock timeout is intentionally NOT applied here. A single
	// task may legitimately run for minutes (slow LLM thinking, multi-step
	// file processing, long tool chains). Killing the gRPC stream on a fixed
	// budget would abort legitimate work. Instead, stuck-task detection uses
	// an idle heuristic (see reapStuckTasks): a task is only reaped when it
	// has produced no activity for longer than the idle threshold, which
	// distinguishes "slow but working" from "deadlocked". The Worker's own
	// RuntimePolicy.TaskTimeoutSeconds remains as a soft timer for its
	// internal use (e.g. cancelling a single hung LLM call), not as a
	// process-level kill switch.
	executeCtx, cancelExecute := context.WithCancel(ctx)
	defer cancelExecute()
	events, errs := client.ExecuteTask(executeCtx, req)
	releaseWorkerCall()
	var terminal *workerv1.Terminal
	var streamErr error
	eventsOpen, errsOpen := true, true
	for eventsOpen || errsOpen {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case ev, ok := <-events:
			if !ok {
				eventsOpen = false
				events = nil
				continue
			}
			if ev.IsChunk() && ev.Chunk != nil {
				text := ev.Chunk.GetText()
				// Empty-text Chunk is a Worker heartbeat (see task_drain.HEARTBEAT_INTERVAL_S).
				// Refresh last_activity_at without writing a chunk event, so the
				// reaper does not trip during slow LLM thinking / file processing.
				if text == "" {
					if err := s.cfg.Store.RecordHeartbeat(ctx, task.ID); err != nil {
						slog.WarnContext(ctx, "scheduler: record heartbeat failed",
							"task_id", task.ID, "error", err)
					}
					continue
				}
				sum := sha256.Sum256([]byte(text))
				digest := "sha256:" + hex.EncodeToString(sum[:])
				if err := s.cfg.Store.RecordChunkEvent(ctx, task.ID, len([]byte(text)), digest); err != nil {
					cancelExecute()
					_ = s.finalizeOrFail(ctx, task, domain.TaskFailed, domain.DeliveryTaskFailed,
						"CHUNK_EVENT_FAILED", err.Error(), "")
					_ = s.KickSession(ctx, task.SessionKey)
					return fmt.Errorf("record chunk event: %w", err)
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
	client, _, err := s.ensureWorker(ctx, task)
	if err != nil {
		return err
	}
	ready, err := client.BeginCheckpoint(ctx, &workerv1.BeginCheckpointRequest{
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
	// Digest consistency with terminal when present.
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

func (s *scheduler) startDispatchHeartbeat(parent context.Context, task domain.Task) (*dispatchHeartbeat, error) {
	ctx, cancel := context.WithCancel(parent)
	if err := s.cfg.Store.HeartbeatClaim(ctx, task.ID, s.cfg.PlatformInstanceID, s.cfg.ClaimLease); err != nil {
		cancel()
		return nil, err
	}
	heartbeat := &dispatchHeartbeat{
		ctx: ctx, cancel: cancel, stop: make(chan struct{}), done: make(chan struct{}),
	}
	interval := s.cfg.ClaimLease / 3
	if interval <= 0 {
		interval = s.cfg.ClaimLease
	}
	go func() {
		defer close(heartbeat.done)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-heartbeat.stop:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := s.cfg.Store.HeartbeatClaim(ctx, task.ID, s.cfg.PlatformInstanceID, s.cfg.ClaimLease); err != nil {
					current, getErr := s.cfg.Store.GetTask(ctx, task.ID)
					if getErr == nil && current.Status.IsTerminal() {
						return
					}
					heartbeat.setError(err)
					cancel()
					return
				}
			}
		}
	}()
	return heartbeat, nil
}

// ensureWorker returns the dedicated Worker for task.SessionKey, creating a new
// Worker process on first use. StartSession is NOT called here; it is invoked
// later by dispatch after MarkDispatchStarted so that cancel-during-StartSession
// sees WorkerDispatchStartedAt != nil and records a durable cancel request.
//
// On first use for a session, a capability_token is issued (via TokenIssuer)
// and a token-only mykey.py is written to ConfigRoot. The real upstream key
// never enters the Worker (spec §7.1). The token JTI is stored for revocation
// when the Worker process is cleaned up.
func (s *scheduler) ensureWorker(ctx context.Context, task domain.Task) (workerclient.WorkerClient, *workerEntry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if entry, ok := s.workers[task.SessionKey]; ok {
		return entry.client, entry, nil
	}

	jti, err := s.issueAndWriteCredential(ctx, task.SessionKey)
	if err != nil {
		return nil, nil, err
	}

	client, instID, cleanup, err := s.startWorkerProcess(ctx, task.SessionKey)
	if err != nil {
		s.revokeTokenBestEffort(context.Background(), jti)
		return nil, nil, err
	}

	entry := &workerEntry{
		client:     client,
		cleanup:    cleanup,
		instID:     instID,
		sessionKey: task.SessionKey,
		jti:        jti,
	}
	s.workers[task.SessionKey] = entry
	return client, entry, nil
}

// configDirFor returns the directory that holds mykey.py for the session.
// When SessionScopedConfig is false the global ConfigRoot is used.
func (s *scheduler) configDirFor(sessionKey string) string {
	if !s.cfg.SessionScopedConfig {
		return s.cfg.ConfigRoot
	}
	return s.cfg.ConfigRoot + "/" + sessionKey
}

// revokeTokenBestEffort attempts a revocation; failures are logged (token TTL
// is the safety net). Never blocks the caller on revocation errors.
func (s *scheduler) revokeTokenBestEffort(ctx context.Context, jti string) {
	if s.cfg.TokenRevoker == nil || jti == "" {
		return
	}
	revokeCtx, cancel := context.WithTimeout(ctx, revokeTimeout)
	defer cancel()
	if err := s.cfg.TokenRevoker.Revoke(revokeCtx, jti); err != nil {
		slog.WarnContext(ctx, "scheduler: best-effort revoke failed",
			"jti", jti,
			"error", err)
	}
}

// startSessionOnWorker calls StartSession on the worker bound to task.SessionKey.
// Must be called AFTER MarkDispatchStarted. Idempotent per worker via startOnce.
func (s *scheduler) startSessionOnWorker(ctx context.Context, task domain.Task) error {
	s.mu.Lock()
	entry := s.workers[task.SessionKey]
	s.mu.Unlock()
	if entry == nil {
		return fmt.Errorf("no worker for session %s", task.SessionKey)
	}
	startReq := &workerv1.StartSessionRequest{
		SessionKey: task.SessionKey,
		RuntimePolicy: &workerv1.RuntimePolicy{
			MaxTurns:           defaultMaxTurns,
			MaxHistoryBytes:    defaultMaxHistoryBytes,
			MaxWorkingBytes:    defaultMaxWorkingBytes,
			MaxOutputBytes:     defaultMaxOutputBytes,
			TaskTimeoutSeconds: uint32(s.cfg.TaskTimeoutSeconds),
			CapabilityVersion:  CapabilityVersion,
			PolicyDigest:       s.cfg.Registry.Digest(),
		},
	}
	if task.SnapshotID != "" {
		startReq.SnapshotId = task.SnapshotID
		startReq.SnapshotChecksum = task.SnapshotChecksum
	}
	if err := entry.startSession(ctx, startReq); err != nil {
		s.mu.Lock()
		delete(s.workers, task.SessionKey)
		s.mu.Unlock()
		s.revokeTokenBestEffort(context.Background(), entry.jti)
		entry.cleanup()
		return err
	}
	return nil
}

// startWorkerProcess asks the configured WorkerRuntime to create a Worker for
// the session. The token-only mykey.py has already been written by ensureWorker.
func (s *scheduler) startWorkerProcess(ctx context.Context, sessionKey string) (workerclient.WorkerClient, string, func(), error) {
	inst, err := s.cfg.Runtime.Start(ctx, worker.StartRequest{
		SessionKey: sessionKey,
		ConfigDir:  s.configDirFor(sessionKey),
		RuntimeDir: s.cfg.RuntimeRoot,
	})
	if err != nil {
		return nil, "", nil, err
	}
	return inst.Client, inst.InstID, inst.Cleanup, nil
}

// shutdownAllWorkers revokes all active capability_tokens and tears down every
// Worker process. Called once on platform shutdown.
func (s *scheduler) shutdownAllWorkers(ctx context.Context) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for sk, entry := range s.workers {
		s.revokeTokenBestEffort(ctx, entry.jti)
		entry.cleanup()
		delete(s.workers, sk)
	}
}

// stopSessionWorker evicts the Worker for a session without cancelling any
// task. Used by /new to force a fresh Worker on the next dispatch.
func (s *scheduler) stopSessionWorker(sessionKey string) {
	s.mu.Lock()
	entry := s.workers[sessionKey]
	delete(s.workers, sessionKey)
	s.mu.Unlock()
	if entry != nil {
		s.revokeTokenBestEffort(context.Background(), entry.jti)
		entry.cleanup()
	}
}

func boundMsg(s string) string {
	if len(s) > postgres.MaxTerminalErrorBytes {
		return s[:postgres.MaxTerminalErrorBytes]
	}
	return s
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
