package application

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/domain"
	workerv1 "github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/gen/worker/v1"
	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/infrastructure/checkpoint"
	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/infrastructure/policy"
	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/infrastructure/postgres"
	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/infrastructure/workerclient"
)

func TestSchedulerConfigValidation(t *testing.T) {
	// Covered primarily in task_service_test; keep explicit package-local case.
	if _, err := NewScheduler(SchedulerConfig{}); err == nil {
		t.Fatal("expected validation error")
	}
	if _, err := NewScheduler(SchedulerConfig{PlatformInstanceID: "a", ClaimLease: -1}); err == nil {
		t.Fatal("expected negative lease error")
	}
	_ = time.Second
}

func TestCredentialReloadDoesNotHoldGlobalWorkersLock(t *testing.T) {
	reloadingWorker := newControlledWorker()
	reloadingWorker.reloadEntered = make(chan struct{})
	reloadingWorker.releaseReload = make(chan struct{})
	otherWorker := newControlledWorker()
	oldSet := workerCredentialSet{Generation: 1, Checksum: "old", JTIs: []string{"old"}}
	newSet := workerCredentialSet{Generation: 2, Checksum: "new", JTIs: []string{"new"}}
	reloadingEntry := &workerEntry{
		client: reloadingWorker, sessionKey: "personal:1", credentials: oldSet,
		pendingRefresh: &pendingCredentialRefresh{Previous: oldSet, Next: newSet},
	}
	otherEntry := &workerEntry{client: otherWorker, sessionKey: "personal:2"}
	s := &scheduler{
		workers: map[string]*workerEntry{"personal:1": reloadingEntry, "personal:2": otherEntry},
		cfg:     SchedulerConfig{},
	}
	reloadDone := make(chan error, 1)
	go func() {
		_, _, err := s.ensureWorker(context.Background(), domain.Task{SessionKey: "personal:1"})
		reloadDone <- err
	}()
	select {
	case <-reloadingWorker.reloadEntered:
	case <-time.After(time.Second):
		t.Fatal("credential reload did not start")
	}
	cancelDone := make(chan error, 1)
	go func() {
		cancelDone <- s.CancelWorker(context.Background(), domain.Task{ID: "task-2", SessionKey: "personal:2"})
	}()
	select {
	case err := <-cancelDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("unrelated session cancellation blocked behind credential reload")
	}
	close(reloadingWorker.releaseReload)
	if err := <-reloadDone; err != nil {
		t.Fatal(err)
	}
}

type fakeAgentRuntimeSettings struct {
	maxTurns int
}

func (f *fakeAgentRuntimeSettings) GetAgentMaxTurns(context.Context) (int, error) {
	return f.maxTurns, nil
}

type controlledWorker struct {
	events                 chan workerclient.WorkerEvent
	errs                   chan error
	executeStarted         chan struct{}
	streamDone             chan struct{}
	cancelObserved         chan bool
	cancelCalls            atomic.Int32
	closeOnce              sync.Once
	checkpointReady        *workerv1.CheckpointReady
	startSessionEntered    chan struct{}
	startSessionRequest    *workerv1.StartSessionRequest
	releaseStartSession    chan struct{}
	beginCheckpointEntered chan struct{}
	releaseBeginCheckpoint chan struct{}
	beginCheckpointErr     error
	reloadErr              error
	reloadRequests         []*workerv1.ReloadCredentialsRequest
	reloadEntered          chan struct{}
	releaseReload          chan struct{}
}

func newControlledWorker() *controlledWorker {
	return &controlledWorker{
		events:         make(chan workerclient.WorkerEvent, 1),
		errs:           make(chan error, 1),
		executeStarted: make(chan struct{}),
		streamDone:     make(chan struct{}),
		cancelObserved: make(chan bool, 1),
	}
}

func (w *controlledWorker) StartSession(ctx context.Context, request *workerv1.StartSessionRequest) (*workerv1.StartSessionResponse, error) {
	w.startSessionRequest = request
	if w.startSessionEntered != nil {
		close(w.startSessionEntered)
	}
	if w.releaseStartSession != nil {
		select {
		case <-w.releaseStartSession:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return &workerv1.StartSessionResponse{}, nil
}

func (w *controlledWorker) ReloadCredentials(ctx context.Context, request *workerv1.ReloadCredentialsRequest) (*workerv1.ReloadCredentialsResponse, error) {
	w.reloadRequests = append(w.reloadRequests, request)
	if w.reloadEntered != nil {
		close(w.reloadEntered)
	}
	if w.releaseReload != nil {
		select {
		case <-w.releaseReload:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if w.reloadErr != nil {
		return nil, w.reloadErr
	}
	return &workerv1.ReloadCredentialsResponse{
		CredentialGeneration: request.GetCredentialGeneration(),
		ConfigChecksum:       request.GetConfigChecksum(),
	}, nil
}

func (w *controlledWorker) ExecuteTask(context.Context, *workerv1.ExecuteTaskRequest) (<-chan workerclient.WorkerEvent, <-chan error) {
	close(w.executeStarted)
	return w.events, w.errs
}

func (w *controlledWorker) BeginCheckpoint(ctx context.Context, _ *workerv1.BeginCheckpointRequest) (*workerv1.CheckpointReady, error) {
	if w.beginCheckpointEntered != nil {
		close(w.beginCheckpointEntered)
	}
	if w.releaseBeginCheckpoint != nil {
		select {
		case <-w.releaseBeginCheckpoint:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if w.beginCheckpointErr != nil {
		return nil, w.beginCheckpointErr
	}
	if w.checkpointReady == nil {
		panic("checkpoint must not begin")
	}
	return w.checkpointReady, nil
}

func (w *controlledWorker) CancelTask(context.Context, string, string, uint64, string) error {
	w.cancelCalls.Add(1)
	select {
	case <-w.streamDone:
		w.cancelObserved <- false
	default:
		w.cancelObserved <- true
	}
	return nil
}

func (w *controlledWorker) Health(context.Context) (*workerv1.HealthResponse, error) {
	return &workerv1.HealthResponse{}, nil
}

func (w *controlledWorker) Shutdown(context.Context, string, string, uint64, string) error { return nil }

func (w *controlledWorker) succeed() {
	w.events <- workerclient.WorkerEvent{
		Kind:     workerclient.KindTerminal,
		Terminal: &workerv1.Terminal{Status: workerv1.TerminalStatus_TASK_SUCCEEDED},
	}
	w.closeOnce.Do(func() {
		close(w.events)
		close(w.errs)
		close(w.streamDone)
	})
}
func (w *controlledWorker) interrupt() {
	w.events <- workerclient.WorkerEvent{
		Kind:     workerclient.KindTerminal,
		Terminal: &workerv1.Terminal{Status: workerv1.TerminalStatus_TASK_INTERRUPTED},
	}
	w.closeOnce.Do(func() {
		close(w.events)
		close(w.errs)
		close(w.streamDone)
	})
}
func (w *controlledWorker) fail(code, message string) {
	w.events <- workerclient.WorkerEvent{
		Kind: workerclient.KindTerminal,
		Terminal: &workerv1.Terminal{
			Status:      workerv1.TerminalStatus_TASK_FAILED,
			UserMessage: message,
			Error:       &workerv1.ErrorEnvelope{Code: code, UserMessage: message},
		},
	}
	w.closeOnce.Do(func() {
		close(w.events)
		close(w.errs)
		close(w.streamDone)
	})
}
func (w *controlledWorker) failStream(err error) {
	w.errs <- err
	w.closeOnce.Do(func() {
		close(w.events)
		close(w.errs)
		close(w.streamDone)
	})
}

type successfulCoordinator struct {
	store        *postgres.Store
	owner        string
	restorePoint checkpoint.RestorePoint
	hasRestore   bool
	restoreErr   error
}

func (c *successfulCoordinator) Prepare(ctx context.Context, request checkpoint.CheckpointPrepareRequest) (checkpoint.CheckpointLease, error) {
	ref := "staging-success"
	stagingRefFor := func(snapshotID, token string, generation int64) string { return ref }
	snapshotID, token, _, err := c.store.PrepareCheckpoint(ctx, request.TaskID, c.owner, 1, stagingRefFor, request.MaxBundleBytes)
	return checkpoint.CheckpointLease{
		SnapshotID: snapshotID, Token: token, StagingRef: ref, MaxBundleBytes: request.MaxBundleBytes,
	}, err
}

func (c *successfulCoordinator) Commit(_ context.Context, ready checkpoint.ReadyCheckpoint) (checkpoint.CommittedCheckpoint, error) {
	return checkpoint.CommittedCheckpoint{
		SnapshotID: ready.SnapshotID, FileRef: "snapshot:success", Checksum: "sha256:bundle",
		ResultRef: "result:success", ResultDigest: "sha256:result",
	}, nil
}

func (c *successfulCoordinator) CurrentRestorePoint(
	context.Context,
	string,
) (checkpoint.RestorePoint, bool, error) {
	return c.restorePoint, c.hasRestore, c.restoreErr
}

func (c *successfulCoordinator) RunnerStagingRef(hostRef string) (string, error) {
	return hostRef, nil
}

func (c *successfulCoordinator) HostStagingRef(runnerRef, expectedHostRef string) (string, error) {
	if runnerRef != expectedHostRef {
		return "", fmt.Errorf("staging ref mismatch: got %q want %q", runnerRef, expectedHostRef)
	}
	return runnerRef, nil
}

func (c *successfulCoordinator) ReadResult(context.Context, string, string) (domain.ResultPayload, error) {
	return domain.ResultPayload{Ref: "result:success", Digest: "sha256:result", Body: []byte("result")}, nil
}

func (c *successfulCoordinator) SweepExpiredCheckpoints(context.Context) (int, error) {
	return 0, nil
}

type readFailCoordinator struct {
	store *postgres.Store
	owner string
}

func (c *readFailCoordinator) Prepare(ctx context.Context, request checkpoint.CheckpointPrepareRequest) (checkpoint.CheckpointLease, error) {
	ref := "staging-read-fail"
	stagingRefFor := func(snapshotID, token string, generation int64) string { return ref }
	snapshotID, token, _, err := c.store.PrepareCheckpoint(ctx, request.TaskID, c.owner, 1, stagingRefFor, request.MaxBundleBytes)
	return checkpoint.CheckpointLease{
		SnapshotID: snapshotID, Token: token, StagingRef: ref, MaxBundleBytes: request.MaxBundleBytes,
	}, err
}

func (c *readFailCoordinator) Commit(context.Context, checkpoint.ReadyCheckpoint) (checkpoint.CommittedCheckpoint, error) {
	return checkpoint.CommittedCheckpoint{
		FileRef: "snapshot:read-fail", Checksum: "sha256:bundle",
		ResultRef: "result:read-fail", ResultDigest: "sha256:result",
	}, nil
}

func (c *readFailCoordinator) CurrentRestorePoint(
	context.Context,
	string,
) (checkpoint.RestorePoint, bool, error) {
	return checkpoint.RestorePoint{}, false, nil
}

func (c *readFailCoordinator) RunnerStagingRef(hostRef string) (string, error) {
	return hostRef, nil
}

func (c *readFailCoordinator) HostStagingRef(runnerRef, expectedHostRef string) (string, error) {
	return expectedHostRef, nil
}

func (c *readFailCoordinator) ReadResult(context.Context, string, string) (domain.ResultPayload, error) {
	return domain.ResultPayload{}, errors.New("digest-checked result read failed")
}

func (c *readFailCoordinator) SweepExpiredCheckpoints(context.Context) (int, error) {
	return 0, nil
}

func testPolicyRegistry(t *testing.T) policy.Registry {
	t.Helper()
	registry, err := policy.LoadRegistry(foundationPolicy(t))
	if err != nil {
		t.Fatal(err)
	}
	return registry
}

func TestStartSessionOnWorkerPassesCurrentRestorePoint(t *testing.T) {
	registry := testPolicyRegistry(t)
	worker := newControlledWorker()
	coordinator := &successfulCoordinator{
		restorePoint: checkpoint.RestorePoint{
			SnapshotID: "snapshot-restore", SnapshotRef: "C:/runtime/committed/restore.json",
			Checksum: "sha256:restore",
		},
		hasRestore: true,
	}
	entry := &workerEntry{client: worker, sessionKey: "personal:restore"}
	s := &scheduler{
		cfg:     SchedulerConfig{Registry: registry, Coordinator: coordinator},
		workers: map[string]*workerEntry{"personal:restore": entry},
	}
	task := domain.Task{SessionKey: "personal:restore", WorkspaceID: "workspace-restore"}

	if err := s.startSessionOnWorker(context.Background(), task); err != nil {
		t.Fatal(err)
	}
	request := worker.startSessionRequest
	if request == nil || request.GetSnapshotId() != coordinator.restorePoint.SnapshotID ||
		request.GetSnapshotRef() != coordinator.restorePoint.SnapshotRef ||
		request.GetSnapshotChecksum() != coordinator.restorePoint.Checksum {
		t.Fatalf("start request=%+v restore=%+v", request, coordinator.restorePoint)
	}
	if request.GetRuntimePolicy().GetMaxTurns() != 80 {
		t.Fatalf("max turns=%d want 80", request.GetRuntimePolicy().GetMaxTurns())
	}
}

func TestStartSessionOnWorkerUsesConfiguredAgentMaxTurns(t *testing.T) {
	registry := testPolicyRegistry(t)
	worker := newControlledWorker()
	entry := &workerEntry{client: worker, sessionKey: "personal:configured"}
	s := &scheduler{
		cfg: SchedulerConfig{
			Registry:        registry,
			RuntimeSettings: &fakeAgentRuntimeSettings{maxTurns: 120},
		},
		workers: map[string]*workerEntry{"personal:configured": entry},
	}

	if err := s.startSessionOnWorker(context.Background(), domain.Task{SessionKey: "personal:configured"}); err != nil {
		t.Fatal(err)
	}
	if got := worker.startSessionRequest.GetRuntimePolicy().GetMaxTurns(); got != 120 {
		t.Fatalf("max turns=%d want 120", got)
	}
}

func TestPrepareWorkerEntryReplacesWorkerAfterAgentMaxTurnsChanges(t *testing.T) {
	settings := &fakeAgentRuntimeSettings{maxTurns: 120}
	entry := &workerEntry{started: true, runtimeMaxTurns: 80}
	s := &scheduler{cfg: SchedulerConfig{RuntimeSettings: settings}}

	replace, err := s.prepareWorkerEntry(context.Background(), entry)
	if err != nil {
		t.Fatal(err)
	}
	if !replace {
		t.Fatal("expected worker with stale max turns to be replaced")
	}
}

func TestPrepareWorkerEntryReplacesWorkerAfterMCPSnapshotChanges(t *testing.T) {
	entry := &workerEntry{credentials: workerCredentialSet{
		MCPSnapshot: RuntimeMCPSnapshot{ID: "sha256:old"},
	}}
	s := &scheduler{cfg: SchedulerConfig{MCPServer: &fakeMCPSource{servers: []domain.MCPServer{{
		ID: 1, ServerKey: "exa", Name: "Exa", URL: "https://mcp.exa.ai/mcp",
		TimeoutSeconds: 30, Enabled: true, Revision: 2,
	}}}}}

	replace, err := s.prepareWorkerEntry(context.Background(), entry)
	if err != nil {
		t.Fatal(err)
	}
	if !replace {
		t.Fatal("expected worker with stale MCP snapshot to be replaced")
	}
}

func TestScheduler_AcceptedRunningCancelReachesWorkerBeforeStreamCompletionAndWinsSuccessRace(t *testing.T) {
	_, store, reg, dev := serviceFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	task, err := store.SubmitTask(ctx, domain.SubmitTaskCommand{
		SessionKey: dev.SessionKey, RequesterUserID: dev.UserID,
		Source: "web", SourceInstanceID: "cancel-race", MessageID: "cancel-race",
		Prompt: "hold", PersonaSnapshot: []string{}, ToolPolicyVersion: "foundation.no-host-tools.v1",
	})
	if err != nil {
		t.Fatal(err)
	}
	claimed, ok, err := store.ClaimNextTask(ctx, dev.SessionKey, "scheduler-cancel", time.Second)
	if err != nil || !ok {
		t.Fatalf("claim: ok=%v err=%v", ok, err)
	}
	worker := newControlledWorker()
	schedulerAPI, err := NewScheduler(SchedulerConfig{
		PlatformInstanceID: "scheduler-cancel", ClaimLease: time.Second,
		Store: store, Registry: reg,
		DialWorker: func(context.Context, string) (workerclient.WorkerClient, func(string), error) {
			return worker, func(_ string) {}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	sched := schedulerAPI.(*scheduler)
	svc, err := NewTaskService(TaskServiceConfig{
		Store: store, Registry: reg, PlatformInstanceID: "scheduler-cancel", ClaimLease: time.Second,
		CancelWorker: sched.CancelWorker,
	})
	if err != nil {
		t.Fatal(err)
	}
	dispatchDone := make(chan error, 1)
	go func() { dispatchDone <- sched.dispatch(ctx, claimed) }()
	select {
	case <-worker.executeStarted:
	case <-ctx.Done():
		t.Fatal("worker execution did not start")
	}
	if _, err := svc.CancelTask(ctx, task.ID, dev.UserID); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.CancelTask(ctx, task.ID, dev.UserID); err != nil {
		t.Fatal(err)
	}
	select {
	case beforeCompletion := <-worker.cancelObserved:
		if !beforeCompletion {
			t.Fatal("cancel RPC arrived after stream completion")
		}
	case <-ctx.Done():
		t.Fatal("cancel RPC was not observed")
	}
	worker.succeed()
	if err := <-dispatchDone; err != nil {
		t.Fatal(err)
	}
	if got := worker.cancelCalls.Load(); got != 1 {
		t.Fatalf("cancel RPC count=%d want 1", got)
	}
	final, err := store.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if final.Status != domain.TaskInterrupted {
		t.Fatalf("status=%s want interrupted", final.Status)
	}
	if _, err := store.GetDelivery(ctx, task.ID, domain.DeliveryTaskInterrupted); err != nil {
		t.Fatal(err)
	}
}

func TestSchedulerDeadlineCancelsWorkerAndCannotSucceed(t *testing.T) {
	_, store, reg, dev := serviceFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	task, err := store.SubmitTask(ctx, domain.SubmitTaskCommand{
		SessionKey: dev.SessionKey, RequesterUserID: dev.UserID,
		Source: "web", SourceInstanceID: "deadline", MessageID: "deadline",
		Prompt: "hold", PersonaSnapshot: []string{}, ToolPolicyVersion: "foundation.no-host-tools.v1",
	})
	if err != nil {
		t.Fatal(err)
	}
	claimed, ok, err := store.ClaimNextTask(ctx, dev.SessionKey, "deadline-owner", time.Second)
	if err != nil || !ok {
		t.Fatalf("claim: ok=%v err=%v", ok, err)
	}
	worker := newControlledWorker()
	schedulerAPI, err := NewScheduler(SchedulerConfig{
		PlatformInstanceID: "deadline-owner", ClaimLease: time.Second,
		Store: store, Registry: reg, MaxTaskWallClock: 50 * time.Millisecond,
		TokenTTL: time.Hour,
		DialWorker: func(context.Context, string) (workerclient.WorkerClient, func(string), error) {
			return worker, func(_ string) {}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	err = schedulerAPI.(*scheduler).dispatch(ctx, claimed)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("dispatch error=%v", err)
	}
	if worker.cancelCalls.Load() != 1 {
		t.Fatalf("cancel calls=%d", worker.cancelCalls.Load())
	}
	final, err := store.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if final.Status != domain.TaskFailed || final.TerminalErrorCode != "TASK_DEADLINE_EXCEEDED" {
		t.Fatalf("status=%s code=%s", final.Status, final.TerminalErrorCode)
	}
}

func TestScheduler_HeartbeatsQuietStreamBeforeLeaseExpires(t *testing.T) {
	_, store, reg, dev := serviceFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	task, err := store.SubmitTask(ctx, domain.SubmitTaskCommand{
		SessionKey: dev.SessionKey, RequesterUserID: dev.UserID,
		Source: "web", SourceInstanceID: "quiet", MessageID: "quiet",
		Prompt: "quiet", PersonaSnapshot: []string{}, ToolPolicyVersion: "foundation.no-host-tools.v1",
	})
	if err != nil {
		t.Fatal(err)
	}
	lease := 2 * time.Second
	claimed, ok, err := store.ClaimNextTask(ctx, dev.SessionKey, "quiet-owner", lease)
	if err != nil || !ok {
		t.Fatalf("claim: ok=%v err=%v", ok, err)
	}
	worker := newControlledWorker()
	schedulerAPI, err := NewScheduler(SchedulerConfig{
		PlatformInstanceID: "quiet-owner", ClaimLease: lease,
		Store: store, Registry: reg,
		DialWorker: func(context.Context, string) (workerclient.WorkerClient, func(string), error) {
			return worker, func(_ string) {}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	sched := schedulerAPI.(*scheduler)
	dispatchDone := make(chan error, 1)
	go func() { dispatchDone <- sched.dispatch(ctx, claimed) }()
	select {
	case <-worker.executeStarted:
	case <-ctx.Done():
		t.Fatal("worker execution did not start")
	}
	var beforeRecovery domain.Task
	deadline := time.Now().Add(5 * time.Second)
	for {
		var databaseNow time.Time
		if err := store.Pool().QueryRow(ctx, `SELECT timezone('utc', now())`).Scan(&databaseNow); err != nil {
			t.Fatal(err)
		}
		beforeRecovery, err = store.GetTask(ctx, task.ID)
		if err != nil {
			t.Fatal(err)
		}
		if databaseNow.After(claimed.ClaimLeaseUntil) && beforeRecovery.ClaimLeaseUntil.After(databaseNow.Add(lease/3)) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("claim lease did not retain margin: initial=%s current=%s database_now=%s", claimed.ClaimLeaseUntil, beforeRecovery.ClaimLeaseUntil, databaseNow)
		}
		time.Sleep(25 * time.Millisecond)
	}
	recovered, err := store.RecoverAfterRestart(ctx, "competing-owner")
	if err != nil {
		t.Fatal(err)
	}
	if recovered != 0 {
		t.Fatalf("competing recovery interrupted %d quiet task(s)", recovered)
	}
	current, err := store.GetTask(ctx, task.ID)

	if err != nil {
		t.Fatal(err)
	}
	if current.Status != domain.TaskRunning {
		t.Fatalf("status=%s want running", current.Status)
	}
	worker.interrupt()
	if err := <-dispatchDone; err != nil {
		t.Fatal(err)
	}
}

func TestScheduler_MissingWorkerDuringCheckpointFinalizesFailure(t *testing.T) {
	_, store, _, dev := serviceFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	task, err := store.SubmitTask(ctx, domain.SubmitTaskCommand{
		SessionKey: dev.SessionKey, RequesterUserID: dev.UserID,
		Source: "web", SourceInstanceID: "missing-worker", MessageID: "missing-worker",
		Prompt: "checkpoint", PersonaSnapshot: []string{}, ToolPolicyVersion: "foundation.no-host-tools.v1",
	})
	if err != nil {
		t.Fatal(err)
	}
	claimed, ok, err := store.ClaimNextTask(ctx, dev.SessionKey, "missing-worker-owner", time.Second)
	if err != nil || !ok {
		t.Fatalf("claim: ok=%v err=%v", ok, err)
	}
	if _, err := store.MarkDispatchStarted(ctx, task.ID, "missing-worker-owner", "worker-gone", false); err != nil {
		t.Fatal(err)
	}
	if _, err := store.MarkRunning(ctx, task.ID, "missing-worker-owner"); err != nil {
		t.Fatal(err)
	}
	s := &scheduler{
		cfg: SchedulerConfig{
			PlatformInstanceID: "missing-worker-owner", Store: store,
			Coordinator:    &successfulCoordinator{store: store, owner: "missing-worker-owner"},
			MaxBundleBytes: 2 * 1024 * 1024,
		},
		workers: make(map[string]*workerEntry), wake: make(chan struct{}, 1),
	}
	err = s.completeSuccess(ctx, claimed, &workerv1.Terminal{Status: workerv1.TerminalStatus_TASK_SUCCEEDED})
	if err == nil {
		t.Fatal("expected missing Worker error")
	}
	final, err := store.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if final.Status != domain.TaskFailed || final.TerminalErrorCode != "CHECKPOINT_WORKER_MISSING" {
		t.Fatalf("status=%s code=%s", final.Status, final.TerminalErrorCode)
	}
}

func TestScheduler_ResultReadFailureCannotCommitSucceeded(t *testing.T) {
	_, store, reg, dev := serviceFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	task, err := store.SubmitTask(ctx, domain.SubmitTaskCommand{
		SessionKey: dev.SessionKey, RequesterUserID: dev.UserID,
		Source: "web", SourceInstanceID: "read-fail", MessageID: "read-fail",
		Prompt: "read", PersonaSnapshot: []string{}, ToolPolicyVersion: "foundation.no-host-tools.v1",
	})
	if err != nil {
		t.Fatal(err)
	}
	claimed, ok, err := store.ClaimNextTask(ctx, dev.SessionKey, "read-owner", time.Second)
	if err != nil || !ok {
		t.Fatalf("claim: ok=%v err=%v", ok, err)
	}
	worker := newControlledWorker()
	worker.checkpointReady = &workerv1.CheckpointReady{
		StagingRef: "staging-read-fail", Checksum: "sha256:bundle", ResultDigest: "sha256:result",
	}
	coord := &readFailCoordinator{store: store, owner: "read-owner"}
	schedulerAPI, err := NewScheduler(SchedulerConfig{
		PlatformInstanceID: "read-owner", ClaimLease: time.Second,
		Store: store, Registry: reg, Coordinator: coord,
		DialWorker: func(context.Context, string) (workerclient.WorkerClient, func(string), error) {
			return worker, func(_ string) {}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	sched := schedulerAPI.(*scheduler)
	dispatchDone := make(chan error, 1)
	go func() { dispatchDone <- sched.dispatch(ctx, claimed) }()
	select {
	case <-worker.executeStarted:
	case <-ctx.Done():
		t.Fatal("worker execution did not start")
	}
	worker.succeed()
	if err := <-dispatchDone; err == nil {
		t.Fatal("expected result read failure")
	}
	final, err := store.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if final.Status != domain.TaskFailed || final.TerminalErrorCode != "RESULT_READ_FAILED" {
		t.Fatalf("status=%s code=%s", final.Status, final.TerminalErrorCode)
	}
	if _, err := store.GetDelivery(ctx, task.ID, domain.DeliveryTaskComplete); err == nil {
		t.Fatal("unexpected task_complete delivery")
	}
}

func TestScheduler_HeartbeatLossCancelsWorkerAndCannotSucceed(t *testing.T) {
	_, store, reg, dev := serviceFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	task, err := store.SubmitTask(ctx, domain.SubmitTaskCommand{
		SessionKey: dev.SessionKey, RequesterUserID: dev.UserID,
		Source: "web", SourceInstanceID: "heartbeat-loss", MessageID: "heartbeat-loss",
		Prompt: "quiet", PersonaSnapshot: []string{}, ToolPolicyVersion: "foundation.no-host-tools.v1",
	})
	if err != nil {
		t.Fatal(err)
	}
	claimed, ok, err := store.ClaimNextTask(ctx, dev.SessionKey, "heartbeat-owner", 600*time.Millisecond)
	if err != nil || !ok {
		t.Fatalf("claim: ok=%v err=%v", ok, err)
	}
	worker := newControlledWorker()
	schedulerAPI, err := NewScheduler(SchedulerConfig{
		PlatformInstanceID: "heartbeat-owner", ClaimLease: 600 * time.Millisecond,
		Store: store, Registry: reg,
		DialWorker: func(context.Context, string) (workerclient.WorkerClient, func(string), error) {
			return worker, func(_ string) {}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	sched := schedulerAPI.(*scheduler)
	dispatchDone := make(chan error, 1)
	go func() { dispatchDone <- sched.dispatch(ctx, claimed) }()
	select {
	case <-worker.executeStarted:
	case <-ctx.Done():
		t.Fatal("worker execution did not start")
	}
	if _, err := store.Pool().Exec(ctx, `
UPDATE tasks SET claim_owner='stolen-owner', claim_lease_until=timezone('utc', now()) + interval '10 minutes'
WHERE id=$1
`, task.ID); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-dispatchDone:
		if err == nil {
			t.Fatal("expected heartbeat failure")
		}
	case <-ctx.Done():
		t.Fatal("dispatch did not fail closed after heartbeat loss")
	}
	if got := worker.cancelCalls.Load(); got != 1 {
		t.Fatalf("worker cancel calls=%d want 1", got)
	}
	final, err := store.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if final.Status == domain.TaskSucceeded {
		t.Fatal("heartbeat loss committed success")
	}
	if _, err := store.GetDelivery(ctx, task.ID, domain.DeliveryTaskComplete); err == nil {
		t.Fatal("unexpected task_complete delivery")
	}
}

func TestScheduler_CancelDuringStartSessionSkipsExecuteTaskAndSendsOneRPC(t *testing.T) {
	_, store, reg, dev := serviceFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	task, err := store.SubmitTask(ctx, domain.SubmitTaskCommand{
		SessionKey: dev.SessionKey, RequesterUserID: dev.UserID,
		Source: "web", SourceInstanceID: "cancel-before-execute", MessageID: "cancel-before-execute",
		Prompt: "must not execute", PersonaSnapshot: []string{}, ToolPolicyVersion: "foundation.no-host-tools.v1",
	})
	if err != nil {
		t.Fatal(err)
	}
	claimed, ok, err := store.ClaimNextTask(ctx, dev.SessionKey, "pre-execute-owner", time.Second)
	if err != nil || !ok {
		t.Fatalf("claim: ok=%v err=%v", ok, err)
	}
	worker := newControlledWorker()
	worker.startSessionEntered = make(chan struct{})
	worker.releaseStartSession = make(chan struct{})
	schedulerAPI, err := NewScheduler(SchedulerConfig{
		PlatformInstanceID: "pre-execute-owner", ClaimLease: time.Second,
		Store: store, Registry: reg,
		DialWorker: func(context.Context, string) (workerclient.WorkerClient, func(string), error) {
			return worker, func(_ string) {}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	sched := schedulerAPI.(*scheduler)
	svc, err := NewTaskService(TaskServiceConfig{
		Store: store, Registry: reg, PlatformInstanceID: "pre-execute-owner", ClaimLease: time.Second,
		CancelWorker: sched.CancelWorker,
	})
	if err != nil {
		t.Fatal(err)
	}
	dispatchDone := make(chan error, 1)
	go func() { dispatchDone <- sched.dispatch(ctx, claimed) }()
	select {
	case <-worker.startSessionEntered:
	case <-ctx.Done():
		t.Fatal("StartSession did not begin")
	}
	cancelDone := make(chan error, 1)
	go func() {
		_, err := svc.CancelTask(ctx, task.ID, dev.UserID)
		cancelDone <- err
	}()
	deadline := time.Now().Add(2 * time.Second)
	for {
		current, err := store.GetTask(ctx, task.ID)
		if err != nil {
			t.Fatal(err)
		}
		if current.CancelRequestedAt != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("durable cancel request was not recorded")
		}
		time.Sleep(10 * time.Millisecond)
	}
	close(worker.releaseStartSession)
	if err := <-cancelDone; err != nil {
		t.Fatal(err)
	}
	select {
	case <-worker.executeStarted:
		worker.succeed()
		<-dispatchDone
		t.Fatal("ExecuteTask started after durable cancellation")
	case err := <-dispatchDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("dispatch did not finish")
	}
	// 取消在 StartSession 期间被接受: dispatch 检测到 cancel_requested_at 后
	// 直接销毁 Worker 重建(内存状态未提交, 不复用), 不再发 cancel RPC
	// (进程已被销毁, RPC 无意义); CancelWorker 观察到 entry 已移除视为已取消。
	if got := worker.cancelCalls.Load(); got != 0 {
		t.Fatalf("cancel RPC count=%d want 0 (worker destroyed instead)", got)
	}
	final, err := store.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if final.Status != domain.TaskInterrupted {
		t.Fatalf("status=%s want interrupted", final.Status)
	}
}

func TestScheduler_HeartbeatsWhileStartSessionIsBlocked(t *testing.T) {
	_, store, reg, dev := serviceFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	task, err := store.SubmitTask(ctx, domain.SubmitTaskCommand{
		SessionKey: dev.SessionKey, RequesterUserID: dev.UserID,
		Source: "web", SourceInstanceID: "blocked-start", MessageID: "blocked-start",
		Prompt: "blocked", PersonaSnapshot: []string{}, ToolPolicyVersion: "foundation.no-host-tools.v1",
	})
	if err != nil {
		t.Fatal(err)
	}
	lease := 600 * time.Millisecond
	claimed, ok, err := store.ClaimNextTask(ctx, dev.SessionKey, "blocked-start-owner", lease)
	if err != nil || !ok {
		t.Fatalf("claim: ok=%v err=%v", ok, err)
	}
	worker := newControlledWorker()
	worker.startSessionEntered = make(chan struct{})
	worker.releaseStartSession = make(chan struct{})
	schedulerAPI, err := NewScheduler(SchedulerConfig{
		PlatformInstanceID: "blocked-start-owner", ClaimLease: lease, Store: store, Registry: reg,
		DialWorker: func(context.Context, string) (workerclient.WorkerClient, func(string), error) {
			return worker, func(_ string) {}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	sched := schedulerAPI.(*scheduler)
	dispatchDone := make(chan error, 1)
	go func() { dispatchDone <- sched.dispatch(ctx, claimed) }()
	select {
	case <-worker.startSessionEntered:
	case <-ctx.Done():
		t.Fatal("StartSession did not block")
	}
	time.Sleep(lease + lease/2)
	current, readErr := store.GetTask(ctx, task.ID)
	recovered, recoverErr := store.RecoverAfterRestart(ctx, "competing-start-owner")
	close(worker.releaseStartSession)
	select {
	case <-worker.executeStarted:
		worker.interrupt()
	case err := <-dispatchDone:
		if err != nil && recoverErr == nil && recovered == 0 {
			t.Fatalf("dispatch: %v", err)
		}
	}
	if readErr != nil {
		t.Fatal(readErr)
	}
	if !current.ClaimLeaseUntil.After(claimed.ClaimLeaseUntil) {
		t.Fatalf("lease did not advance while StartSession blocked: initial=%s current=%s", claimed.ClaimLeaseUntil, current.ClaimLeaseUntil)
	}
	if recoverErr != nil {
		t.Fatal(recoverErr)
	}
	if recovered != 0 {
		t.Fatalf("competing recovery interrupted %d blocked StartSession task(s)", recovered)
	}
	select {
	case err := <-dispatchDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("dispatch did not finish")
	}
}

func TestScheduler_HeartbeatsThroughBlockedCheckpointCommit(t *testing.T) {
	_, store, reg, dev := serviceFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	task, err := store.SubmitTask(ctx, domain.SubmitTaskCommand{
		SessionKey: dev.SessionKey, RequesterUserID: dev.UserID,
		Source: "web", SourceInstanceID: "blocked-checkpoint", MessageID: "blocked-checkpoint",
		Prompt: "checkpoint", PersonaSnapshot: []string{}, ToolPolicyVersion: "foundation.no-host-tools.v1",
	})
	if err != nil {
		t.Fatal(err)
	}
	lease := 600 * time.Millisecond
	claimed, ok, err := store.ClaimNextTask(ctx, dev.SessionKey, "checkpoint-owner", lease)
	if err != nil || !ok {
		t.Fatalf("claim: ok=%v err=%v", ok, err)
	}
	worker := newControlledWorker()
	worker.checkpointReady = &workerv1.CheckpointReady{StagingRef: "staging-success", Checksum: "sha256:bundle", ResultDigest: "sha256:result"}
	worker.beginCheckpointEntered = make(chan struct{})
	worker.releaseBeginCheckpoint = make(chan struct{})
	coord := &successfulCoordinator{store: store, owner: "checkpoint-owner"}
	schedulerAPI, err := NewScheduler(SchedulerConfig{
		PlatformInstanceID: "checkpoint-owner", ClaimLease: lease, Store: store, Registry: reg, Coordinator: coord,
		DialWorker: func(context.Context, string) (workerclient.WorkerClient, func(string), error) {
			return worker, func(_ string) {}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	sched := schedulerAPI.(*scheduler)
	dispatchDone := make(chan error, 1)
	go func() { dispatchDone <- sched.dispatch(ctx, claimed) }()
	<-worker.executeStarted
	worker.succeed()
	select {
	case <-worker.beginCheckpointEntered:
	case <-ctx.Done():
		t.Fatal("BeginCheckpoint did not block")
	}
	time.Sleep(lease + lease/2)
	current, readErr := store.GetTask(ctx, task.ID)
	recovered, recoverErr := store.RecoverAfterRestart(ctx, "competing-checkpoint-owner")
	close(worker.releaseBeginCheckpoint)
	dispatchErr := <-dispatchDone
	if readErr != nil {
		t.Fatal(readErr)
	}
	if !current.ClaimLeaseUntil.After(claimed.ClaimLeaseUntil) {
		t.Fatalf("lease did not advance through checkpoint: initial=%s current=%s", claimed.ClaimLeaseUntil, current.ClaimLeaseUntil)
	}
	if recoverErr != nil {
		t.Fatal(recoverErr)
	}
	if recovered != 0 {
		t.Fatalf("competing recovery interrupted %d checkpoint task(s)", recovered)
	}
	if dispatchErr != nil {
		t.Fatal(dispatchErr)
	}
	final, err := store.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if final.Status != domain.TaskSucceeded {
		t.Fatalf("status=%s want succeeded", final.Status)
	}
}

func TestScheduler_HeartbeatLossDuringCheckpointCannotPublishSuccess(t *testing.T) {
	_, store, reg, dev := serviceFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	task, err := store.SubmitTask(ctx, domain.SubmitTaskCommand{
		SessionKey: dev.SessionKey, RequesterUserID: dev.UserID,
		Source: "web", SourceInstanceID: "checkpoint-heartbeat-loss", MessageID: "checkpoint-heartbeat-loss",
		Prompt: "checkpoint", PersonaSnapshot: []string{}, ToolPolicyVersion: "foundation.no-host-tools.v1",
	})
	if err != nil {
		t.Fatal(err)
	}
	lease := 600 * time.Millisecond
	claimed, ok, err := store.ClaimNextTask(ctx, dev.SessionKey, "checkpoint-loss-owner", lease)
	if err != nil || !ok {
		t.Fatalf("claim: ok=%v err=%v", ok, err)
	}
	worker := newControlledWorker()
	worker.checkpointReady = &workerv1.CheckpointReady{StagingRef: "staging-success", Checksum: "sha256:bundle", ResultDigest: "sha256:result"}
	worker.beginCheckpointEntered = make(chan struct{})
	worker.releaseBeginCheckpoint = make(chan struct{})
	coord := &successfulCoordinator{store: store, owner: "checkpoint-loss-owner"}
	schedulerAPI, err := NewScheduler(SchedulerConfig{
		PlatformInstanceID: "checkpoint-loss-owner", ClaimLease: lease, Store: store, Registry: reg, Coordinator: coord,
		DialWorker: func(context.Context, string) (workerclient.WorkerClient, func(string), error) {
			return worker, func(_ string) {}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	sched := schedulerAPI.(*scheduler)
	dispatchDone := make(chan error, 1)
	go func() { dispatchDone <- sched.dispatch(ctx, claimed) }()
	<-worker.executeStarted
	worker.succeed()
	<-worker.beginCheckpointEntered
	if _, err := store.Pool().Exec(ctx, `
UPDATE tasks SET claim_owner='stolen-checkpoint-owner', claim_lease_until=timezone('utc', now()) + interval '10 minutes'
WHERE id=$1
`, task.ID); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-dispatchDone:
		if err == nil {
			t.Fatal("expected checkpoint heartbeat failure")
		}
	case <-time.After(2 * lease):
		close(worker.releaseBeginCheckpoint)
		<-dispatchDone
		t.Fatal("dispatch did not fail closed during checkpoint heartbeat loss")
	}
	final, err := store.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if final.Status == domain.TaskSucceeded {
		t.Fatal("heartbeat loss published success")
	}
	if _, err := store.GetDelivery(ctx, task.ID, domain.DeliveryTaskComplete); err == nil {
		t.Fatal("unexpected task_complete delivery")
	}
}

func TestScheduler_MaxTurnsExceededCommitsFailedAndCreatesFailedDelivery(t *testing.T) {
	_, store, reg, dev := serviceFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	task, err := store.SubmitTask(ctx, domain.SubmitTaskCommand{
		SessionKey: dev.SessionKey, RequesterUserID: dev.UserID,
		Source: "web", SourceInstanceID: "max-turns", MessageID: "max-turns",
		Prompt: "long task", PersonaSnapshot: []string{}, ToolPolicyVersion: "foundation.no-host-tools.v1",
	})
	if err != nil {
		t.Fatal(err)
	}
	claimed, ok, err := store.ClaimNextTask(ctx, dev.SessionKey, "max-turns-owner", time.Second)
	if err != nil || !ok {
		t.Fatalf("claim: ok=%v err=%v", ok, err)
	}
	worker := newControlledWorker()
	schedulerAPI, err := NewScheduler(SchedulerConfig{
		PlatformInstanceID: "max-turns-owner", ClaimLease: time.Second, Store: store, Registry: reg,
		DialWorker: func(context.Context, string) (workerclient.WorkerClient, func(string), error) {
			return worker, func(_ string) {}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	dispatchDone := make(chan error, 1)
	go func() { dispatchDone <- schedulerAPI.(*scheduler).dispatch(ctx, claimed) }()
	<-worker.executeStarted
	worker.fail("MAX_TURNS_EXCEEDED", "agent reached configured turn limit (80) before completing the task")
	if err := <-dispatchDone; err != nil {
		t.Fatal(err)
	}

	final, err := store.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if final.Status != domain.TaskFailed || final.TerminalErrorCode != "MAX_TURNS_EXCEEDED" {
		t.Fatalf("status=%s code=%s", final.Status, final.TerminalErrorCode)
	}
	if _, err := store.GetDelivery(ctx, task.ID, domain.DeliveryTaskFailed); err != nil {
		t.Fatal(err)
	}
	if _, err := store.GetDelivery(ctx, task.ID, domain.DeliveryTaskComplete); err == nil {
		t.Fatal("unexpected task_complete delivery")
	}
}

func TestScheduler_CancelRacingStreamErrorCommitsInterrupted(t *testing.T) {
	_, store, reg, dev := serviceFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	task, err := store.SubmitTask(ctx, domain.SubmitTaskCommand{
		SessionKey: dev.SessionKey, RequesterUserID: dev.UserID,
		Source: "web", SourceInstanceID: "cancel-stream-error", MessageID: "cancel-stream-error",
		Prompt: "stream", PersonaSnapshot: []string{}, ToolPolicyVersion: "foundation.no-host-tools.v1",
	})
	if err != nil {
		t.Fatal(err)
	}
	claimed, ok, err := store.ClaimNextTask(ctx, dev.SessionKey, "cancel-stream-owner", time.Second)
	if err != nil || !ok {
		t.Fatalf("claim: ok=%v err=%v", ok, err)
	}
	worker := newControlledWorker()
	schedulerAPI, err := NewScheduler(SchedulerConfig{
		PlatformInstanceID: "cancel-stream-owner", ClaimLease: time.Second, Store: store, Registry: reg,
		DialWorker: func(context.Context, string) (workerclient.WorkerClient, func(string), error) {
			return worker, func(_ string) {}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	sched := schedulerAPI.(*scheduler)
	svc, err := NewTaskService(TaskServiceConfig{
		Store: store, Registry: reg, PlatformInstanceID: "cancel-stream-owner", ClaimLease: time.Second,
		CancelWorker: sched.CancelWorker,
	})
	if err != nil {
		t.Fatal(err)
	}
	dispatchDone := make(chan error, 1)
	go func() { dispatchDone <- sched.dispatch(ctx, claimed) }()
	<-worker.executeStarted
	if _, err := svc.CancelTask(ctx, task.ID, dev.UserID); err != nil {
		t.Fatal(err)
	}
	<-worker.cancelObserved
	worker.failStream(errors.New("stream transport failed"))
	if err := <-dispatchDone; err == nil {
		t.Fatal("expected stream error")
	}
	final, err := store.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if final.Status != domain.TaskInterrupted || final.TerminalErrorCode != "TASK_INTERRUPTED" {
		t.Fatalf("status=%s code=%s", final.Status, final.TerminalErrorCode)
	}
	if _, err := store.GetDelivery(ctx, task.ID, domain.DeliveryTaskInterrupted); err != nil {
		t.Fatal(err)
	}
	if _, err := store.GetDelivery(ctx, task.ID, domain.DeliveryTaskFailed); err == nil {
		t.Fatal("unexpected task_failed delivery")
	}
}

func TestScheduler_CancelRacingCheckpointErrorCommitsInterrupted(t *testing.T) {
	_, store, reg, dev := serviceFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	task, err := store.SubmitTask(ctx, domain.SubmitTaskCommand{
		SessionKey: dev.SessionKey, RequesterUserID: dev.UserID,
		Source: "web", SourceInstanceID: "cancel-checkpoint-error", MessageID: "cancel-checkpoint-error",
		Prompt: "checkpoint", PersonaSnapshot: []string{}, ToolPolicyVersion: "foundation.no-host-tools.v1",
	})
	if err != nil {
		t.Fatal(err)
	}
	claimed, ok, err := store.ClaimNextTask(ctx, dev.SessionKey, "cancel-checkpoint-owner", time.Second)
	if err != nil || !ok {
		t.Fatalf("claim: ok=%v err=%v", ok, err)
	}
	worker := newControlledWorker()
	worker.beginCheckpointEntered = make(chan struct{})
	worker.releaseBeginCheckpoint = make(chan struct{})
	worker.beginCheckpointErr = errors.New("checkpoint failed")
	coord := &successfulCoordinator{store: store, owner: "cancel-checkpoint-owner"}
	schedulerAPI, err := NewScheduler(SchedulerConfig{
		PlatformInstanceID: "cancel-checkpoint-owner", ClaimLease: time.Second,
		Store: store, Registry: reg, Coordinator: coord,
		DialWorker: func(context.Context, string) (workerclient.WorkerClient, func(string), error) {
			return worker, func(_ string) {}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	sched := schedulerAPI.(*scheduler)
	svc, err := NewTaskService(TaskServiceConfig{
		Store: store, Registry: reg, PlatformInstanceID: "cancel-checkpoint-owner", ClaimLease: time.Second,
		CancelWorker: sched.CancelWorker,
	})
	if err != nil {
		t.Fatal(err)
	}
	dispatchDone := make(chan error, 1)
	go func() { dispatchDone <- sched.dispatch(ctx, claimed) }()
	<-worker.executeStarted
	worker.succeed()
	<-worker.beginCheckpointEntered
	if _, err := svc.CancelTask(ctx, task.ID, dev.UserID); err != nil {
		t.Fatal(err)
	}
	close(worker.releaseBeginCheckpoint)
	if err := <-dispatchDone; err == nil {
		t.Fatal("expected checkpoint error")
	}
	final, err := store.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if final.Status != domain.TaskInterrupted || final.TerminalErrorCode != "TASK_INTERRUPTED" {
		t.Fatalf("status=%s code=%s", final.Status, final.TerminalErrorCode)
	}
	if _, err := store.GetDelivery(ctx, task.ID, domain.DeliveryTaskInterrupted); err != nil {
		t.Fatal(err)
	}
	if _, err := store.GetDelivery(ctx, task.ID, domain.DeliveryTaskFailed); err == nil {
		t.Fatal("unexpected task_failed delivery")
	}
}

func TestShouldReapIdleTask(t *testing.T) {
	now := time.Now().UTC()
	cutoff := now.Add(-5 * time.Minute)
	dispatchRecent := now.Add(-1 * time.Minute)
	dispatchStale := now.Add(-10 * time.Minute)
	activityRecent := now.Add(-1 * time.Minute)
	activityStale := now.Add(-10 * time.Minute)

	tests := []struct {
		name string
		task domain.Task
		want bool
	}{
		{
			name: "non-running task never reaped",
			task: domain.Task{Status: domain.TaskQueued, LastActivityAt: activityStale},
			want: false,
		},
		{
			name: "active task with recent activity not reaped",
			task: domain.Task{Status: domain.TaskRunning, LastActivityAt: activityRecent},
			want: false,
		},
		{
			name: "running task with stale activity reaped",
			task: domain.Task{Status: domain.TaskRunning, LastActivityAt: activityStale},
			want: true,
		},
		{
			name: "cold-start with nil dispatch not reaped",
			task: domain.Task{Status: domain.TaskRunning, LastActivityAt: time.Time{}},
			want: false,
		},
		{
			name: "cold-start with recent dispatch not reaped (grace period)",
			task: domain.Task{
				Status:                  domain.TaskRunning,
				LastActivityAt:          time.Time{},
				WorkerDispatchStartedAt: &dispatchRecent,
			},
			want: false,
		},
		{
			name: "cold-start with stale dispatch reaped (Worker stuck before first chunk)",
			task: domain.Task{
				Status:                  domain.TaskRunning,
				LastActivityAt:          time.Time{},
				WorkerDispatchStartedAt: &dispatchStale,
			},
			want: true,
		},
		{
			name: "exactly at cutoff boundary is reaped (After is exclusive)",
			task: domain.Task{Status: domain.TaskRunning, LastActivityAt: cutoff},
			want: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := shouldReapIdleTask(tt.task, cutoff); got != tt.want {
				t.Fatalf("shouldReapIdleTask = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestFormatActivityTime(t *testing.T) {
	ts := time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)
	dispatch := time.Date(2026, 7, 26, 11, 55, 0, 0, time.UTC)

	if got := formatActivityTime(ts, &dispatch); got != "2026-07-26T12:00:00Z" {
		t.Fatalf("non-zero activity: got %q", got)
	}
	if got := formatActivityTime(time.Time{}, &dispatch); got != "dispatch:2026-07-26T11:55:00Z" {
		t.Fatalf("zero activity with dispatch: got %q", got)
	}
	if got := formatActivityTime(time.Time{}, nil); got != "unknown" {
		t.Fatalf("zero activity without dispatch: got %q", got)
	}
}

type blockedShutdownWorker struct {
	*controlledWorker
	deadlineObserved chan struct{}
}

func (w *blockedShutdownWorker) Shutdown(ctx context.Context, _ string, _ string, _ uint64, _ string) error {
	<-ctx.Done()
	close(w.deadlineObserved)
	return ctx.Err()
}

// TestSchedulerGlobalCapacityKeepsTasksQueued verifies the GA_RUNNER_MAX_ACTIVE
// contract: when the global running cap is reached, claimable tasks stay queued
// (never finalized as failed) and are claimed once capacity frees up.
func TestSchedulerGlobalCapacityKeepsTasksQueued(t *testing.T) {
	_, store, reg, dev := serviceFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	// Two sessions, one task each, so per-tenant limits don't interfere.
	dev2, err := store.EnsureDevelopmentContext(ctx, 2, "dev2")
	if err != nil {
		t.Fatal(err)
	}
	first, err := store.SubmitTask(ctx, domain.SubmitTaskCommand{
		SessionKey: dev.SessionKey, RequesterUserID: 1,
		Source: "web", SourceInstanceID: "cap-hold", MessageID: "cap-hold-1",
		Prompt: "hold", PersonaSnapshot: []string{}, ToolPolicyVersion: "foundation.no-host-tools.v1",
	})
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.SubmitTask(ctx, domain.SubmitTaskCommand{
		SessionKey: dev2.SessionKey, RequesterUserID: 2,
		Source: "web", SourceInstanceID: "cap-hold", MessageID: "cap-hold-2",
		Prompt: "hold", PersonaSnapshot: []string{}, ToolPolicyVersion: "foundation.no-host-tools.v1",
	})
	if err != nil {
		t.Fatal(err)
	}

	// Claim the first task and hold it (simulating an in-flight Runner).
	claimedFirst, ok, err := store.ClaimNextTask(ctx, dev.SessionKey, "cap-owner", time.Minute)
	if err != nil || !ok {
		t.Fatalf("claim first: ok=%v err=%v", ok, err)
	}
	_ = claimedFirst

	// Second task must remain queued (not claimed) while the first task occupies
	// the only capacity slot. ClaimNextTask would move it to starting, so we
	// must NOT claim it manually — the tick loop's capacity gate decides.
	second, err = store.GetTask(ctx, second.ID)
	if err != nil {
		t.Fatal(err)
	}
	if second.Status != domain.TaskQueued {
		t.Fatalf("second task status = %s, want queued before capacity tick", second.Status)
	}

	// The tick loop with MaxRunningTasks=1 must not claim the second task
	// (CountRunningTasks >= cap), and must not finalize it as failed.
	sched, err := NewScheduler(SchedulerConfig{
		PlatformInstanceID: "cap-owner", ClaimLease: time.Minute,
		Store: store, Registry: reg,
		MaxRunningTasks: 1,
		DialWorker: func(context.Context, string) (workerclient.WorkerClient, func(string), error) {
			return newControlledWorker(), func(_ string) {}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	// Mark first as running so CountRunningTasks sees 1 (claim alone leaves it starting).
	if _, err := store.MarkDispatchStarted(ctx, first.ID, "cap-owner", "cap-worker", false); err != nil {
		t.Fatal(err)
	}
	if err := sched.(*scheduler).tick(ctx); err != nil {
		t.Fatalf("tick at capacity: %v", err)
	}
	after, err := store.GetTask(ctx, second.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Status != domain.TaskQueued {
		t.Fatalf("second task status = %s after capacity tick, want queued (not failed)", after.Status)
	}

	// Release the running slot via cancellation; the queued task is claimable again.
	if _, _, err := store.CancelTask(ctx, first.ID, 1); err != nil {
		t.Fatal(err)
	}
	claimed, ok, err := store.ClaimNextTask(ctx, dev2.SessionKey, "cap-owner", time.Minute)
	if err != nil || !ok {
		t.Fatalf("claim after release: ok=%v err=%v", ok, err)
	}
	if claimed.ID != second.ID {
		t.Fatalf("claimed %s, want %s", claimed.ID, second.ID)
	}
}

// TestSchedulerRunnerKeySerialExecution verifies the runner_key contract: two
// tasks under the same runner_key (personal:<uid>) are serial — the second
// stays queued while the first is starting/running, and becomes claimable only
// after the first terminal. The scheduler never creates a second Worker for
// the same runner_key (workerEntry cache is keyed by SessionKey == runner_key).
func TestSchedulerRunnerKeySerialExecution(t *testing.T) {
	_, store, _, dev := serviceFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	first, err := store.SubmitTask(ctx, domain.SubmitTaskCommand{
		SessionKey: dev.SessionKey, RequesterUserID: dev.UserID,
		Source: "web", SourceInstanceID: "serial", MessageID: "serial-1",
		Prompt: "one", PersonaSnapshot: []string{}, ToolPolicyVersion: "foundation.no-host-tools.v1",
	})
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.SubmitTask(ctx, domain.SubmitTaskCommand{
		SessionKey: dev.SessionKey, RequesterUserID: dev.UserID,
		Source: "web", SourceInstanceID: "serial", MessageID: "serial-2",
		Prompt: "two", PersonaSnapshot: []string{}, ToolPolicyVersion: "foundation.no-host-tools.v1",
	})
	if err != nil {
		t.Fatal(err)
	}

	// Claim first; second must not be claimable (serial gate).
	claimed, ok, err := store.ClaimNextTask(ctx, dev.SessionKey, "serial-owner", time.Minute)
	if err != nil || !ok {
		t.Fatalf("claim first: ok=%v err=%v", ok, err)
	}
	if claimed.ID != first.ID {
		t.Fatalf("claimed %s, want %s", claimed.ID, first.ID)
	}
	keys, err := store.ListClaimableSessionKeys(ctx, 16, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range keys {
		if k == dev.SessionKey {
			t.Fatalf("session %s still claimable while first task starting", dev.SessionKey)
		}
	}
	secondAfter, err := store.GetTask(ctx, second.ID)
	if err != nil {
		t.Fatal(err)
	}
	if secondAfter.Status != domain.TaskQueued {
		t.Fatalf("second status = %s, want queued", secondAfter.Status)
	}

	// Terminal first; second becomes claimable.
	if _, _, err := store.CancelTask(ctx, first.ID, dev.UserID); err != nil {
		t.Fatal(err)
	}
	next, ok, err := store.ClaimNextTask(ctx, dev.SessionKey, "serial-owner", time.Minute)
	if err != nil || !ok {
		t.Fatalf("claim second: ok=%v err=%v", ok, err)
	}
	if next.ID != second.ID {
		t.Fatalf("claimed %s, want %s", next.ID, second.ID)
	}
}
