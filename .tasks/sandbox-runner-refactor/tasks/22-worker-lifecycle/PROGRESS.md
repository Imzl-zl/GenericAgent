# Progress

- Shape: durable (Epic child: sandbox-runner-refactor)
- Parent: ../PROGRESS.md
- FinalizationStatus: active
- Truth: .tasks/sandbox-runner-refactor/tasks/22-worker-lifecycle/TODO.csv
- Current: P2 任务 1 梳理影响面
- Latest validation: P1 已提交(11d5510)且全量验证绿
- Next: 读 scheduler_worker/dispatch/checkpoint 全文, 列出去留清单

# Design Drift

（无）

# P2 完成(2026-08-05)
- createTaskWorker: 每任务全新 Worker(lease+签发+进程), 无复用循环; 任务终态(成功/失败/取消/超时/requeue)统一 destroyTaskWorker(Stop+撤销+移除)
- 删除: prepareWorkerEntry 复用检查/workerEntryIsCurrent/startOnce/started/startErr/lastUsedAt/rotateWorkerCredentials/stopSessionWorker/evictIdleWorkers+WorkerIdleTTL 配置(main.go flag+API 字段+compose env)/mcpSnapshotRequiresReplacement/routingSnapshotRequiresReplacement
- 保留: executing 标记+startMu(round11 C3 取消竞态防护)/cancelOnce 合并/IdleTimeout 任务卡死检测/shutdownAllWorkers
- 语义决策: resolveRoutingSnapshot 默认 provider 恒第一——每任务新 Worker 重新解析, 默认切换立即生效(集成测试 ga-existing-after-default/ga-existing-oai-after-claude 断言同步更新)
- 集成测试适配: 进程隔离断言(tasklist 毫秒级枚举替代 psutil 18s 全扫描)+任务活跃窗口内采样(任务即进程后进程只活秒级)
- 验证: Go 18 包(DB)+race 4 包+worker-python 116+契约/安全/集成 47+compose+linux build+diff check 全绿
