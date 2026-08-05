package application

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"path/filepath"
	"sync"
	"sync/atomic"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/domain"
	workerv1 "github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/gen/worker/v1"
	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/infrastructure/worker"
	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/infrastructure/workerclient"
)

// workerEntry holds the Worker process bound to ONE task(决策 D2.1: 任务即
// 进程)。任务终态由 destroyTaskWorker 销毁, 进程内不复用。
type workerEntry struct {
	client      workerclient.WorkerClient
	cleanup     func(capabilityJTI string)
	instID      string
	sessionKey  string
	taskID      string // 本 Worker 绑定的 task(方案 §7 per-task capability)
	credentials workerCredentialSet
	lifecycleMu sync.Mutex
	// startMu 只串行 StartSession 与 CancelWorker(round11 审查 C3): 同一
	// 任务的取消等待 StartSession 完成; 而 checkpoint 收尾(completeSuccess
	// 持 lifecycleMu)不被取消阻塞。
	startMu sync.Mutex
	// executing 标记 ExecuteTask 流进行中(round11 C3): CancelWorker 只在
	// 任务实际执行时向 Worker 发取消 RPC; StartSession 刚完成但尚未执行
	// 时跳过——dispatch 会检测 durable cancel_requested_at 并销毁。
	executing atomic.Bool
	// runnerGeneration 是该 Worker 的 Runner lease generation(方案 §7 fencing)。
	// loopback 路径恒为 1; Sandbox 路径由持久 lease 提供。
	runnerGeneration uint64
}

func (s *scheduler) maybeCancelWorker(ctx context.Context, task domain.Task) {
	_ = s.CancelWorker(ctx, task)
}

func (s *scheduler) CancelWorker(ctx context.Context, task domain.Task) error {
	value, _ := s.cancelOnce.LoadOrStore(task.ID, &cancelCall{})
	call := value.(*cancelCall)
	call.mu.Lock()
	if call.done {
		err := call.err
		call.mu.Unlock()
		return err
	}
	if call.inflight {
		// 已有并发取消在途: 同步合并等待其结果(保持旧 once.Do 语义)。
		notify := call.notify
		if notify == nil {
			notify = make(chan struct{})
			call.notify = notify
		}
		call.mu.Unlock()
		select {
		case <-notify:
		case <-ctx.Done():
			return ctx.Err()
		}
		call.mu.Lock()
		err := call.err
		call.mu.Unlock()
		return err
	}
	call.inflight = true
	call.mu.Unlock()

	err := s.cancelWorkerRPC(ctx, task)

	call.mu.Lock()
	call.inflight = false
	// round11 审查(I2): 失败不再永久缓存——任务仍是 running 且
	// cancel_requested_at 已持久, 下一次 tick 的 maybeCancelWorker 会重试;
	// 只有成功(含"无需 RPC"的幂等成功)才终态缓存。
	if err == nil {
		call.done = true
		call.err = nil
	} else {
		call.done = false
		call.err = err
	}
	notify := call.notify
	call.notify = nil
	call.mu.Unlock()
	if notify != nil {
		close(notify)
	}
	return err
}

// cancelWorkerRPC 执行一次取消 RPC(带 per-entry 锁与超时)。
func (s *scheduler) cancelWorkerRPC(ctx context.Context, task domain.Task) error {
	s.mu.Lock()
	entry := s.workers[task.SessionKey]
	s.mu.Unlock()
	if entry == nil {
		// worker 已不存在 = 任务已终态销毁, 取消已生效,
		// 视为成功而不是向用户报错(否则 CancelTask API 在
		// cancel-during-StartSession 竞态下返回 500, 尽管任务已中断)。
		return nil
	}
	// round11 审查(C3): 取消与同 session 的 StartSession 互斥用 per-entry
	// startMu; 与 checkpoint 收尾(lifecycleMu)不互斥。
	// 任务尚未进入执行阶段时跳过 RPC——dispatch 会检测 durable
	// cancel_requested_at 并销毁 Worker, 无需向 Worker 发取消。
	entry.startMu.Lock()
	defer entry.startMu.Unlock()
	if !entry.executing.Load() {
		return nil
	}
	cancelCtx, cancel := context.WithTimeout(ctx, workerControlTimeout)
	defer cancel()
	return entry.client.CancelTask(cancelCtx, entry.sessionKey, task.ID, entry.runnerGeneration, controlJTIFor(entry.credentials))
}

// createTaskWorker 为任务创建全新 Worker 进程(决策 D2.1: 任务即进程, 不复用)。
// StartSession 不在本函数调用; 由 dispatch 在 MarkDispatchStarted 之后调用
// (cancel-during-StartSession 可见 durable cancel 并记录, 而非立即终态化)。
//
// 每任务签发: 一个 capability token per routed Provider 写入 session 级
// runtime JSON。真实上游密钥永不进入 Worker。凭证集在 JTI 持久撤销前保持
// 追踪。
func (s *scheduler) createTaskWorker(ctx context.Context, task domain.Task) (workerclient.WorkerClient, *workerEntry, error) {
	s.mu.Lock()
	entry := &workerEntry{sessionKey: task.SessionKey, taskID: task.ID}
	s.workers[task.SessionKey] = entry
	s.mu.Unlock()
	entry.lifecycleMu.Lock()
	return s.initializeWorkerEntry(ctx, task, entry)
}

func (s *scheduler) initializeWorkerEntry(
	ctx context.Context, task domain.Task, entry *workerEntry,
) (workerclient.WorkerClient, *workerEntry, error) {
	// generation fencing(方案 §7): 先解析持久 lease generation, 再签发绑定
	// 该 generation 的 per-task capability 与运行时配置(Sandbox 注入容器)。
	generation, err := s.startGeneration(ctx, task.SessionKey)
	if err != nil {
		s.removeWorkerEntry(task.SessionKey, entry)
		entry.lifecycleMu.Unlock()
		return nil, nil, err
	}
	credentials, files, err := s.issueInitialWorkerCredentials(ctx, task, generation)
	if err != nil {
		// 审查: lease 已获取/续租但初始化失败——立即释放归还全局容量,
		// 否则多工作区各失败一次即可占满容量直到 TTL 到期。
		s.releaseRunnerLeaseBestEffort(task.SessionKey, generation)
		s.removeWorkerEntry(task.SessionKey, entry)
		entry.lifecycleMu.Unlock()
		return nil, nil, err
	}
	runtimeConfigFiles := map[string][]byte{}
	if len(files.JSON) > 0 {
		runtimeConfigFiles[runtimeConfigFilename] = files.JSON
		runtimeConfigFiles[myKeyLoaderFilename] = files.Loader
	}
	client, instID, cleanup, gen, err := s.startWorkerProcess(ctx, task.SessionKey, runtimeConfigFiles)
	if err != nil {
		s.removeWorkerEntry(task.SessionKey, entry)
		s.revokeCredentialSetBestEffort(context.Background(), credentials)
		entry.lifecycleMu.Unlock()
		return nil, nil, err
	}
	entry.client = client
	entry.cleanup = cleanup
	entry.instID = instID
	entry.runnerGeneration = gen
	entry.credentials = credentials
	entry.taskID = task.ID
	entry.lifecycleMu.Unlock()
	return client, entry, nil
}

// startGeneration 解析会话工作区的 Runner lease generation(方案 §7 fencing)。
// Loopback 恒为 1; Sandbox 路径由持久 lease 提供(并刷新到期时间)。
func (s *scheduler) startGeneration(ctx context.Context, sessionKey string) (uint64, error) {
	if s.cfg.Runtime == nil {
		return 0, fmt.Errorf("runtime is not configured")
	}
	return s.cfg.Runtime.ResolveGeneration(ctx, sessionKey)
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

// runtimeConfigDir 返回 credential 刷新读写的配置目录(审查 C4):
// 生产 Runner 模式由部署注入(workspace 共享卷 config/, Runner 可见);
// 回退到 configDirFor(loopback/测试)。
// runtimeConfigDir 返回 session 在 runnerGeneration 下的运行时配置目录
// (审查 C1/I6: 必须按 generation 隔离, 与容器挂载的 config/g<gen> 一致)。
func (s *scheduler) runtimeConfigDir(sessionKey string, runnerGeneration uint64) string {
	if s.cfg.RuntimeConfigDir != nil {
		return s.cfg.RuntimeConfigDir(sessionKey, runnerGeneration)
	}
	return s.configDirFor(sessionKey)
}

func (s *scheduler) cleanupWorkerEntryBestEffort(ctx context.Context, entry *workerEntry) {
	if entry.cleanup != nil {
		// 审查 C1/I7: 清理/关闭携带当前凭据集 JTI, Worker 校验通过后
		// 才能优雅停止; 任务终态后 credentials 可能已被撤销, 但 Worker
		// 内存 session 的 JTI 集随配置重载更新, firstJTI 仍在集合内。
		entry.cleanup(controlJTIFor(entry.credentials))
	}
	s.revokeCredentialSetBestEffort(ctx, entry.credentials)
}

// revokeCredentialSetBestEffort persists every JTI with the token's exact
// expiry. Token material and full JTIs are never logged.

// releaseRunnerLeaseBestEffort 释放会话的 Runner lease(best-effort):
// loopback/static 无持久 lease 返回 nil; Sandbox 路径失败只记录日志,
// 不阻断调用方(容量由 TTL 兜底)。
func (s *scheduler) releaseRunnerLeaseBestEffort(sessionKey string, generation uint64) {
	if s.cfg.Runtime == nil || generation == 0 {
		return
	}
	if err := s.cfg.Runtime.ReleaseRunnerLease(context.Background(), sessionKey, generation); err != nil {
		slog.WarnContext(context.Background(), "scheduler: release runner lease after init failure failed",
			"session_key", sessionKey, "generation", generation, "error", err)
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
	// round11 审查(C3): 与 CancelWorker 互斥(同 session), 跨 session 并行。
	entry.startMu.Lock()
	defer entry.startMu.Unlock()
	entry.lifecycleMu.Lock()
	defer entry.lifecycleMu.Unlock()
	maxTurns, err := s.agentMaxTurns(ctx)
	if err != nil {
		return fmt.Errorf("resolve agent max turns: %w", err)
	}
	startReq := &workerv1.StartSessionRequest{
		SessionKey: task.SessionKey,
		// 方案 §7: workspace_key + runner_generation 由 Platform 写入, Runner 校验。
		WorkspaceKey:     task.SessionKey,
		RunnerGeneration: entry.runnerGeneration,
		RuntimePolicy: &workerv1.RuntimePolicy{
			MaxTurns: maxTurns, MaxHistoryBytes: defaultMaxHistoryBytes,
			MaxWorkingBytes: defaultMaxWorkingBytes, MaxOutputBytes: defaultMaxOutputBytes,
			TaskTimeoutSeconds: uint32(s.cfg.TaskTimeoutSeconds),
			CapabilityVersion:  CapabilityVersion, PolicyDigest: s.cfg.Registry.Digest(),
		},
	}
	if !task.FreshSession && s.cfg.Coordinator != nil {
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
			// 审查 R4-I6: 恢复读取按 Prepare 时的 max_bundle_bytes 限长。
			startReq.MaxBundleBytes = uint64(restore.MaxBundleBytes)
		}
	}
	if !task.FreshSession && startReq.SnapshotId == "" && task.SnapshotID != "" {
		startReq.SnapshotId = task.SnapshotID
		startReq.SnapshotChecksum = task.SnapshotChecksum
	}
	// round11 审查(C3): StartSession 使用独立超时——RPC 卡住时本 session
	// 派发失败并销毁 Worker(后续 tick 重建), 但不再阻塞其他工作区
	// (per-entry lifecycleMu 已释放)。
	startTimeout := s.cfg.StartSessionTimeout
	if startTimeout <= 0 {
		startTimeout = DefaultStartSessionTimeout
	}
	startCtx, cancelStart := context.WithTimeout(ctx, startTimeout)
	defer cancelStart()
	// 决策 D2.1: 每任务新 Worker, StartSession 恰好调用一次。
	if _, err := entry.client.StartSession(startCtx, startReq); err != nil {
		s.destroyTaskWorkerLocked(task.SessionKey, entry)
		return err
	}
	return nil
}

func (s *scheduler) agentMaxTurns(ctx context.Context) (uint32, error) {
	maxTurns := defaultMaxTurns
	if s.cfg.RuntimeSettings != nil {
		configured, err := s.cfg.RuntimeSettings.GetAgentMaxTurns(ctx)
		if err != nil {
			return 0, err
		}
		maxTurns = configured
	}
	if err := domain.ValidateAgentMaxTurns(maxTurns); err != nil {
		return 0, err
	}
	return uint32(maxTurns), nil
}

// startWorkerProcess creates the Worker after its runtime JSON and fixed
// mykey.py loader have been written.
func (s *scheduler) startWorkerProcess(ctx context.Context, sessionKey string, runtimeConfigFiles map[string][]byte) (workerclient.WorkerClient, string, func(string), uint64, error) {
	inst, err := s.cfg.Runtime.Start(ctx, worker.StartRequest{
		SessionKey:         sessionKey,
		ConfigDir:          s.configDirFor(sessionKey),
		RuntimeDir:         s.cfg.RuntimeRoot,
		RuntimeConfigFiles: runtimeConfigFiles,
	})
	if err != nil {
		return nil, "", nil, 0, err
	}
	return inst.Client, inst.InstID, inst.Cleanup, inst.RunnerGeneration, nil
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

// destroyTaskWorker 销毁任务 Worker(决策 D2.1: 任务终态即销毁): 优雅停止
// 进程(带 capability JTI)并撤销凭证集。任务终态(成功/失败/取消/超时)后
// 调用——下一任务冷启动全新进程, 不继承未提交内存状态。
func (s *scheduler) destroyTaskWorker(sessionKey string) {
	s.mu.Lock()
	entry := s.workers[sessionKey]
	s.mu.Unlock()
	if entry == nil {
		return
	}
	entry.lifecycleMu.Lock()
	defer entry.lifecycleMu.Unlock()
	s.destroyTaskWorkerLocked(sessionKey, entry)
}

// destroyTaskWorkerLocked 是 destroyTaskWorker 的持锁变体: 调用方必须已
// 持有 entry.lifecycleMu(如 completeSuccess 的错误分支)。内部只获取 s.mu
// (removeWorkerEntry)与执行清理, 不再获取 lifecycleMu, 避免持锁重入死锁。
func (s *scheduler) destroyTaskWorkerLocked(sessionKey string, entry *workerEntry) {
	if s.workers[sessionKey] != entry {
		return
	}
	s.removeWorkerEntry(sessionKey, entry)
	s.cleanupWorkerEntryBestEffort(context.Background(), entry)
}
