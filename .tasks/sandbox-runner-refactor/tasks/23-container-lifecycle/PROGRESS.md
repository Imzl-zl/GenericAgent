# Progress

- Shape: durable (Epic child: sandbox-runner-refactor)
- Parent: ../PROGRESS.md
- FinalizationStatus: active
- Truth: .tasks/sandbox-runner-refactor/tasks/23-container-lifecycle/TODO.csv
- Current: P3 任务 1 梳理容器层现状
- Latest validation: P2 已提交(98b2cfe)且全量验证绿
- Next: 读 sandbox_runtime/manager/main.go, 列出容器生命周期去留清单

# Design Drift

（无）

# P3 完成(2026-08-05)
- 梳理结论: P2 后容器生命周期已是任务容器闭环(Sandbox 路径: 任务派发 Start 创建容器+lease 续租 goroutine, 任务终态 cleanup 停止续租+Shutdown+Destroy+释放 lease)
- 无真实容器 idle 回收代码: Manager idle-ttl flag 仅日志参考; GA_RUNNER_IDLE_TTL 实为 lease 时长(名字误导, 保留不改 compose); WorkerIdleTTL 已随 P2 删除
- EnsureRunner 同 generation 复用分支仅崩溃恢复场景出现(幂等安全), 注释澄清
- 文档: GA_SANDBOX_RUNNER_REFACTOR.zh-CN.md 运行粒度/流程图/决策表更新为任务容器语义; minRunnerCertTTL 注释同步
- 验证: Go 18 包(DB)+race 4 包+worker-python 116+契约/安全/集成 47+compose+linux build+diff check 全绿
