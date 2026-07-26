package application

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/infrastructure/checkpoint"
	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/domain"
	workerv1 "github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/gen/worker/v1"
	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/infrastructure/postgres"
	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/infrastructure/worker"
	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/infrastructure/workerclient"
)

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
	// lastUsedAt is updated every time a task is dispatched to this Worker.
	// Used by the idle eviction reaper to reclaim memory from long-idle
	// sessions (pattern: Kubernetes pod eviction, AWS Lambda container TTL).
	lastUsedAt time.Time
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
		entry.lastUsedAt = time.Now().UTC() // refresh idle-eviction clock on reuse
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
		lastUsedAt: time.Now().UTC(),
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
