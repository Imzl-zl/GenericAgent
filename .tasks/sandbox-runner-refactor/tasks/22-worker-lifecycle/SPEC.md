# 架构重构"任务即进程" P2: 删除常驻 Worker 生命周期 — Execution Spec

**Goal:** 按决策 D1 删除常驻 Worker 生命周期。每任务创建全新 Worker 进程（容器内），任务终态销毁；删除 ensureWorker 复用逻辑、idle eviction、startOnce、executing 状态机中为复用服务的部分。取消语义保留：CancelTask 仍须在任务执行中送达。

**Background（P1 完成后的现状）:**
- `scheduler_worker.go` 551 行：workerEntry 持有 client/cleanup/instID/sessionKey/taskID/credentials/lifecycleMu/startMu/executing/startOnce/started/runtimeMaxTurns/lastUsedAt/runnerGeneration；ensureWorker 按 sessionKey 复用 + taskChanged 边界 rotateWorkerCredentials（P1 新增，P2 后不再需要——新 Worker 直接 issueInitialWorkerCredentials）。
- dispatch 流程：ensureWorker → prepareWorkerEntry（复用检查：lease generation/maxTurns/MCP/routing snapshot）→ startSessionOnWorker（startOnce 幂等）→ ExecuteTask → 终态撤销 + evict。
- round11 C3：startMu 串行 StartSession 与 CancelWorker；executing 标记"未执行不发取消 RPC"。P2 后每任务新 Worker：取消只需按 taskID 找到活跃 Worker 发 RPC，无复用竞态。

**Decisions（D1，用户已确认）:**
- D2.1 每任务全新 Worker：dispatch 开始时创建（lease generation + 签发 + runtime.Start + StartSession），任务终态（成功/失败/取消/超时）销毁（Stop + 撤销 + 移除 entry）。
- D2.2 删除为复用服务的全部机制：ensureWorker 复用分支、prepareWorkerEntry 替换检查、startOnce/started/startErr、executing 原子标记、lastUsedAt/idle eviction、taskChanged 轮换（rotateWorkerCredentials 删除）。
- D2.3 保留取消语义：CancelWorker 按 sessionKey（串行调度）或 taskID 定位活跃 Worker 发 CancelTask RPC；Worker 未就绪/已销毁时依赖 durable cancel_requested_at + dispatch 检查（既有机制）。
- D2.4 保留：Runner lease + generation fencing、checkpoint、JTI 撤销、心跳、recover-after-restart、容量排队。

**Constraints:**
- 只删生命周期复用；不动 checkpoint/JTI/lease/消息事务链/compose 拓扑。
- 每行先写测试（红）再实现（绿）。
- 验证基线同 P1（Go 18 包 + race + worker-python + 契约/安全/集成 + compose + linux build）。

**Non-goals:**
- P3（容器语义简化）在 P2 完成后独立做。
- 不改 Python 侧（P1 已把任务边界刷新放 ExecuteTask 入口，P2 后每任务新 Worker 走 StartSession 全量加载，_refresh_task_credentials 保留为幂等兜底）。

**Architecture:** workerEntry 生命周期=单任务。dispatch 创建/终态销毁；map 保留 sessionKey→entry（串行调度语义，任务间不复用）。Cancel/心跳/checkpoint 按既有路径查 entry。

**Sync targets:** scheduler_worker.go（ensureWorker/prepareWorkerEntry/startSessionOnWorker/cleanupWorkerEntryBestEffort/evictWorkerAfterFailure/shutdownAllWorkers）、scheduler_dispatch.go（dispatch 创建/销毁接线）、scheduler_checkpoint.go（终态销毁）、scheduler_capacity_test.go/scheduler_test.go/scheduler_checkpoint_test.go/worker_credential_test.go（复用语义测试改写）。

**Final validation（P2 完成门）:** 同 P1 六条命令全绿 + 集成全链路（提交→执行→checkpoint→取消→重启恢复）。
