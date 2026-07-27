package application

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"sync"
	"time"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/domain"
	workerv1 "github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/gen/worker/v1"
	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/infrastructure/worker"
	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/infrastructure/workerclient"
)

// workerEntry holds a dedicated Worker process bound to one session_key.
type workerEntry struct {
	client             workerclient.WorkerClient
	cleanup            func()
	instID             string
	sessionKey         string
	credentials        workerCredentialSet
	pendingRefresh     *pendingCredentialRefresh
	pendingRevocations []workerCredentialSet
	lifecycleMu        sync.Mutex
	startOnce          sync.Once
	startErr           error
	started            bool
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
// On first use, one capability token per routed Provider is written to the
// session-scoped runtime JSON. The real upstream keys never enter the Worker.
// Credential sets remain tracked until their JTIs are durably revoked.
func (s *scheduler) ensureWorker(ctx context.Context, task domain.Task) (workerclient.WorkerClient, *workerEntry, error) {
	for {
		s.mu.Lock()
		entry := s.workers[task.SessionKey]
		if entry == nil {
			entry = &workerEntry{sessionKey: task.SessionKey}
			entry.lifecycleMu.Lock()
			s.workers[task.SessionKey] = entry
			s.mu.Unlock()
			return s.initializeWorkerEntry(ctx, task.SessionKey, entry)
		}
		s.mu.Unlock()

		entry.lifecycleMu.Lock()
		if !s.workerEntryIsCurrent(task.SessionKey, entry) {
			entry.lifecycleMu.Unlock()
			continue
		}
		replace, err := s.prepareWorkerEntry(ctx, entry)
		if err != nil {
			entry.lifecycleMu.Unlock()
			return nil, entry, err
		}
		if !replace {
			entry.lastUsedAt = time.Now().UTC()
			client := entry.client
			entry.lifecycleMu.Unlock()
			return client, entry, nil
		}

		s.removeWorkerEntry(task.SessionKey, entry)
		s.cleanupWorkerEntryBestEffort(context.Background(), entry)
		entry.lifecycleMu.Unlock()
	}
}

func (s *scheduler) prepareWorkerEntry(ctx context.Context, entry *workerEntry) (bool, error) {
	if err := s.flushPendingCredentialRevocations(ctx, entry); err != nil {
		return false, fmt.Errorf("flush pending credential revocations: %w", err)
	}
	if entry.pendingRefresh != nil {
		if err := s.refreshWorkerCredentials(ctx, entry); err != nil {
			return false, err
		}
	}
	if s.cfg.TokenIssuer == nil {
		return false, nil
	}
	replace, err := s.routingSnapshotRequiresReplacement(ctx, entry.credentials.Snapshot)
	if err != nil || replace {
		return replace, err
	}
	if s.credentialsNeedRefresh(entry.credentials) {
		if err := s.refreshWorkerCredentials(ctx, entry); err != nil {
			return false, err
		}
	}
	return false, nil
}

func (s *scheduler) initializeWorkerEntry(
	ctx context.Context, sessionKey string, entry *workerEntry,
) (workerclient.WorkerClient, *workerEntry, error) {
	credentials, err := s.issueInitialWorkerCredentials(ctx, sessionKey)
	if err != nil {
		s.removeWorkerEntry(sessionKey, entry)
		entry.lifecycleMu.Unlock()
		return nil, nil, err
	}
	client, instID, cleanup, err := s.startWorkerProcess(ctx, sessionKey)
	if err != nil {
		s.removeWorkerEntry(sessionKey, entry)
		s.revokeCredentialSetBestEffort(context.Background(), credentials)
		entry.lifecycleMu.Unlock()
		return nil, nil, err
	}
	entry.client = client
	entry.cleanup = cleanup
	entry.instID = instID
	entry.credentials = credentials
	entry.lastUsedAt = time.Now().UTC()
	entry.lifecycleMu.Unlock()
	return client, entry, nil
}

func (s *scheduler) workerEntryIsCurrent(sessionKey string, entry *workerEntry) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.workers[sessionKey] == entry
}

func (s *scheduler) removeWorkerEntry(sessionKey string, entry *workerEntry) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.workers[sessionKey] == entry {
		delete(s.workers, sessionKey)
	}
}

// configDirFor returns the directory that holds mykey.py for the session.
// When SessionScopedConfig is false the global ConfigRoot is used.
func (s *scheduler) configDirFor(sessionKey string) string {
	if !s.cfg.SessionScopedConfig {
		return s.cfg.ConfigRoot
	}
	digest := sha256.Sum256([]byte(sessionKey))
	return filepath.Join(s.cfg.ConfigRoot, hex.EncodeToString(digest[:]))
}

func (s *scheduler) cleanupWorkerEntryBestEffort(ctx context.Context, entry *workerEntry) {
	if entry.cleanup != nil {
		entry.cleanup()
	}
	if entry.pendingRefresh != nil {
		s.revokeCredentialSetBestEffort(ctx, entry.pendingRefresh.Next)
	}
	for _, set := range entry.pendingRevocations {
		s.revokeCredentialSetBestEffort(ctx, set)
	}
	s.revokeCredentialSetBestEffort(ctx, entry.credentials)
}

// revokeCredentialSetBestEffort persists every JTI with the token's exact
// expiry. Token material and full JTIs are never logged.

// startSessionOnWorker calls StartSession on the worker bound to task.SessionKey.
// Must be called AFTER MarkDispatchStarted. Idempotent per worker via startOnce.
func (s *scheduler) startSessionOnWorker(ctx context.Context, task domain.Task) error {
	s.mu.Lock()
	entry := s.workers[task.SessionKey]
	s.mu.Unlock()
	if entry == nil {
		return fmt.Errorf("no worker for session %s", task.SessionKey)
	}
	entry.lifecycleMu.Lock()
	defer entry.lifecycleMu.Unlock()
	if !s.workerEntryIsCurrent(task.SessionKey, entry) {
		return fmt.Errorf("worker replaced for session %s", task.SessionKey)
	}
	startReq := &workerv1.StartSessionRequest{
		SessionKey: task.SessionKey,
		RuntimePolicy: &workerv1.RuntimePolicy{
			MaxTurns: defaultMaxTurns, MaxHistoryBytes: defaultMaxHistoryBytes,
			MaxWorkingBytes: defaultMaxWorkingBytes, MaxOutputBytes: defaultMaxOutputBytes,
			TaskTimeoutSeconds: uint32(s.cfg.TaskTimeoutSeconds),
			CapabilityVersion:  CapabilityVersion, PolicyDigest: s.cfg.Registry.Digest(),
		},
	}
	if !entry.started && !task.FreshSession && s.cfg.Coordinator != nil {
		restore, ok, err := s.cfg.Coordinator.CurrentRestorePoint(ctx, task.WorkspaceID)
		if err != nil {
			s.removeWorkerEntry(task.SessionKey, entry)
			s.cleanupWorkerEntryBestEffort(context.Background(), entry)
			return fmt.Errorf("resolve current workspace checkpoint: %w", err)
		}
		if ok {
			startReq.SnapshotId = restore.SnapshotID
			startReq.SnapshotRef = restore.SnapshotRef
			startReq.SnapshotChecksum = restore.Checksum
		}
	}
	if !task.FreshSession && startReq.SnapshotId == "" && task.SnapshotID != "" {
		startReq.SnapshotId = task.SnapshotID
		startReq.SnapshotChecksum = task.SnapshotChecksum
	}
	if err := entry.startSession(ctx, startReq); err != nil {
		s.removeWorkerEntry(task.SessionKey, entry)
		s.cleanupWorkerEntryBestEffort(context.Background(), entry)
		return err
	}
	return nil
}

// startWorkerProcess creates the Worker after its runtime JSON and fixed
// mykey.py loader have been written.
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

// shutdownAllWorkers revokes active capability sets and tears down every
// Worker process. Called once on platform shutdown.
func (s *scheduler) shutdownAllWorkers(ctx context.Context) {
	s.mu.Lock()
	entries := make([]*workerEntry, 0, len(s.workers))
	for sessionKey, entry := range s.workers {
		entries = append(entries, entry)
		delete(s.workers, sessionKey)
	}
	s.mu.Unlock()
	for _, entry := range entries {
		entry.lifecycleMu.Lock()
		s.cleanupWorkerEntryBestEffort(ctx, entry)
		entry.lifecycleMu.Unlock()
	}
}

// stopSessionWorker evicts the Worker for a session without cancelling any
// task. Used by /new to force a fresh Worker on the next dispatch.
func (s *scheduler) stopSessionWorker(sessionKey string) {
	s.mu.Lock()
	entry := s.workers[sessionKey]
	s.mu.Unlock()
	if entry == nil {
		return
	}
	entry.lifecycleMu.Lock()
	defer entry.lifecycleMu.Unlock()
	if !s.workerEntryIsCurrent(sessionKey, entry) {
		return
	}
	s.removeWorkerEntry(sessionKey, entry)
	s.cleanupWorkerEntryBestEffort(context.Background(), entry)
}
