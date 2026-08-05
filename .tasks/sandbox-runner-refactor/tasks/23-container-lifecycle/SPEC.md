# 架构重构"任务即进程" P3: Runner 容器生命周期简化 — Execution Spec

**Goal:** 按决策 D1 完成容器生命周期简化。Runner lease + generation fencing 保留（防旧容器复活），语义简化为**任务容器**：任务终态即销毁，无 idle 复用。

**Background（P1/P2 完成后的现状）:**
- P2 已删除 Worker 进程层复用：每任务新进程，任务终态 destroyTaskWorker（Stop + 撤销）。Sandbox 路径下 Stop → runtime 层 → Manager destroy → 容器销毁。
- P3 需确认/收尾容器层：SandboxWorkerRuntime 的 lease 续租 goroutine（任务期间续租）、Manager 的 GA_RUNNER_IDLE_TTL（容器 idle 回收）、sweepOrphans/stale destroy（保留）、lease 语义（任务容器 vs 会话容器）。

**Decisions（D1，用户已确认）:**
- D3.1 Runner 容器=任务容器：创建于任务派发，销毁于任务终态（P2 已接线）；无 idle 复用路径。
- D3.2 Runner lease + generation fencing 保留：任务开始解析/续租（防旧容器复活），generation 递增语义不变。
- D3.3 Manager 的 GA_RUNNER_IDLE_TTL 与容器 idle 回收：语义与任务容器冲突（容器不再 idle 驻留），评估删除或保留为兜底（孤儿容器清理）。

**Constraints:**
- 不动 checkpoint/JTI/消息事务链/compose 拓扑/Manager 控制面（HMAC+mTLS）。
- 每行先写测试（红）再实现（绿）。
- 验证基线同 P1/P2。

**Non-goals:**
- 不改 Python 侧；不引入新依赖。

**Architecture:** 收尾容器层语义：确认 Sandbox 路径每任务容器生命周期完整（创建→执行→销毁），消除残余的 idle/复用语义，lease 语义文档化。

**Sync targets:** sandbox_runtime.go（lease 续租 goroutine）、sandbox/manager.go（GA_RUNNER_IDLE_TTL 评估）、cmd/sandbox-manager/main.go（env 接线）、scheduler_lease.go（lease 语义）、相关测试。

**Final validation（P3 完成门）:** 同 P1/P2 六条命令全绿 + 集成全链路。
