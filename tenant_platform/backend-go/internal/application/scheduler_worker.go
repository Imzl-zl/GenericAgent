package application

import (
	"context"
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

// workerEntry holds the Worker process bound to ONE task(决策 D1: 任务即
// 进程)。任务终态由 destroyTaskWorker 销毁, 进程内不复用。
//
// cancelCall(Round16-B8): 取消 RPC 的进程内串行器, 从 scheduler_lease.go
// 移入本文件——它与 claim lease 心跳无关, 只属于 CancelWorker 路径。
type cancelCall struct {
	mu       sync.Mutex
	inflight bool         // 是否有取消 RPC 执行中(并发合并)
	done     bool         // 已成功(终态缓存, 不再重试)
	err      error        // 最近一次结果
	notify   chan struct{} // inflight 完成通知(等待者)
}

type workerEntry struct {
	client      workerclient.WorkerClient
	cleanup     func(capabilityJTI string)
	instID      string
	sessionKey  string
	// taskID 是本 entry 绑定的任务(round12 审查 M1): 销毁时清理 cancelOnce
	// 条目, 防止按任务数无界增长。
	taskID string
	// destroyed 标记 entry 已被销毁(round13 L2): CAS 保证"销毁恰好一次"
	// 从 map 身份检查升级为对象自身状态——销毁与 map 归属解耦, 替换窗口
	// 类问题从结构上不可能再出现。
	destroyed atomic.Bool
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
	// round13 L2: 已销毁的 entry 不再向容器发取消 RPC(容器已停, RPC 只会
	// 超时浪费; 任务状态由终态化路径负责)。
	if entry.destroyed.Load() {
		return nil
	}
	// round11 审查(C3): 取消与同 session 的 StartSession 互斥用 per-entry
	// startMu; 与 checkpoint 收尾(lifecycleMu)不互斥。
	// 任务尚未进入执行阶段时跳过 RPC——dispatch 会检测 durable
	// cancel_requested_at 并销毁 Worker, 无需向 Worker 发取消。
	// 决策 D1(任务即进程): entry 只服务一个任务, 无需 currentness 复核——
	// 迟到的取消若命中已终态任务的旧 entry, 最多向新任务的 Worker 发一次
	// no-op CancelTask(task_id 不匹配, Worker 侧拒绝, accepted=false)。
	entry.startMu.Lock()
	defer entry.startMu.Unlock()
	if !entry.executing.Load() {
		return nil
	}
	cancelCtx, cancel := context.WithTimeout(ctx, workerControlTimeout)
	defer cancel()
	return entry.client.CancelTask(cancelCtx, entry.sessionKey, task.ID, entry.runnerGeneration, controlJTIFor(entry.credentials))
}

// createTaskWorker 为任务创建全新 Worker 进程(决策 D1: 任务即进程, 不复用)。
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
		// 审查 F1: lease 已获取/续租但初始化失败——只归还容量
		// (ExpireRunnerLease), 不清空 container_id/stale_container_id:
		// 接管后旧容器可能仍挂载 workspace, 引用必须保留给下一轮
		// EnsureRunner 定向销毁, 否则残留容器失去 DB 追踪只能等
		// 扫描/TTL 兜底。语义与 sandbox_runtime 失败路径一致。
		s.expireRunnerLeaseBestEffort(task.SessionKey, generation)
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
// hash 推导与容器挂载/checkpoint 共用 domain.WorkspaceDirHash 唯一实现
// (审查 B1 收敛): 目录名同源, 改算法不会局部漂移。
func (s *scheduler) configDirFor(sessionKey string) string {
	if !s.cfg.SessionScopedConfig {
		return s.cfg.ConfigRoot
	}
	digest, err := domain.WorkspaceDirHash(sessionKey)
	if err != nil {
		// loopback/测试的 key 均为合法格式; 非法 key 显式暴露并隔离到
		// 固定目录, 后续写配置失败会继续显式报错(fail-closed)。
		slog.Warn("configDirFor: invalid workspace key", "session_key", sessionKey, "error", err)
		return filepath.Join(s.cfg.ConfigRoot, "invalid-session")
	}
	return filepath.Join(s.cfg.ConfigRoot, digest)
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

// expireRunnerLeaseBestEffort 归还会话的 Runner 容量(best-effort), 但保留
// 容器引用(container_id/stale_container_id)供下一轮接管定向销毁——
// 与 sandbox_runtime 失败路径语义一致(审查 F1)。loopback/static 无持久
// lease 返回 nil; Sandbox 路径失败只记录日志, 不阻断调用方。
func (s *scheduler) expireRunnerLeaseBestEffort(sessionKey string, generation uint64) {
	if s.cfg.Runtime == nil || generation == 0 {
		return
	}
	if err := s.cfg.Runtime.ExpireRunnerLease(context.Background(), sessionKey, generation); err != nil {
		slog.WarnContext(context.Background(), "scheduler: expire runner lease after init failure failed",
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
			// round13 审查(D4): 统一走 disposeWorkerEntryCore(本函数已持有
			// lifecycleMu)——修复前直接 cleanup 绕过 destroyed CAS, 与
			// dispatch defer 的 teardown 并发时可能重复清理。
			s.disposeWorkerEntryCore(context.Background(), task.SessionKey, entry)
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
	// 决策 D1: 每任务新 Worker, StartSession 恰好调用一次。
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
// round13 审查(D4): 统一走 disposeWorkerEntry(身份校验 + destroyed CAS +
// lifecycleMu), 不再绕过 CAS 直接 cleanup——shutdown 与并发 dispatch 的
// deferred teardown 之间保证每 entry 恰好清理一次。
func (s *scheduler) shutdownAllWorkers(ctx context.Context) {
	s.mu.Lock()
	entries := make([]*workerEntry, 0, len(s.workers))
	for _, entry := range s.workers {
		entries = append(entries, entry)
	}
	clear(s.workers)
	s.mu.Unlock()
	for _, entry := range entries {
		s.disposeWorkerEntry(ctx, entry.sessionKey, entry)
	}
}

// disposeWorkerEntry 统一销毁入口(round13 审查 D4): map 身份校验 + destroyed
// CAS + cancelOnce + lifecycleMu + cleanup 全部收敛于此。所有清理路径
// (任务终态/初始化失败/恢复失败/平台关闭)必须走本方法或其持锁变体,
// 保证同一 entry 恰好清理一次。
func (s *scheduler) disposeWorkerEntry(ctx context.Context, sessionKey string, entry *workerEntry) {
	if entry == nil {
		return
	}
	entry.lifecycleMu.Lock()
	defer entry.lifecycleMu.Unlock()
	s.disposeWorkerEntryCore(ctx, sessionKey, entry)
}

// disposeWorkerEntryCore 是 disposeWorkerEntry 的持锁变体: 调用方必须已
// 持有 entry.lifecycleMu(如 completeSuccess 的错误分支、startSessionOnWorker
// 的恢复失败路径)。检查与删除在 s.mu 下原子完成(审查: 无锁读 s.workers 与
// createTaskWorker 写入并发会触发 Go map 读写竞争; 且同 session 下一任务在
// 终态提交与销毁之间派发时可能误跳过销毁)。
// round12 审查(I1 补充/独立审查): 身份不匹配(同 session 新任务已替换
// map entry)时仍必须清理旧 entry——旧任务已终态, 其容器/凭据不得因被
// 替换而泄漏(completeSuccess 终态提交与销毁之间的替换窗口); 只是不
// 触碰 map 中的新 entry。
// round13 L2: destroyed CAS 保证同一 entry 恰好清理一次。
func (s *scheduler) disposeWorkerEntryCore(ctx context.Context, sessionKey string, entry *workerEntry) {
	s.mu.Lock()
	current := s.workers[sessionKey] == entry
	if current {
		delete(s.workers, sessionKey)
	}
	s.mu.Unlock()
	if !entry.destroyed.CompareAndSwap(false, true) {
		return
	}
	if entry.taskID != "" {
		s.cancelOnce.Delete(entry.taskID)
	}
	s.cleanupWorkerEntryBestEffort(ctx, entry)
}

// destroyTaskWorkerEntry 按身份销毁指定 entry(round12 审查 I1): 仅当
// s.workers[sessionKey] 仍指向该 entry 时删除并清理——旧任务收尾不得误毁
// 同 session 新任务的 Worker(dispatch 的 deferred teardown 与并发派发之间的
// 竞争窗口由身份校验闭合)。统一走 disposeWorkerEntry(round13 审查 D4)。
func (s *scheduler) destroyTaskWorkerEntry(sessionKey string, entry *workerEntry) {
	s.disposeWorkerEntry(context.Background(), sessionKey, entry)
}

// destroyTaskWorker 销毁任务 Worker(决策 D1: 任务终态即销毁): 优雅停止
// 进程(带 capability JTI)并撤销凭证集。任务终态(成功/失败/取消/超时)后
// 调用——下一任务冷启动全新进程, 不继承未提交内存状态。
func (s *scheduler) destroyTaskWorker(sessionKey string) {
	s.mu.Lock()
	entry := s.workers[sessionKey]
	s.mu.Unlock()
	if entry == nil {
		return
	}
	s.destroyTaskWorkerEntry(sessionKey, entry)
}

// destroyTaskWorkerLocked 是 destroyTaskWorker 的持锁变体: 调用方必须已
// 持有 entry.lifecycleMu(如 completeSuccess 的错误分支)。检查与删除在
// s.mu 下原子完成(审查: 无锁读 s.workers 与 createTaskWorker 写入并发会
// 触发 Go map 读写竞争; 且同 session 下一任务在终态提交与销毁之间派发时
// 可能误跳过销毁)。内部不再获取 lifecycleMu, 避免持锁重入死锁。
// round13 审查(D4): 统一走 disposeWorkerEntryCore。
func (s *scheduler) destroyTaskWorkerLocked(sessionKey string, entry *workerEntry) {
	s.disposeWorkerEntryCore(context.Background(), sessionKey, entry)
}
