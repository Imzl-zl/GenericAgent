package application

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/domain"
	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/infrastructure/postgres"
	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/infrastructure/workerclient"
)

// Round12 审查 I1: dispatch 是任务 Worker 的唯一 owner——任何退出路径(含
// panic/并发终态/策略失败/无 coordinator)都必须销毁 Worker, 否则容器与
// lease 续租泄漏, 同 generation 下一条任务会命中旧容器。
//
// 每个测试: 触发泄漏路径后断言 cleanup 被调用(workers map 清空 + cleanup
// flag 置位)且任务已终态化。

// cleanWorkerCleanup 返回带 cleanup 标志的 DialWorker(每会话一个 Worker)。
type cleanWorkerCleanup struct {
	worker  *controlledWorker
	cleaned atomic.Bool
}

func (c *cleanWorkerCleanup) dial(context.Context, string) (workerclient.WorkerClient, func(string), error) {
	return c.worker, func(_ string) { c.cleaned.Store(true) }, nil
}

// claimedTaskFor 提交并 claim 一个任务, 返回任务行。
func claimedTaskFor(t *testing.T, store *postgres.Store, sessionKey string, policyVersion string, owner string) domain.Task {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	task, err := store.SubmitTask(ctx, domain.SubmitTaskCommand{
		SessionKey: sessionKey, RequesterUserID: 1,
		Source: "web", SourceInstanceID: owner, MessageID: owner + "-m",
		Prompt: "run", PersonaSnapshot: []string{}, ToolPolicyVersion: policyVersion,
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = task
	claimed, ok, err := store.ClaimNextTask(ctx, sessionKey, owner, time.Second)
	if err != nil || !ok {
		t.Fatalf("claim: ok=%v err=%v", ok, err)
	}
	return claimed
}

func newLeakTestScheduler(t *testing.T, store TaskStore, cleanup *cleanWorkerCleanup, owner string, settings AgentRuntimeSettings) *scheduler {
	t.Helper()
	schedulerAPI, err := NewScheduler(SchedulerConfig{
		PlatformInstanceID: owner, ClaimLease: time.Second,
		Store: store, Registry: testPolicyRegistry(t),
		RuntimeSettings: settings,
		DialWorker:      cleanup.dial,
	})
	if err != nil {
		t.Fatal(err)
	}
	return schedulerAPI.(*scheduler)
}

func TestDispatchPolicyResolveFailureDestroysWorker(t *testing.T) {
	_, store, _, dev := serviceFixture(t)
	cleanup := &cleanWorkerCleanup{worker: newControlledWorker()}
	sched := newLeakTestScheduler(t, store, cleanup, "leak-policy", nil)
	claimed := claimedTaskFor(t, store, dev.SessionKey, "no-such-policy.v1", "leak-policy")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := sched.dispatch(ctx, claimed); err == nil {
		t.Fatal("expected policy resolve error")
	}
	if !cleanup.cleaned.Load() {
		t.Fatal("worker cleanup was not called after POLICY_RESOLVE_FAILED")
	}
	if _, present := sched.workers[dev.SessionKey]; present {
		t.Fatal("worker entry leaked after POLICY_RESOLVE_FAILED")
	}
	final, err := store.GetTask(ctx, claimed.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !final.Status.IsTerminal() || final.TerminalErrorCode != "POLICY_RESOLVE_FAILED" {
		t.Fatalf("task status=%s code=%s want terminal POLICY_RESOLVE_FAILED", final.Status, final.TerminalErrorCode)
	}
	assertNoWorkerLeaks(t, sched)
}

func TestDispatchAgentMaxTurnsErrorDestroysWorker(t *testing.T) {
	_, store, _, dev := serviceFixture(t)
	cleanup := &cleanWorkerCleanup{worker: newControlledWorker()}
	sched := newLeakTestScheduler(t, store, cleanup, "leak-turns",
		&errRuntimeSettings{})
	claimed := claimedTaskFor(t, store, dev.SessionKey, "foundation.no-host-tools.v1", "leak-turns")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := sched.dispatch(ctx, claimed); err == nil {
		t.Fatal("expected agent max turns error")
	}
	if !cleanup.cleaned.Load() {
		t.Fatal("worker cleanup was not called after startSession agentMaxTurns error")
	}
	if _, present := sched.workers[dev.SessionKey]; present {
		t.Fatal("worker entry leaked after startSession agentMaxTurns error")
	}
	final, err := store.GetTask(ctx, claimed.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !final.Status.IsTerminal() || final.TerminalErrorCode != "WORKER_START_FAILED" {
		t.Fatalf("task status=%s code=%s want terminal WORKER_START_FAILED", final.Status, final.TerminalErrorCode)
	}
	assertNoWorkerLeaks(t, sched)
}

// errRuntimeSettings 让 agentMaxTurns 解析失败。
type errRuntimeSettings struct{}

func (e *errRuntimeSettings) GetAgentMaxTurns(context.Context) (int, error) {
	return 0, errors.New("runtime settings unavailable")
}

func (e *errRuntimeSettings) GetIMStreamingMode(context.Context) (domain.IMStreamingMode, error) {
	return "", errors.New("runtime settings unavailable")
}

// terminalizeAfterMarkRunningStore 在 MarkRunning 成功后立即终态化任务,
// 模拟"另一路径在 MarkRunning 与 GetTask 之间终态化任务"的并发场景。
type terminalizeAfterMarkRunningStore struct {
	*postgres.Store
	instance string
	once     sync.Once
}

func (s *terminalizeAfterMarkRunningStore) MarkRunning(ctx context.Context, taskID, platformInstanceID string) (domain.Task, error) {
	t, err := s.Store.MarkRunning(ctx, taskID, platformInstanceID)
	if err != nil {
		return t, err
	}
	s.once.Do(func() {
		_, _ = s.Store.CompleteFailedTerminal(context.Background(), taskID, platformInstanceID,
			domain.TaskFailed, domain.DeliveryTaskFailed, "CONCURRENT_TERMINAL", "terminalized concurrently", "")
	})
	return t, nil
}

func TestDispatchConcurrentTerminalAfterMarkRunningDestroysWorker(t *testing.T) {
	_, store, _, dev := serviceFixture(t)
	cleanup := &cleanWorkerCleanup{worker: newControlledWorker()}
	wrapped := &terminalizeAfterMarkRunningStore{Store: store, instance: "leak-concurrent"}
	sched := newLeakTestScheduler(t, wrapped, cleanup, "leak-concurrent", nil)
	claimed := claimedTaskFor(t, store, dev.SessionKey, "foundation.no-host-tools.v1", "leak-concurrent")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := sched.dispatch(ctx, claimed); err != nil {
		t.Fatalf("dispatch error=%v (concurrent terminal is not an error path)", err)
	}
	if !cleanup.cleaned.Load() {
		t.Fatal("worker cleanup was not called after concurrent terminal")
	}
	if _, present := sched.workers[dev.SessionKey]; present {
		t.Fatal("worker entry leaked after concurrent terminal")
	}
	final, err := store.GetTask(ctx, claimed.ID)
	if err != nil {
		t.Fatal(err)
	}
	if final.TerminalErrorCode != "CONCURRENT_TERMINAL" {
		t.Fatalf("task code=%s want CONCURRENT_TERMINAL", final.TerminalErrorCode)
	}
	assertNoWorkerLeaks(t, sched)
}

// panicAfterDispatchStartedStore 在 MarkDispatchStarted 成功后注入 panic,
// 覆盖 dispatch 的 panic recovery 路径(Worker 已创建)。
type panicAfterDispatchStartedStore struct {
	*postgres.Store
	once sync.Once
}

func (s *panicAfterDispatchStartedStore) MarkDispatchStarted(ctx context.Context, taskID, platformInstanceID, workerInstanceID string, freshSession bool) (domain.Task, error) {
	t, err := s.Store.MarkDispatchStarted(ctx, taskID, platformInstanceID, workerInstanceID, freshSession)
	if err != nil {
		return t, err
	}
	s.once.Do(func() { panic("injected dispatch panic") })
	return t, nil
}

func TestDispatchPanicDestroysWorker(t *testing.T) {
	_, store, _, dev := serviceFixture(t)
	cleanup := &cleanWorkerCleanup{worker: newControlledWorker()}
	wrapped := &panicAfterDispatchStartedStore{Store: store}
	sched := newLeakTestScheduler(t, wrapped, cleanup, "leak-panic", nil)
	claimed := claimedTaskFor(t, store, dev.SessionKey, "foundation.no-host-tools.v1", "leak-panic")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := sched.dispatch(ctx, claimed); err == nil {
		t.Fatal("expected dispatch panic error")
	}
	if !cleanup.cleaned.Load() {
		t.Fatal("worker cleanup was not called after dispatch panic")
	}
	if _, present := sched.workers[dev.SessionKey]; present {
		t.Fatal("worker entry leaked after dispatch panic")
	}
	final, err := store.GetTask(ctx, claimed.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !final.Status.IsTerminal() {
		t.Fatalf("task status=%s want terminal after panic", final.Status)
	}
	assertNoWorkerLeaks(t, sched)
}

func TestDispatchSuccessWithoutCoordinatorDestroysWorker(t *testing.T) {
	_, store, _, dev := serviceFixture(t)
	cleanup := &cleanWorkerCleanup{worker: newControlledWorker()}
	sched := newLeakTestScheduler(t, store, cleanup, "leak-nocoord", nil)
	claimed := claimedTaskFor(t, store, dev.SessionKey, "foundation.no-host-tools.v1", "leak-nocoord")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	// Worker 成功完成, 但 checkpoint coordinator 未配置 → NO_COORDINATOR。
	cleanup.worker.succeed()
	if err := sched.dispatch(ctx, claimed); err == nil {
		t.Fatal("expected NO_COORDINATOR error")
	}
	if !cleanup.cleaned.Load() {
		t.Fatal("worker cleanup was not called after NO_COORDINATOR")
	}
	if _, present := sched.workers[dev.SessionKey]; present {
		t.Fatal("worker entry leaked after NO_COORDINATOR")
	}
	final, err := store.GetTask(ctx, claimed.ID)
	if err != nil {
		t.Fatal(err)
	}
	if final.TerminalErrorCode != "NO_COORDINATOR" {
		t.Fatalf("task code=%s want NO_COORDINATOR", final.TerminalErrorCode)
	}
	assertNoWorkerLeaks(t, sched)
}

// TestDestroyTaskWorkerEntryDoesNotTouchReplacedEntry: 身份校验——旧任务收尾
// 不得误毁同 session 新任务的 Worker(0af8228 竞争修复的延续); 同时旧 entry
// 本身必须被清理(round12 审查 I1 补充: 终态任务不得因 map 被替换而泄漏容器/
// 凭据——completeSuccess 终态提交与销毁之间的替换窗口)。
func TestDestroyTaskWorkerEntryDoesNotTouchReplacedEntry(t *testing.T) {
	_, store, _, dev := serviceFixture(t)
	cleanup := &cleanWorkerCleanup{worker: newControlledWorker()}
	sched := newLeakTestScheduler(t, store, cleanup, "leak-identity", nil)

	oldEntry := &workerEntry{sessionKey: dev.SessionKey}
	oldCleaned := atomic.Bool{}
	oldEntry.cleanup = func(_ string) { oldCleaned.Store(true) }
	newEntry := &workerEntry{sessionKey: dev.SessionKey}
	newEntry.cleanup = func(_ string) { cleanup.cleaned.Store(true) }

	sched.mu.Lock()
	sched.workers[dev.SessionKey] = newEntry // 新任务已替换旧 entry
	sched.mu.Unlock()

	sched.destroyTaskWorkerEntry(dev.SessionKey, oldEntry)

	sched.mu.Lock()
	current := sched.workers[dev.SessionKey]
	sched.mu.Unlock()
	if current != newEntry {
		t.Fatal("destroyTaskWorkerEntry removed the replaced (new) entry")
	}
	if cleanup.cleaned.Load() {
		t.Fatal("destroyTaskWorkerEntry cleaned the replaced (new) entry")
	}
	if !oldCleaned.Load() {
		t.Fatal("old (replaced) entry must still be cleaned up; its container would leak otherwise")
	}
}
