package application

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/domain"
	workerv1 "github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/gen/worker/v1"
	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/infrastructure/checkpoint"
	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/infrastructure/postgres"
	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/infrastructure/workerclient"
)

// ---------------------------------------------------------------------------
// round11 审查 C2: completeSuccess 在 CompleteSucceeded 提交结果不确定
// (ErrCommitOutcomeUnknown)时不得删除已物化的 committed/result 文件——
// 文件可能已被 DB 恢复指针引用, 删除会破坏恢复点。只有确定回滚的错误才
// 允许清理。GetTask 重读失败也视为不确定, 保留文件交对账回收。
// ---------------------------------------------------------------------------

type commitFailureStore struct {
	*postgres.Store
	completeErr error
	getTaskErr  error
	getTaskTask domain.Task
	finalErr    error
	mu          sync.Mutex
	complete    int
	final       int
}

func (s *commitFailureStore) CompleteSucceeded(ctx context.Context, taskID, platformInstanceID, snapshotID, fileRef, checksum, resultRef, resultDigest string, resultBytes int, deliveryFiles []domain.DeliveryFile) (domain.Task, error) {
	s.mu.Lock()
	s.complete++
	s.mu.Unlock()
	return domain.Task{}, s.completeErr
}



func (s *commitFailureStore) IsApprovedUser(_ context.Context, _ int64) (bool, error) {
	return true, nil
}
func (f *commitFailureStore) IsApprovedTeamMember(_ context.Context, _ string, _ int64) (bool, error) {
	return true, nil
}

func (s *commitFailureStore) GetTask(ctx context.Context, taskID string) (domain.Task, error) {
	if s.getTaskErr != nil {
		return domain.Task{}, s.getTaskErr
	}
	return s.getTaskTask, nil
}

func (s *commitFailureStore) CompleteFailedTerminal(ctx context.Context, taskID, owner string, status domain.TaskStatus, deliveryType domain.DeliveryType, code, message, traceID string) (domain.Task, error) {
	s.mu.Lock()
	s.final++
	s.mu.Unlock()
	if s.finalErr != nil {
		return domain.Task{}, s.finalErr
	}
	return domain.Task{ID: taskID, Status: status}, nil
}

type cleanupRecordingCoordinator struct {
	mu          sync.Mutex
	cleanupCalls []string
}

func (c *cleanupRecordingCoordinator) Prepare(ctx context.Context, request checkpoint.CheckpointPrepareRequest) (checkpoint.CheckpointLease, error) {
	return checkpoint.CheckpointLease{
		SnapshotID: "11111111-1111-4111-8111-111111111111", Token: "tok",
		StagingRef: "/staging/host.bundle.json", MaxBundleBytes: 1 << 20,
	}, nil
}

func (c *cleanupRecordingCoordinator) Commit(ctx context.Context, ready checkpoint.ReadyCheckpoint) (checkpoint.CommittedCheckpoint, error) {
	return checkpoint.CommittedCheckpoint{
		SnapshotID:   "11111111-1111-4111-8111-111111111111",
		FileRef:      "snapshot:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa:11111111-1111-4111-8111-111111111111",
		Checksum:     "sha256:committed",
		ResultRef:    "result:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa:11111111-1111-4111-8111-111111111111",
		ResultDigest: "sha256:result",
	}, nil
}

func (c *cleanupRecordingCoordinator) CurrentRestorePoint(ctx context.Context, workspaceID string) (checkpoint.RestorePoint, bool, error) {
	return checkpoint.RestorePoint{}, false, nil
}

func (c *cleanupRecordingCoordinator) ReadResult(ctx context.Context, ref, expectedDigest string) (domain.ResultPayload, error) {
	return domain.ResultPayload{Ref: ref, Digest: expectedDigest, Body: []byte("ok")}, nil
}

func (c *cleanupRecordingCoordinator) SweepExpiredCheckpoints(ctx context.Context) (int, error) {
	return 0, nil
}

func (c *cleanupRecordingCoordinator) CleanupCommittedFiles(ctx context.Context, committed checkpoint.CommittedCheckpoint) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cleanupCalls = append(c.cleanupCalls, committed.SnapshotID)
	return nil
}

func (c *cleanupRecordingCoordinator) ReconcileOrphanCommittedFiles(ctx context.Context) (int, error) {
	return 0, nil
}

func (c *cleanupRecordingCoordinator) ReconcileOrphanStagingFiles(context.Context) (int, error) {
	return 0, nil
}

func (c *cleanupRecordingCoordinator) RunnerStagingRef(hostRef string) (string, error) {
	return hostRef, nil
}

func (c *cleanupRecordingCoordinator) HostStagingRef(runnerRef, expectedHostRef string) (string, error) {
	return expectedHostRef, nil
}

type checkpointTestWorker struct{}

func (w *checkpointTestWorker) StartSession(ctx context.Context, req *workerv1.StartSessionRequest) (*workerv1.StartSessionResponse, error) {
	return &workerv1.StartSessionResponse{}, nil
}

func (w *checkpointTestWorker) ExecuteTask(ctx context.Context, req *workerv1.ExecuteTaskRequest) (<-chan workerclient.WorkerEvent, <-chan error) {
	return make(chan workerclient.WorkerEvent), make(chan error)
}

func (w *checkpointTestWorker) BeginCheckpoint(ctx context.Context, req *workerv1.BeginCheckpointRequest) (*workerv1.CheckpointReady, error) {
	return &workerv1.CheckpointReady{
		StagingRef: req.GetStagingRef(), Checksum: req.GetCheckpointToken(),
		ResultDigest: "sha256:result", RunnerGeneration: req.GetRunnerGeneration(),
	}, nil
}

func (w *checkpointTestWorker) CancelTask(ctx context.Context, workspaceKey, taskID string, runnerGeneration uint64, capabilityJTI string) error {
	return nil
}

func (w *checkpointTestWorker) Health(ctx context.Context) (*workerv1.HealthResponse, error) {
	return &workerv1.HealthResponse{}, nil
}

func (w *checkpointTestWorker) Shutdown(ctx context.Context, workspaceKey, reason string, runnerGeneration uint64, capabilityJTI string) error {
	return nil
}

func newCompleteSuccessHarness(store TaskStore, coord checkpoint.Coordinator) *scheduler {
	entry := &workerEntry{
		client: &checkpointTestWorker{}, sessionKey: "personal:1",
		runnerGeneration: 1, credentials: workerCredentialSet{},
	}
	s := &scheduler{
		cfg: SchedulerConfig{
			PlatformInstanceID: "platform-a",
			Store:              store,
			Coordinator:        coord,
		},
		wake:    make(chan struct{}, 1),
		workers: map[string]*workerEntry{"personal:1": entry},
	}
	return s
}

func completeSuccessTask() domain.Task {
	return domain.Task{ID: "task-1", SessionKey: "personal:1", WorkspaceID: "ws-1"}
}

// TestCompleteSuccess_UnknownCommitOutcomeKeepsFiles: CompleteSucceeded 返回
// ErrCommitOutcomeUnknown(网络/超时)且 GetTask 重读也失败时, 不得调用
// CleanupCommittedFiles——文件可能已被 DB 提交引用, 删除会破坏恢复点。
func TestCompleteSuccess_UnknownCommitOutcomeKeepsFiles(t *testing.T) {
	store := &commitFailureStore{
		completeErr: fmt.Errorf("%w: network reset", postgres.ErrCommitOutcomeUnknown),
		getTaskErr:  errors.New("database unavailable"),
	}
	coord := &cleanupRecordingCoordinator{}
	s := newCompleteSuccessHarness(store, coord)

	err := s.completeSuccess(context.Background(), completeSuccessTask(), &workerv1.Terminal{ResultDigest: "sha256:result"})
	if err == nil {
		t.Fatal("expected error for unknown commit outcome")
	}
	if !errors.Is(err, postgres.ErrCommitOutcomeUnknown) {
		t.Fatalf("expected ErrCommitOutcomeUnknown to propagate, got %v", err)
	}
	if got := len(coord.cleanupCalls); got != 0 {
		t.Fatalf("committed files must NOT be deleted on unknown outcome, cleanup calls=%v", coord.cleanupCalls)
	}
	if store.final != 1 {
		t.Fatalf("expected task finalization attempt, got %d", store.final)
	}
}

// TestCompleteSuccess_DeterministicRollbackCleansFiles: 确定回滚(非 unknown)
// 且重读失败时, 文件确实是孤儿, 必须清理(round10 B9a 语义保留)。
func TestCompleteSuccess_DeterministicRollbackCleansFiles(t *testing.T) {
	store := &commitFailureStore{
		completeErr: errors.New("complete: snapshot not writable"),
		getTaskErr:  errors.New("database unavailable"),
	}
	coord := &cleanupRecordingCoordinator{}
	s := newCompleteSuccessHarness(store, coord)

	err := s.completeSuccess(context.Background(), completeSuccessTask(), &workerv1.Terminal{ResultDigest: "sha256:result"})
	if err == nil {
		t.Fatal("expected error")
	}
	if errors.Is(err, postgres.ErrCommitOutcomeUnknown) {
		t.Fatalf("deterministic rollback must not be classified unknown: %v", err)
	}
	if got := len(coord.cleanupCalls); got != 1 {
		t.Fatalf("orphan committed files must be cleaned on deterministic rollback, got %d calls", got)
	}
}

// TestCompleteSuccess_UnknownOutcomeButTerminalReadKeepsFiles: 提交结果不确定,
// 但重读发现任务已终态——事务实际已提交, 文件是恢复点, 不得删除且不得
// 把成功覆盖为失败(返回 nil)。
func TestCompleteSuccess_UnknownOutcomeButTerminalReadKeepsFiles(t *testing.T) {
	store := &commitFailureStore{
		completeErr: fmt.Errorf("%w: network reset", postgres.ErrCommitOutcomeUnknown),
		getTaskTask: domain.Task{ID: "task-1", Status: domain.TaskSucceeded},
	}
	coord := &cleanupRecordingCoordinator{}
	s := newCompleteSuccessHarness(store, coord)

	err := s.completeSuccess(context.Background(), completeSuccessTask(), &workerv1.Terminal{ResultDigest: "sha256:result"})
	if err != nil {
		t.Fatalf("terminal read-back must be treated as committed success, got %v", err)
	}
	if got := len(coord.cleanupCalls); got != 0 {
		t.Fatalf("no cleanup allowed when task is already terminal, got %v", coord.cleanupCalls)
	}
	if store.final != 0 {
		t.Fatalf("terminal task must not be re-finalized, got %d finalize calls", store.final)
	}
}

// TestCompleteSuccess_DeterministicRollbackWithTerminalReadKeepsFiles: 确定回滚
// 但重读已是终态(其他路径终态化)——文件仍可能是恢复点(事务已提交), 不删。
func TestCompleteSuccess_DeterministicRollbackWithTerminalReadKeepsFiles(t *testing.T) {
	store := &commitFailureStore{
		completeErr: errors.New("complete: snapshot not writable"),
		getTaskTask: domain.Task{ID: "task-1", Status: domain.TaskSucceeded},
	}
	coord := &cleanupRecordingCoordinator{}
	s := newCompleteSuccessHarness(store, coord)

	err := s.completeSuccess(context.Background(), completeSuccessTask(), &workerv1.Terminal{ResultDigest: "sha256:result"})
	if err != nil {
		t.Fatalf("terminal read-back must be treated as committed success, got %v", err)
	}
	if got := len(coord.cleanupCalls); got != 0 {
		t.Fatalf("no cleanup allowed when task is already terminal, got %v", coord.cleanupCalls)
	}
}

// TestClassifyCommitErrorSentinelWireUp verifies the sentinel is exported and
// strings correctly (cross-package sanity for scheduler-side errors.Is).
func TestClassifyCommitErrorSentinelWireUp(t *testing.T) {
	err := fmt.Errorf("%w: boom", postgres.ErrCommitOutcomeUnknown)
	if !strings.Contains(err.Error(), "transaction commit outcome unknown") {
		t.Fatalf("unexpected sentinel message: %v", err)
	}
	_ = time.Second
}
