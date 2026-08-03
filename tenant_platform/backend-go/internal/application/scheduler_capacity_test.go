package application

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/domain"
	workerv1 "github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/gen/worker/v1"
	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/infrastructure/checkpoint"
	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/infrastructure/llmproxy"
	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/infrastructure/policy"
	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/infrastructure/postgres"
	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/infrastructure/worker"
	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/infrastructure/workerclient"
)

// capacityTaskStore 是最小 TaskStore fake, 只实现 dispatch→ensureWorker 失败
// 路径实际用到的方法; 其余方法 panic 以暴露意外调用。
type capacityTaskStore struct {
	mu             sync.Mutex
	requeued       []string
	claimableKeys  []string
	claimable      map[string]domain.Task // sessionKey -> task
	claimed        map[string]domain.Task
	running        int
	finalized      []domain.Task
	getTask        map[string]domain.Task
	dispatchMarked bool
	// heartbeatLeaseLostAfterRequeue 模拟审查 R5-I1 竞态: RequeueTask 提交
	// queued(claim 已清空)后, dispatch heartbeat ticker 再次 HeartbeatClaim
	// 得到 0 rows → ErrLeaseExpired。fake 对已 requeue 的任务返回该错误。
	heartbeatLeaseLostAfterRequeue bool
}

func newCapacityTaskStore() *capacityTaskStore {
	return &capacityTaskStore{
		claimable: map[string]domain.Task{},
		claimed:   map[string]domain.Task{},
		getTask:   map[string]domain.Task{},
	}
}

func (f *capacityTaskStore) GetTask(ctx context.Context, taskID string) (domain.Task, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if t, ok := f.getTask[taskID]; ok {
		return t, nil
	}
	return domain.Task{}, errors.New("task not found")
}

func (f *capacityTaskStore) HeartbeatClaim(ctx context.Context, taskID, platformInstanceID string, claimLease time.Duration) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.heartbeatLeaseLostAfterRequeue {
		for _, id := range f.requeued {
			if id == taskID {
				return domain.ErrLeaseExpired
			}
		}
	}
	if t, ok := f.getTask[taskID]; ok && !t.Status.IsTerminal() {
		return nil
	}
	return domain.ErrLeaseExpired
}

func (f *capacityTaskStore) SetTaskCapabilityJTIs(_ context.Context, _ string, _ string, _ []string) error {
	return nil
}

func (f *capacityTaskStore) RequeueTask(ctx context.Context, taskID, platformInstanceID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requeued = append(f.requeued, taskID)
	return nil
}

func (f *capacityTaskStore) CompleteFailedTerminal(ctx context.Context, taskID, owner string, status domain.TaskStatus, deliveryType domain.DeliveryType, code, message, traceID string) (domain.Task, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	t := f.getTask[taskID]
	t.Status = status
	f.finalized = append(f.finalized, t)
	f.getTask[taskID] = t
	return t, nil
}

func (f *capacityTaskStore) ListClaimableSessionKeys(ctx context.Context, limit, perUserRunningLimit int) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.claimableKeys...), nil
}

func (f *capacityTaskStore) ClaimNextTask(ctx context.Context, sessionKey, platformInstanceID string, claimLease time.Duration) (domain.Task, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	t, ok := f.claimable[sessionKey]
	if !ok {
		return domain.Task{}, false, nil
	}
	delete(f.claimable, sessionKey)
	t.Status = domain.TaskStarting
	f.claimed[sessionKey] = t
	f.getTask[t.ID] = t
	return t, true, nil
}

func (f *capacityTaskStore) CountRunningTasks(ctx context.Context) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.running, nil
}

func (f *capacityTaskStore) ListOwnedActiveTasks(ctx context.Context, platformInstanceID string) ([]domain.Task, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]domain.Task, 0, len(f.claimed))
	for _, t := range f.claimed {
		out = append(out, t)
	}
	return out, nil
}

func (f *capacityTaskStore) RecoverAfterRestart(ctx context.Context, platformInstanceID string) (int, error) {
	return 0, nil
}

func (f *capacityTaskStore) MarkDispatchStarted(ctx context.Context, taskID, platformInstanceID, workerInstanceID string, freshSession bool) (domain.Task, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.dispatchMarked = true
	return f.getTask[taskID], nil
}

func (f *capacityTaskStore) MarkRunning(ctx context.Context, taskID, platformInstanceID string) (domain.Task, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.getTask[taskID], nil
}

func (f *capacityTaskStore) SubmitTask(ctx context.Context, cmd domain.SubmitTaskCommand) (domain.Task, error) {
	panic("unexpected SubmitTask")
}
func (f *capacityTaskStore) CancelTask(ctx context.Context, taskID string, requesterUserID int64) (domain.Task, bool, error) {
	panic("unexpected CancelTask")
}
func (f *capacityTaskStore) CompleteSucceeded(ctx context.Context, taskID, platformInstanceID, snapshotID, fileRef, checksum, resultRef, resultDigest string, resultBytes int, deliveryFiles []domain.DeliveryFile) (domain.Task, error) {
	panic("unexpected CompleteSucceeded")
}
func (f *capacityTaskStore) RecordChunkEvent(ctx context.Context, taskID string, byteCount int, digest string) error {
	return nil
}
func (f *capacityTaskStore) RecordHeartbeat(ctx context.Context, taskID string) error {
	return nil
}
func (f *capacityTaskStore) CountQueuedTasksByRequester(ctx context.Context, requesterUserID int64) (int, error) {
	panic("unexpected CountQueuedTasksByRequester")
}
func (f *capacityTaskStore) ResetWorkspaceForNewSession(ctx context.Context, sessionKey string) (int, error) {
	panic("unexpected ResetWorkspaceForNewSession")
}
func (f *capacityTaskStore) WorkspaceIsFresh(ctx context.Context, sessionKey string) (bool, error) {
	return false, nil
}

// capacityRuntime 是 fake WorkerRuntime: 按配置返回容量错误或成功。
type capacityRuntime struct {
	err error
}

func (r *capacityRuntime) Start(ctx context.Context, req worker.StartRequest) (*worker.Instance, error) {
	if r.err != nil {
		return nil, r.err
	}
	return nil, errors.New("unexpected Start success in capacity test")
}

func (r *capacityRuntime) ResolveGeneration(ctx context.Context, sessionKey string) (uint64, error) {
	if r.err != nil {
		return 0, r.err
	}
	return 1, nil
}

func (r *capacityRuntime) ReleaseRunnerLease(context.Context, string, uint64) error {
	return nil
}

// TestDispatchRunnerCapacityErrorRequeuesNotFinalizes 验证 dispatch 在 Runner
// 容量满时把任务退回 queued(RequeueTask), 绝不终态化为 failed(审查 C3)。
func TestDispatchRunnerCapacityErrorRequeuesNotFinalizes(t *testing.T) {
	store := newCapacityTaskStore()
	task := domain.Task{
		ID: "cap-task-1", SessionKey: "personal:1",
		Status: domain.TaskStarting, ClaimOwner: "p1",
		ToolPolicyVersion: "foundation.no-host-tools.v1",
	}
	store.getTask[task.ID] = task

	sched := &scheduler{
		cfg: SchedulerConfig{
			PlatformInstanceID: "p1",
			ClaimLease:         time.Minute,
			Store:              store,
			Registry:           mustLoadFoundationPolicy(t),
			Runtime:            &capacityRuntime{err: postgres.ErrRunnerLeaseCapacity},
			Coordinator:        &checkpoint.LocalCoordinator{}, // 未到达
		},
		workers: map[string]*workerEntry{},
		mu:      sync.Mutex{},
	}
	_ = sched.dispatch(context.Background(), task)

	store.mu.Lock()
	defer store.mu.Unlock()
	if len(store.requeued) != 1 || store.requeued[0] != task.ID {
		t.Fatalf("RequeueTask calls = %v, want [%s]", store.requeued, task.ID)
	}
	if len(store.finalized) != 0 {
		t.Fatalf("task must not be finalized on capacity error, got %+v", store.finalized)
	}
}

// TestDispatchRunnerOwnedErrorRequeuesNotFinalizes 验证 foreign-owner lease
// 同样退回 queued 而非失败。
func TestDispatchRunnerOwnedErrorRequeuesNotFinalizes(t *testing.T) {
	store := newCapacityTaskStore()
	task := domain.Task{
		ID: "owned-task-1", SessionKey: "personal:2",
		Status: domain.TaskStarting, ClaimOwner: "p1",
		ToolPolicyVersion: "foundation.no-host-tools.v1",
	}
	store.getTask[task.ID] = task

	sched := &scheduler{
		cfg: SchedulerConfig{
			PlatformInstanceID: "p1",
			ClaimLease:         time.Minute,
			Store:              store,
			Registry:           mustLoadFoundationPolicy(t),
			Runtime:            &capacityRuntime{err: postgres.ErrRunnerLeaseOwned},
		},
		workers: map[string]*workerEntry{},
		mu:      sync.Mutex{},
	}
	_ = sched.dispatch(context.Background(), task)

	store.mu.Lock()
	defer store.mu.Unlock()
	if len(store.requeued) != 1 || store.requeued[0] != task.ID {
		t.Fatalf("RequeueTask calls = %v, want [%s]", store.requeued, task.ID)
	}
	if len(store.finalized) != 0 {
		t.Fatalf("task must not be finalized on owned error, got %+v", store.finalized)
	}
}

// TestDispatchHardErrorStillFinalizes 验证非瞬时错误(如凭证准备失败)仍终态化。
func TestDispatchHardErrorStillFinalizes(t *testing.T) {
	store := newCapacityTaskStore()
	task := domain.Task{
		ID: "hard-task-1", SessionKey: "personal:3",
		Status: domain.TaskStarting, ClaimOwner: "p1",
		ToolPolicyVersion: "foundation.no-host-tools.v1",
	}
	store.getTask[task.ID] = task

	sched := &scheduler{
		cfg: SchedulerConfig{
			PlatformInstanceID: "p1",
			ClaimLease:         time.Minute,
			Store:              store,
			Registry:           mustLoadFoundationPolicy(t),
			Runtime:            &capacityRuntime{err: errors.New("boom")},
		},
		workers: map[string]*workerEntry{},
		mu:      sync.Mutex{},
	}
	_ = sched.dispatch(context.Background(), task)

	store.mu.Lock()
	defer store.mu.Unlock()
	if len(store.requeued) != 0 {
		t.Fatalf("hard error must not requeue, got %v", store.requeued)
	}
	if len(store.finalized) != 1 || store.finalized[0].Status != domain.TaskFailed {
		t.Fatalf("hard error must finalize as failed, got %+v", store.finalized)
	}
}

func mustLoadFoundationPolicy(t *testing.T) policy.Registry {
	t.Helper()
	reg, err := policy.LoadRegistry(foundationPolicy(t))
	if err != nil {
		t.Fatal(err)
	}
	return reg
}

// TestRevokeSessionCredentialsIfTerminalOnlyWhenTerminal 验证 credential 撤销
// 只在任务终态后发生; requeue(仍 queued)时不动(审查 I9 + C3 组合)。
func TestRevokeSessionCredentialsIfTerminalOnlyWhenTerminal(t *testing.T) {
	store := newCapacityTaskStore()
	capabilities := &routingCapabilityStore{}
	sched := &scheduler{
		cfg: SchedulerConfig{
			Store:           store,
			CapabilityStore: capabilities,
		},
		workers: map[string]*workerEntry{
			"personal:6": {
				sessionKey: "personal:6",
				credentials: workerCredentialSet{
					Generation: 1, Checksum: "c", ExpiresAt: time.Now().UTC().Add(time.Hour),
					JTIs: []string{"jti-queued"},
				},
			},
		},
		mu: sync.Mutex{},
	}

	// 任务仍 queued: 不撤销。
	queued := domain.Task{ID: "q1", SessionKey: "personal:6", Status: domain.TaskQueued}
	store.getTask[queued.ID] = queued
	sched.revokeSessionCredentialsIfTerminal(context.Background(), queued.ID, queued.SessionKey, workerCredentialSet{
		Generation: 1, Checksum: "c", ExpiresAt: time.Now().UTC().Add(time.Hour),
		JTIs: []string{"jti-queued"},
	})
	if got := len(capabilities.revoked); got != 0 {
		t.Fatalf("queued task must not revoke, got %d revocations", got)
	}

	// 任务终态: 撤销任务实际使用的集合(不再依赖 entry 当前集合)。
	done := domain.Task{ID: "d1", SessionKey: "personal:6", Status: domain.TaskFailed}
	store.getTask[done.ID] = done
	sched.revokeSessionCredentialsIfTerminal(context.Background(), done.ID, done.SessionKey, workerCredentialSet{
		Generation: 1, Checksum: "c", ExpiresAt: time.Now().UTC().Add(time.Hour),
		JTIs: []string{"jti-done"},
	})
	if got := len(capabilities.revoked); got != 1 || capabilities.revoked[0].jti != "jti-done" {
		t.Fatalf("terminal task must revoke the task's credential set, got %+v", capabilities.revoked)
	}
}

// fakeRevokeWorker 是 dispatch 成功路径的 fake Worker: StartSession 幂等成功,
// ExecuteTask 立即返回 succeeded terminal(completeSuccess 因 Coordinator nil
// 走 NO_COORDINATOR 终态化, defer 撤销仍必须触发)。
type fakeRevokeWorker struct{}

func (w *fakeRevokeWorker) StartSession(context.Context, *workerv1.StartSessionRequest) (*workerv1.StartSessionResponse, error) {
	return &workerv1.StartSessionResponse{SessionKey: "personal:1", WorkerInstanceId: "revoke-worker"}, nil
}
func (w *fakeRevokeWorker) ReloadCredentials(context.Context, *workerv1.ReloadCredentialsRequest) (*workerv1.ReloadCredentialsResponse, error) {
	return &workerv1.ReloadCredentialsResponse{}, nil
}
func (w *fakeRevokeWorker) ExecuteTask(_ context.Context, req *workerv1.ExecuteTaskRequest) (<-chan workerclient.WorkerEvent, <-chan error) {
	events := make(chan workerclient.WorkerEvent, 1)
	errs := make(chan error, 1)
	events <- workerclient.WorkerEvent{
		Kind: workerclient.KindTerminal,
		Terminal: &workerv1.Terminal{
			TaskId:       req.GetTask().GetTaskId(),
			Status:       workerv1.TerminalStatus_TASK_SUCCEEDED,
			ResultDigest: "sha256:" + strings.Repeat("0", 64),
		},
	}
	close(events)
	close(errs)
	return events, errs
}
func (w *fakeRevokeWorker) BeginCheckpoint(context.Context, *workerv1.BeginCheckpointRequest) (*workerv1.CheckpointReady, error) {
	return &workerv1.CheckpointReady{}, nil
}
func (w *fakeRevokeWorker) CancelTask(context.Context, string, string, uint64, string) error { return nil }
func (w *fakeRevokeWorker) Health(context.Context) (*workerv1.HealthResponse, error) {
	return &workerv1.HealthResponse{Ready: true}, nil
}
func (w *fakeRevokeWorker) Shutdown(context.Context, string, string, uint64, string) error { return nil }

// revokeDispatchRuntime 是 dispatch 成功路径的 fake Runtime。
type revokeDispatchRuntime struct{}

func (r *revokeDispatchRuntime) Start(context.Context, worker.StartRequest) (*worker.Instance, error) {
	return &worker.Instance{
		Client: &fakeRevokeWorker{}, InstID: "revoke-worker", Cleanup: func() {}, RunnerGeneration: 1,
	}, nil
}
func (r *revokeDispatchRuntime) ResolveGeneration(context.Context, string) (uint64, error) {
	return 1, nil
}
func (r *revokeDispatchRuntime) ReleaseRunnerLease(context.Context, string, uint64) error { return nil }

// TestDispatchRevokesCredentialsAfterTerminal 回归测试(第三轮 review C1):
// dispatch 的撤销 defer 必须通过闭包捕获 taskCredentialSet——Go defer 注册时
// 立即求值实参, 直接传变量会捕获零值空集, 导致任何终态路径都不撤销。
func TestDispatchRevokesCredentialsAfterTerminal(t *testing.T) {
	store := newCapacityTaskStore()
	task := domain.Task{
		ID: "rev-task-1", SessionKey: "personal:1",
		Status: domain.TaskStarting, ClaimOwner: "p1",
		ToolPolicyVersion: "foundation.no-host-tools.v1",
	}
	store.getTask[task.ID] = task

	issuer, err := llmproxy.NewIssuer([]byte("test-signing-key-at-least-32-bytes"), 2*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	capabilities := &routingCapabilityStore{}
	configRoot := t.TempDir()
	sched := &scheduler{
		cfg: SchedulerConfig{
			PlatformInstanceID: "p1",
			ClaimLease:         time.Minute,
			Store:              store,
			Registry:           mustLoadFoundationPolicy(t),
			Runtime:            &revokeDispatchRuntime{},
			Coordinator:        nil, // completeSuccess 走 NO_COORDINATOR 终态化
			TokenIssuer:        issuer,
			CapabilityStore:    capabilities,
			LLMProvider:        &fakeLLMProviderSource{providers: []domain.LLMProvider{testProvider(1, 1, domain.ProviderNativeOAI, true)}},
			LLMProxyAddr:       "http://127.0.0.1:9999",
			ModelPolicyVersion: "test.v1",
			MaxTaskWallClock:   time.Hour,
			TokenRefreshSkew:   5 * time.Minute,
			ConfigRoot:         configRoot,
		},
		workers: map[string]*workerEntry{},
		mu:      sync.Mutex{},
	}
	_ = sched.dispatch(context.Background(), task)

	store.mu.Lock()
	store.mu.Unlock()
	t.Logf("revoked=%d %+v", len(capabilities.revoked), capabilities.revoked)
	if len(capabilities.revoked) == 0 {
		t.Fatal("dispatch terminal path must revoke the task credential set (defer must capture by closure)")
	}
	if len(capabilities.revoked[0].jti) == 0 {
		t.Fatalf("revoked jti is empty: %+v", capabilities.revoked[0])
	}
}

// TestDispatchHeartbeatRequeueSuppressesLeaseLoss 验证审查 R5-I1 竞态修复:
// requeue 标记(容量满退回 queued)后, dispatch heartbeat 的 ticker 若在
// deferred Stop 之前触发 HeartbeatClaim(requeue 已清空 claim → 0 rows →
// ErrLeaseExpired), 必须静默退出——不得把已 requeue 的任务终态化。
// 单元级确定性测试: 直接控制 markRequeued 与 requeue 状态。
func TestDispatchHeartbeatRequeueSuppressesLeaseLoss(t *testing.T) {
	store := newCapacityTaskStore()
	store.heartbeatLeaseLostAfterRequeue = true
	task := domain.Task{
		ID: "race-unit-1", SessionKey: "personal:9",
		Status: domain.TaskStarting, ClaimOwner: "p1",
	}
	store.getTask[task.ID] = task

	sched := &scheduler{
		cfg: SchedulerConfig{
			PlatformInstanceID: "p1",
			ClaimLease:         60 * time.Millisecond, // ticker 间隔 20ms
			Store:              store,
		},
		workers: map[string]*workerEntry{},
		mu:      sync.Mutex{},
	}
	heartbeat, err := sched.startDispatchHeartbeat(context.Background(), task)
	if err != nil {
		t.Fatalf("startDispatchHeartbeat: %v", err)
	}
	// 容量错误路径: 先标记 requeue, 再提交 requeue(fake 记录 requeued)。
	heartbeat.markRequeued()
	if err := store.RequeueTask(context.Background(), task.ID, "p1"); err != nil {
		t.Fatalf("RequeueTask: %v", err)
	}
	// 让 ticker 在 requeue 后至少触发一次(interval 20ms)。
	time.Sleep(100 * time.Millisecond)
	if err := heartbeat.Stop(); err != nil {
		t.Fatalf("Stop must not report lease loss after requeue, got: %v", err)
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if len(store.finalized) != 0 {
		t.Fatalf("requeued task must not be finalized by heartbeat race, got %+v", store.finalized)
	}
}

// TestDispatchRequeueHeartbeatRaceDoesNotFinalize 集成级验证同一竞态: dispatch
// 全流程中容量错误 requeue 后, 若 heartbeat 触发 lease 丢失也不得终态化。
func TestDispatchRequeueHeartbeatRaceDoesNotFinalize(t *testing.T) {
	store := newCapacityTaskStore()
	store.heartbeatLeaseLostAfterRequeue = true
	task := domain.Task{
		ID: "race-int-1", SessionKey: "personal:10",
		Status: domain.TaskStarting, ClaimOwner: "p1",
		ToolPolicyVersion: "foundation.no-host-tools.v1",
	}
	store.getTask[task.ID] = task

	sched := &scheduler{
		cfg: SchedulerConfig{
			PlatformInstanceID: "p1",
			ClaimLease:         60 * time.Millisecond,
			Store:              store,
			Registry:           mustLoadFoundationPolicy(t),
			Runtime:            &capacityRuntime{err: postgres.ErrRunnerLeaseCapacity},
		},
		workers: map[string]*workerEntry{},
		mu:      sync.Mutex{},
	}
	_ = sched.dispatch(context.Background(), task)

	store.mu.Lock()
	defer store.mu.Unlock()
	if len(store.requeued) != 1 || store.requeued[0] != task.ID {
		t.Fatalf("RequeueTask calls = %v, want [%s]", store.requeued, task.ID)
	}
	if len(store.finalized) != 0 {
		t.Fatalf("requeued task must not be finalized by heartbeat race, got %+v", store.finalized)
	}
}
