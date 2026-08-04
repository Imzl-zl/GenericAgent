package application

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
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
	cleanup            func(capabilityJTI string)
	instID             string
	sessionKey         string
	taskID             string // 当前已下发 capability 的 task(方案 §7 per-task capability)
	credentials        workerCredentialSet
	pendingRefresh     *pendingCredentialRefresh
	pendingRevocations []workerCredentialSet
	lifecycleMu        sync.Mutex
	startOnce          sync.Once
	startErr           error
	started            bool
	runtimeMaxTurns    uint32
	// lastUsedAt is updated every time a task is dispatched to this Worker.
	// Used by the idle eviction reaper to reclaim memory from long-idle
	// sessions (pattern: Kubernetes pod eviction, AWS Lambda container TTL).
	lastUsedAt time.Time
	// runnerGeneration 是该 Worker 的 Runner lease generation(方案 §7 fencing)。
	// loopback 路径恒为 1; Sandbox 路径由持久 lease 提供。
	runnerGeneration uint64
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
		e.runtimeMaxTurns = req.GetRuntimePolicy().GetMaxTurns()
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
			// worker 已不存在 = 已被 dispatch 销毁重建/evict, 取消已生效,
			// 视为成功而不是向用户报错(否则 CancelTask API 在
			// cancel-during-StartSession 竞态下返回 500, 尽管任务已中断)。
			call.err = nil
			return
		}
		call.err = entry.client.CancelTask(ctx, entry.sessionKey, task.ID, entry.runnerGeneration, firstJTI(entry.credentials))
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
			return s.initializeWorkerEntry(ctx, task, entry)
		}
		s.mu.Unlock()

		entry.lifecycleMu.Lock()
		if !s.workerEntryIsCurrent(task.SessionKey, entry) {
			entry.lifecycleMu.Unlock()
			continue
		}
		// 审查 R5-I2: 先把 entry.taskID 切换到当前任务, 再执行
		// prepareWorkerEntry——prepare 内部(凭据到期/待刷新)触发的
		// credential 刷新以 entry.taskID 签发 token 并持久化 JTI。若仍指向
		// 已终态旧任务, 新 token 会挂到无法被撤销的行上(旧任务终态事务已
		// 提交撤销旧集, 恢复扫描只处理未终态任务), 崩溃窗口内无人撤销。
		taskChanged := entry.taskID != task.ID
		generationBefore := entry.credentials.Generation
		entry.taskID = task.ID
		replace, err := s.prepareWorkerEntry(ctx, entry)
		if err != nil {
			entry.lifecycleMu.Unlock()
			return nil, entry, err
		}
		if !replace {
			entry.lastUsedAt = time.Now().UTC()
			// per-task capability(方案 §7): 复用 Worker 时新任务必须签发
			// 新 token(绑定 task_id/generation); prepare 已因到期/待刷新签发
			// 过(Generation 已递增)则跳过, 避免重复签发。
			if taskChanged && s.cfg.TokenIssuer != nil && entry.credentials.Generation == generationBefore {
				if err := s.refreshWorkerCredentials(ctx, entry); err != nil {
					entry.lifecycleMu.Unlock()
					return nil, entry, err
				}
			}
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
	// 审查(review I4): 复用路径必须确认持久 lease generation 未被接管
	// (异主接管/重启恢复会 generation+1)。ResolveGeneration 同时刷新 lease
	// 到期时间(防 idle 过期); 不一致说明旧容器已 fence, 强制替换 Worker。
	if entry.runnerGeneration > 0 && s.cfg.Runtime != nil {
		generation, err := s.cfg.Runtime.ResolveGeneration(ctx, entry.sessionKey)
		if err != nil {
			return false, fmt.Errorf("resolve runner generation on reuse: %w", err)
		}
		if generation != entry.runnerGeneration {
			slog.InfoContext(ctx, "scheduler: runner lease generation changed; replacing worker",
				"session_key", entry.sessionKey,
				"old_generation", entry.runnerGeneration,
				"new_generation", generation)
			return true, nil
		}
	}
	if entry.started && entry.runtimeMaxTurns > 0 {
		maxTurns, err := s.agentMaxTurns(ctx)
		if err != nil {
			return false, err
		}
		if entry.runtimeMaxTurns != maxTurns {
			return true, nil
		}
	}
	if err := s.flushPendingCredentialRevocations(ctx, entry); err != nil {
		return false, fmt.Errorf("flush pending credential revocations: %w", err)
	}
	if entry.pendingRefresh != nil {
		if err := s.refreshWorkerCredentials(ctx, entry); err != nil {
			return false, err
		}
	}
	mcpReplace, err := s.mcpSnapshotRequiresReplacement(ctx, entry.credentials.MCPSnapshot.ID)
	if err != nil || mcpReplace {
		return mcpReplace, err
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
	entry.lastUsedAt = time.Now().UTC()
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
		// 内存 session 的 JTI 集在 Reload 时更新, firstJTI 仍在集合内。
		entry.cleanup(firstJTI(entry.credentials))
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
	entry.lifecycleMu.Lock()
	defer entry.lifecycleMu.Unlock()
	if !s.workerEntryIsCurrent(task.SessionKey, entry) {
		return fmt.Errorf("worker replaced for session %s", task.SessionKey)
	}
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
			// 审查 R4-I6: 恢复读取按 Prepare 时的 max_bundle_bytes 限长。
			startReq.MaxBundleBytes = uint64(restore.MaxBundleBytes)
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

// evictWorkerAfterFailure 在任务失败/取消/异常终止后移除并销毁该 session
// 的 Worker(审查): 失败任务的内存历史与 working 未持久化, 复用会让下一
// 任务继承未提交状态, 违反"只有成功 task 推进 state"不变量。下一任务从
// 最后一个已提交 checkpoint 冷启动(generation+1 重建容器)。
func (s *scheduler) evictWorkerAfterFailure(sessionKey string) {
	s.mu.Lock()
	entry := s.workers[sessionKey]
	s.mu.Unlock()
	if entry == nil {
		return
	}
	entry.lifecycleMu.Lock()
	defer entry.lifecycleMu.Unlock()
	s.evictWorkerAfterFailureLocked(sessionKey, entry)
}

// evictWorkerAfterFailureLocked 是 evictWorkerAfterFailure 的持锁变体:
// 调用方必须已持有 entry.lifecycleMu(如 completeSuccess 的错误分支)。
// 内部只获取 s.mu(workerEntryIsCurrent/removeWorkerEntry)与执行清理,
// 不再获取 lifecycleMu, 避免持锁重入自死锁(审查: checkpoint 失败路径
// 在锁内调用公开版会永久卡死该工作区)。
func (s *scheduler) evictWorkerAfterFailureLocked(sessionKey string, entry *workerEntry) {
	if !s.workerEntryIsCurrent(sessionKey, entry) {
		return
	}
	s.removeWorkerEntry(sessionKey, entry)
	s.cleanupWorkerEntryBestEffort(context.Background(), entry)
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
