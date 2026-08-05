# Round 13 根本性收拢 — 生命周期不变量结构强制

- Task root: `.tasks/sandbox-runner-refactor`（EPIC active）
- Subtask: SUBTASKS.csv id=25
- Truth: 本 DESIGN.md + SUBTASKS.csv id=25
- 决策（用户确认，2026-08-05）: "直接从根源优化, 不是打补丁"——三层结构强制全做

## 根因（Round 12 审查后结论）

"任务终态 ⇔ 全部资源恰好释放一次"是核心不变量，但实现是**约定式**的：
`finalizeOrFail + destroyTaskWorker + KickSession` 三件套在 4 个文件（dispatch/
scheduler/reaper/checkpoint）被手写 20+ 次，漏一处即泄漏。补丁级修复（M1/M2/
I5/I6 等）只能逐个补点，病根是"不变量没有单一表达点、没有结构强制、没有测试强制"。

## 三层结构强制

### L1 单一收尾 API: terminateTask
- 签名: `terminateTask(ctx, task, status, deliveryType, code, message, traceID) domain.Task`
- 固定顺序: destroyTaskWorker(幂等) → finalizeOrFail(失败注册 pendingFinalize) → KickSession
- destroy 先于 finalize: 终态提交前资源已释放, 同 session 新任务不可能在
  "提交与销毁之间" claim 并撞上旧 entry——独立审查发现的替换窗口结构性消除
- 替换全部非成功终态调用点(dispatch 约 9 处 + tick LEASE_EXPIRED + reaper + deadline)
- 例外(文档化): completeSuccess 成功路径必须两段式(captureTaskDeliverableFiles
  依赖 Worker 存活 + 持 entry.lifecycleMu), 保持显式 destroy+finalize, 不收拢
- completeSuccess 失败分支(持锁)同样不收拢进 terminateTask(避免锁重入),
  但其 destroy→finalize 顺序已正确

### L2 workerEntry 生命周期状态
- entry 增加 `destroyed atomic.Bool`; destroy 路径 CAS(false→true), 失败即返回
- "销毁恰好一次"从 map 身份检查升级为对象自身状态(销毁与 map 归属解耦)
- CancelWorker/cancelRPC 对 destroyed entry 直接跳过(不向已销毁容器发 RPC)

### L3 泄漏不变量测试强制
- `assertNoWorkerLeaks(t, sched)` helper: 断言 sched.workers 为空
- 挂到全部 dispatch 终态路径测试(新 scheduler_terminate_test.go 逐路径覆盖)
- 未来任何新分支漏销毁 → 测试直接红, 不依赖下一轮人肉审查

## 不做

- 全量状态机重写(defer 单出口 + terminateTask 已等价于收尾状态机, 重写是
  最大不稳定源)
- completeSuccess 成功路径的两段式例外不动(capture 依赖 Worker 存活是真实
  业务约束)

## 验证

- 新增测试: terminateTask 顺序(先销毁后终态化) / 幂等(双销毁一次清理) /
  destroyed 跳过 cancel / 各终态路径无泄漏(收拢回归)
- 全量: Go 18 包 -p 1(DB 套件) + race 5 关键包 + vet + GOOS=linux build +
  worker-python + tests/ + bot_poller + compose config + diff check
