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
	"net/http"
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
	// Optional injected Worker factory for unit tests.
	DialWorker func(ctx context.Context) (workerclient.WorkerClient, func(), error)
	// Optional OAI fixture base URL; when empty scheduler starts its own fixture.
	OAIBaseURL string
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

type scheduler struct {
	cfg           SchedulerConfig
	mu            sync.Mutex
	workerCallMu  sync.Mutex
	wake          chan struct{}
	worker        workerclient.WorkerClient
	workerCleanup func()
	workerInstID  string
	sessionKey    string   // last started worker session
	cancelOnce    sync.Map // taskID -> *cancelCall
	oai           *oaiFixture
	ownOAI        bool
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
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = time.Second
	}
	if cfg.MaxBundleBytes == 0 {
		cfg.MaxBundleBytes = 2 * 1024 * 1024
	}
	return &scheduler{
		cfg:  cfg,
		wake: make(chan struct{}, 1),
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
			s.shutdownWorker()
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
		client := s.worker
		s.mu.Unlock()
		if client == nil {
			call.err = fmt.Errorf("no active worker for task %s", task.ID)
			return
		}
		call.err = client.CancelTask(ctx, task.ID)
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

	client, err := s.ensureWorker(ctx, task.SessionKey)
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
	// Record dispatch intent before Worker RPC.
	cur, err = s.cfg.Store.MarkDispatchStarted(ctx, task.ID, s.cfg.PlatformInstanceID, s.workerInstID)
	if err != nil {
		// Likely cancelled before dispatch.
		return nil
	}

	if _, err := s.cfg.Registry.Resolve(CapabilityVersion, task.ToolPolicyVersion); err != nil {
		_ = s.finalizeOrFail(ctx, task, domain.TaskFailed, domain.DeliveryTaskFailed,
			"POLICY_RESOLVE_FAILED", err.Error(), "")
		return err
	}

	// Start session once per worker process / session key.
	if s.sessionKey != task.SessionKey {
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
		// Optional restore from durable snapshot pointer (StartSession owns restore fields).
		if task.SnapshotID != "" {
			startReq.SnapshotId = task.SnapshotID
			startReq.SnapshotChecksum = task.SnapshotChecksum
		}
		if _, err := client.StartSession(ctx, startReq); err != nil {
			_ = s.finalizeOrFail(ctx, task, domain.TaskFailed, domain.DeliveryTaskFailed,
				"START_SESSION_FAILED", err.Error(), "")
			return err
		}
		s.sessionKey = task.SessionKey
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
	client, err := s.ensureWorker(ctx, task.SessionKey)
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

func (s *scheduler) ensureWorker(ctx context.Context, sessionKey string) (workerclient.WorkerClient, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.worker != nil {
		return s.worker, nil
	}
	if s.cfg.DialWorker != nil {
		client, cleanup, err := s.cfg.DialWorker(ctx)
		if err != nil {
			return nil, err
		}
		s.worker = client
		s.workerCleanup = cleanup
		s.workerInstID = "injected-worker"
		return client, nil
	}
	// Start OAI fixture if needed.
	if s.cfg.OAIBaseURL == "" && s.oai == nil {
		fx, err := startOAIFixture()
		if err != nil {
			return nil, err
		}
		s.oai = fx
		s.ownOAI = true
	}
	base := s.cfg.OAIBaseURL
	if base == "" && s.oai != nil {
		base = s.oai.URL
	}
	if err := writeFixtureMyKey(s.cfg.ConfigRoot, base); err != nil {
		// Allow existing test-written mykey only if exclusive create fails with exist and content already fixture?
		// Spec: refuse overwrite of existing mykey. Tests create temp config roots empty.
		if !errors.Is(err, os.ErrExist) && !strings.Contains(err.Error(), "refusing to overwrite") {
			return nil, err
		}
		// If refused, continue only when file already exists (test pre-seeded).
		if _, statErr := os.Stat(filepath.Join(s.cfg.ConfigRoot, "mykey.py")); statErr != nil {
			return nil, err
		}
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
		return nil, err
	}
	if !isLoopbackAddr(listen) {
		_ = proc.Process.Kill()
		return nil, fmt.Errorf("worker not loopback: %s", listen)
	}
	conn, err := grpc.DialContext(ctx, listen, grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithBlock())
	if err != nil {
		_ = proc.Process.Kill()
		return nil, err
	}
	client, err := workerclient.New(conn)
	if err != nil {
		_ = conn.Close()
		_ = proc.Process.Kill()
		return nil, err
	}
	s.worker = client
	s.workerInstID = "loopback-" + listen
	cleanup := workerProcessCleanup{
		client:      client,
		closeConn:   conn.Close,
		killProcess: proc.Process.Kill,
		waitProcess: func() error {
			_, err := proc.Process.Wait()
			return err
		},
	}
	s.workerCleanup = func() {
		cleanup.run(workerShutdownTimeout)
		if s.ownOAI && s.oai != nil {
			s.oai.Close()
		}
	}
	_ = sessionKey
	return client, nil
}

func (s *scheduler) shutdownWorker() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.workerCleanup != nil {
		s.workerCleanup()
		s.workerCleanup = nil
		s.worker = nil
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

const testToken = "test-worker-token-not-a-real-key"

var workerListenRE = regexp.MustCompile(`WORKER_LISTEN=(\S+)`)

type oaiFixture struct {
	URL    string
	server *http.Server
	ln     net.Listener
}

func (f *oaiFixture) Close() {
	if f == nil || f.server == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = f.server.Shutdown(ctx)
	if f.ln != nil {
		_ = f.ln.Close()
	}
}

func startOAIFixture() (*oaiFixture, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		if !strings.Contains(auth, testToken) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		body := `{"id":"chatcmpl-platform","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"platform-fixture-reply"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(body))
	})
	mux := http.NewServeMux()
	mux.Handle("/v1/chat/completions", handler)
	mux.Handle("/chat/completions", handler)
	srv := &http.Server{Handler: mux}
	go func() { _ = srv.Serve(ln) }()
	return &oaiFixture{URL: "http://" + ln.Addr().String(), server: srv, ln: ln}, nil
}

func writeFixtureMyKey(configRoot, apibase string) error {
	content := fmt.Sprintf(
		"native_oai_config = {\n"+
			"    'name': 'platform-fixture-gpt',\n"+
			"    'apikey': %q,\n"+
			"    'apibase': %q,\n"+
			"    'model': 'gpt-test',\n"+
			"    'api_mode': 'chat_completions',\n"+
			"    'stream': False,\n"+
			"    'read_timeout': 30,\n"+
			"}\n",
		testToken, apibase,
	)
	if err := os.MkdirAll(configRoot, 0o755); err != nil {
		return err
	}
	path := filepath.Join(configRoot, "mykey.py")
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write([]byte(content))
	return err
}

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
