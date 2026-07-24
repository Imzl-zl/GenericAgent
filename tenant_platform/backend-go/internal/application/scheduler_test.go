package application

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/checkpoint"
	workerv1 "github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/gen/worker/v1"
	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/domain"
	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/workerclient"
	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/postgres"
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

type controlledWorker struct {
	events         chan workerclient.WorkerEvent
	errs           chan error
	executeStarted chan struct{}
	streamDone     chan struct{}
	cancelObserved chan bool
	cancelCalls    atomic.Int32
	closeOnce      sync.Once
	checkpointReady *workerv1.CheckpointReady
	startSessionEntered chan struct{}
	releaseStartSession chan struct{}
	beginCheckpointEntered chan struct{}
	releaseBeginCheckpoint chan struct{}
	beginCheckpointErr     error
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

func (w *controlledWorker) StartSession(ctx context.Context, _ *workerv1.StartSessionRequest) (*workerv1.StartSessionResponse, error) {
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

func (w *controlledWorker) CancelTask(context.Context, string) error {
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

func (w *controlledWorker) Shutdown(context.Context, string) error { return nil }

func (w *controlledWorker) succeed() {
	w.events <- workerclient.WorkerEvent{
		Kind: workerclient.KindTerminal,
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
		Kind: workerclient.KindTerminal,
		Terminal: &workerv1.Terminal{Status: workerv1.TerminalStatus_TASK_INTERRUPTED},
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
	store *postgres.Store
	owner string
}

func (c *successfulCoordinator) Prepare(ctx context.Context, request checkpoint.CheckpointPrepareRequest) (checkpoint.CheckpointLease, error) {
	snapshotID, token, _, err := c.store.PrepareCheckpoint(ctx, request.TaskID, c.owner, "staging-success", request.MaxBundleBytes)
	return checkpoint.CheckpointLease{
		SnapshotID: snapshotID, Token: token, StagingRef: "staging-success", MaxBundleBytes: request.MaxBundleBytes,
	}, err
}

func (c *successfulCoordinator) Commit(_ context.Context, ready checkpoint.ReadyCheckpoint) (checkpoint.CommittedCheckpoint, error) {
	return checkpoint.CommittedCheckpoint{
		SnapshotID: ready.SnapshotID, FileRef: "snapshot:success", Checksum: "sha256:bundle",
		ResultRef: "result:success", ResultDigest: "sha256:result",
	}, nil
}

func (c *successfulCoordinator) ReadResult(context.Context, string, string) (domain.ResultPayload, error) {
	return domain.ResultPayload{Ref: "result:success", Digest: "sha256:result", Body: []byte("result")}, nil
}


type readFailCoordinator struct {
	store *postgres.Store
	owner string
}

func (c *readFailCoordinator) Prepare(ctx context.Context, request checkpoint.CheckpointPrepareRequest) (checkpoint.CheckpointLease, error) {
	snapshotID, token, _, err := c.store.PrepareCheckpoint(ctx, request.TaskID, c.owner, "staging-read-fail", request.MaxBundleBytes)
	return checkpoint.CheckpointLease{
		SnapshotID: snapshotID, Token: token, StagingRef: "staging-read-fail", MaxBundleBytes: request.MaxBundleBytes,
	}, err
}

func (c *readFailCoordinator) Commit(context.Context, checkpoint.ReadyCheckpoint) (checkpoint.CommittedCheckpoint, error) {
	return checkpoint.CommittedCheckpoint{
		FileRef: "snapshot:read-fail", Checksum: "sha256:bundle",
		ResultRef: "result:read-fail", ResultDigest: "sha256:result",
	}, nil
}

func (c *readFailCoordinator) ReadResult(context.Context, string, string) (domain.ResultPayload, error) {
	return domain.ResultPayload{}, errors.New("digest-checked result read failed")
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
		DialWorker: func(context.Context) (workerclient.WorkerClient, func(), error) {
			return worker, func() {}, nil
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
		DialWorker: func(context.Context) (workerclient.WorkerClient, func(), error) {
			return worker, func() {}, nil
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
		DialWorker: func(context.Context) (workerclient.WorkerClient, func(), error) {
			return worker, func() {}, nil
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
		DialWorker: func(context.Context) (workerclient.WorkerClient, func(), error) {
			return worker, func() {}, nil
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
		DialWorker: func(context.Context) (workerclient.WorkerClient, func(), error) {
			return worker, func() {}, nil
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
		DialWorker: func(context.Context) (workerclient.WorkerClient, func(), error) { return worker, func() {}, nil },
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
		DialWorker: func(context.Context) (workerclient.WorkerClient, func(), error) { return worker, func() {}, nil },
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
		DialWorker: func(context.Context) (workerclient.WorkerClient, func(), error) { return worker, func() {}, nil },
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
		DialWorker: func(context.Context) (workerclient.WorkerClient, func(), error) { return worker, func() {}, nil },
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
		DialWorker: func(context.Context) (workerclient.WorkerClient, func(), error) { return worker, func() {}, nil },
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
