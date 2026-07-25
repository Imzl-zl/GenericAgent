package application

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/checkpoint"
	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/domain"
	workerv1 "github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/gen/worker/v1"
	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/llmproxy"
	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/policy"
	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/postgres"
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
	// Worker process environment (dev-loopback).
	PolicyFile   string
	ConfigRoot   string
	LegacyRoot   string
	RuntimeRoot  string
	WorkerPython string
	WorkerSrc    string
	// Optional injected Worker factory for unit tests. When set, the scheduler
	// calls it once per session_key to obtain a dedicated worker instance.
	DialWorker func(ctx context.Context, sessionKey string) (workerclient.WorkerClient, func(), error)
	// LLM Proxy capability_token issuance. Required when DialWorker is nil
	// (real Worker path): the platform issues a short-lived, session-bound
	// token and writes a token-only mykey.py (no real upstream key).
	TokenIssuer       *llmproxy.Issuer
	TokenRevoker      TokenRevoker
	LLMProxyAddr      string // e.g. "http://127.0.0.1:8081"
	ModelPolicyVersion string
	// MaxBundleBytes for checkpoint prepare.
	MaxBundleBytes uint64
}

const workerShutdownTimeout = 5 * time.Second

type cancelCall struct {
	once sync.Once
	err  error
}

type workerProcessCleanup struct {
	client      workerclient.WorkerClient
	closeConn   func() error
	killProcess func() error
	waitProcess func() error
}

func (c workerProcessCleanup) run(timeout time.Duration) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	_ = c.client.Shutdown(ctx, "scheduler-stop")
	cancel()
	_ = c.closeConn()
	_ = c.killProcess()
	_ = c.waitProcess()
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
		log.Printf("scheduler: CompleteFailedTerminal failed task=%s target_status=%s: %v", task.ID, status, err)
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
	// Real Worker path (no injected DialWorker) MUST go through the LLM Proxy:
	// token issuer + revoker + Proxy address + config root for token-only
	// mykey.py. This is the spec §7.1 security red line.
	if cfg.DialWorker == nil {
		if cfg.TokenIssuer == nil {
			return nil, fmt.Errorf("SchedulerConfig.TokenIssuer is required when DialWorker is nil (real Worker must use capability_token)")
		}
		if cfg.TokenRevoker == nil {
			return nil, fmt.Errorf("SchedulerConfig.TokenRevoker is required when DialWorker is nil")
		}
		if strings.TrimSpace(cfg.LLMProxyAddr) == "" {
			return nil, fmt.Errorf("SchedulerConfig.LLMProxyAddr is required when DialWorker is nil")
		}
		if strings.TrimSpace(cfg.ConfigRoot) == "" {
			return nil, fmt.Errorf("SchedulerConfig.ConfigRoot is required when DialWorker is nil")
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
		_ = s.cfg.Store.HeartbeatClaim(ctx, t.ID, s.cfg.PlatformInstanceID, s.cfg.ClaimLease)
		// Drive cancel if requested and dispatch started.
		if t.CancelRequestedAt != nil && t.WorkerDispatchStartedAt != nil {
			s.maybeCancelWorker(ctx, t)
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
	keys, err := s.cfg.Store.ListClaimableSessionKeys(ctx, 16)
	if err != nil {
		return err
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
		// Likely cancelled before dispatch.
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

	jti, err := s.issueAndWriteCredential(task.SessionKey)
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

// issueAndWriteCredential issues a capability_token for the session and writes
// a token-only mykey.py. Returns the JTI for later revocation. When
// TokenIssuer is nil (unit tests with injected DialWorker), returns "".
func (s *scheduler) issueAndWriteCredential(sessionKey string) (string, error) {
	if s.cfg.TokenIssuer == nil {
		return "", nil
	}
	token, claims, err := s.cfg.TokenIssuer.Issue(sessionKey, s.cfg.ModelPolicyVersion)
	if err != nil {
		return "", fmt.Errorf("issue capability_token: %w", err)
	}
	if s.cfg.LLMProxyAddr != "" && s.cfg.ConfigRoot != "" {
		if err := writeTokenOnlyMyKey(s.cfg.ConfigRoot, s.cfg.LLMProxyAddr, token); err != nil {
			return "", fmt.Errorf("write token-only mykey.py: %w", err)
		}
	}
	return claims.Jti, nil
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
		log.Printf("scheduler: best-effort revoke jti=%s failed: %v", jti, err)
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
			MaxTurns:           6,
			MaxHistoryBytes:    256 * 1024,
			MaxWorkingBytes:    64 * 1024,
			MaxOutputBytes:     256 * 1024,
			TaskTimeoutSeconds: 60,
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

// startWorkerProcess launches a Python Worker subprocess (or an injected test
// double) and returns its client. The token-only mykey.py has already been
// written by ensureWorker; this function only launches the process and dials
// its gRPC port. The real upstream key is never on this code path.
func (s *scheduler) startWorkerProcess(ctx context.Context, sessionKey string) (workerclient.WorkerClient, string, func(), error) {
	if s.cfg.DialWorker != nil {
		client, cleanup, err := s.cfg.DialWorker(ctx, sessionKey)
		if err != nil {
			return nil, "", nil, err
		}
		return client, "injected-worker", cleanup, nil
	}

	python := s.cfg.WorkerPython
	if python == "" {
		python = defaultPython(s.cfg.LegacyRoot)
	}
	workerSrc := s.cfg.WorkerSrc
	if workerSrc == "" {
		workerSrc = filepath.Join(s.cfg.LegacyRoot, "tenant_platform", "worker-python", "src")
	}
	proc, listen, err := startPythonWorker(python, workerSrc, s.cfg.ConfigRoot, s.cfg.LegacyRoot, s.cfg.RuntimeRoot, s.cfg.PolicyFile)
	if err != nil {
		return nil, "", nil, err
	}
	if !isLoopbackAddr(listen) {
		_ = proc.Process.Kill()
		return nil, "", nil, fmt.Errorf("worker not loopback: %s", listen)
	}
	conn, err := grpc.DialContext(ctx, listen, grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithBlock())
	if err != nil {
		_ = proc.Process.Kill()
		return nil, "", nil, err
	}
	client, err := workerclient.New(conn)
	if err != nil {
		_ = conn.Close()
		_ = proc.Process.Kill()
		return nil, "", nil, err
	}
	instID := "loopback-" + listen
	cleanup := func() {
		workerProcessCleanup{
			client:      client,
			closeConn:   conn.Close,
			killProcess: proc.Process.Kill,
			waitProcess: func() error {
				_, err := proc.Process.Wait()
				return err
			},
		}.run(workerShutdownTimeout)
	}
	return client, instID, cleanup, nil
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

// --- worker process helpers (dev loopback) ---

var workerListenRE = regexp.MustCompile(`WORKER_LISTEN=(\S+)`)

func startPythonWorker(python, workerSrc, configRoot, legacyRoot, runtimeDir, policyFile string) (*exec.Cmd, string, error) {
	listen := "127.0.0.1:0"
	cmd := exec.Command(python, "-m", "ga_worker.entrypoint", "--listen", listen, "--grace-seconds", "5")
	configureWorkerProcess(cmd)
	if base := filepath.Base(workerSrc); base == "src" {
		cmd.Dir = filepath.Dir(workerSrc)
	} else {
		cmd.Dir = workerSrc
	}
	env := os.Environ()
	env = setEnv(env, "GA_CONFIG_ROOT", configRoot)
	env = setEnv(env, "GA_LEGACY_ROOT", legacyRoot)
	env = setEnv(env, "GA_RUNTIME_DIR", runtimeDir)
	env = setEnv(env, "GA_POLICY_FILE", policyFile)
	env = setEnv(env, "GA_WORKER_LISTEN", listen)
	env = unsetEnv(env, "OPENAI_API_KEY")
	env = unsetEnv(env, "ANTHROPIC_API_KEY")
	pp := workerSrc
	if existing := getEnv(env, "PYTHONPATH"); existing != "" {
		pp = workerSrc + string(os.PathListSeparator) + existing
	}
	env = setEnv(env, "PYTHONPATH", pp)
	cmd.Env = env
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, "", err
	}
	cmd.Stderr = cmd.Stdout
	if err := cmd.Start(); err != nil {
		return nil, "", err
	}
	listenAddr, err := waitWorkerListen(stdout, 30*time.Second)
	if err != nil {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
		rest, _ := io.ReadAll(stdout)
		return nil, "", fmt.Errorf("%w\nworker output:\n%s", err, string(rest))
	}
	go func() { _, _ = io.Copy(io.Discard, stdout) }()
	return cmd, listenAddr, nil
}

func waitWorkerListen(r io.Reader, timeout time.Duration) (string, error) {
	deadline := time.Now().Add(timeout)
	buf := make([]byte, 0, 4096)
	tmp := make([]byte, 512)
	for time.Now().Before(deadline) {
		n, err := r.Read(tmp)
		if n > 0 {
			buf = append(buf, tmp[:n]...)
			if m := workerListenRE.FindSubmatch(buf); m != nil {
				return string(m[1]), nil
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return "", fmt.Errorf("worker exited before WORKER_LISTEN; output:\n%s", string(buf))
			}
			return "", err
		}
	}
	return "", fmt.Errorf("timeout waiting for WORKER_LISTEN; output:\n%s", string(buf))
}

func isLoopbackAddr(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func setEnv(env []string, key, value string) []string {
	prefix := key + "="
	out := env[:0]
	for _, e := range env {
		if !strings.HasPrefix(e, prefix) {
			out = append(out, e)
		}
	}
	return append(out, prefix+value)
}

func unsetEnv(env []string, key string) []string {
	prefix := key + "="
	out := env[:0]
	for _, e := range env {
		if !strings.HasPrefix(e, prefix) {
			out = append(out, e)
		}
	}
	return out
}

func getEnv(env []string, key string) string {
	prefix := key + "="
	for _, e := range env {
		if strings.HasPrefix(e, prefix) {
			return strings.TrimPrefix(e, prefix)
		}
	}
	return ""
}

func defaultPython(legacyRoot string) string {
	candidates := []string{
		filepath.Join(legacyRoot, ".venv", "Scripts", "python.exe"),
		filepath.Join(legacyRoot, ".venv", "bin", "python"),
		"python3",
		"python",
	}
	for _, c := range candidates {
		if c == "python3" || c == "python" {
			return c
		}
		if st, err := os.Stat(c); err == nil && !st.IsDir() {
			return c
		}
	}
	return "python"
}
