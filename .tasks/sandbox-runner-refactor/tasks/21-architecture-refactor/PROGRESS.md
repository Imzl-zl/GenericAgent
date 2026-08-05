# Progress

- Shape: durable (Epic child: sandbox-runner-refactor)
- Parent: ../PROGRESS.md
- FinalizationStatus: active
- Truth: .tasks/sandbox-runner-refactor/tasks/21-architecture-refactor/TODO.csv
- Current: P1 任务 1 proto 删 ReloadCredentials
- Latest validation: 基线已验证（round11 提交前：Go 18 包 + DB 全绿；worker-python 116 passed）
- Next: 安装 protoc 插件 → 改 worker.proto → 重生成 bindings → 契约测试

# Design Drift

（无）

# P1 完成(2026-08-05, 分支 refactor/task-per-process)
- 提交基线: round11 已提交(7fe363b + b9eb824), 分支基于干净 main
- proto: worker.proto 删 ReloadCredentials RPC+2 消息; Go/Python bindings 重生成(protoc-gen-go v1.34.2 匹配); 契约测试改为断言删除
- Go: worker_credential.go 删 6 函数+pendingCredentialRefresh+workerCredentialSet.Generation/Checksum+pendingRevocations; runtime_config.go 删 checksum 机制; scheduler_worker.go 删刷新调用点; workerclient 删方法; 新增 rotateWorkerCredentials(任务边界签发+写配置+撤销旧集); 终态撤销失败改日志(恢复路径兜底)
- Python: credential_config.py 删 generation/checksum 校验; managed_agent.py 删 reload_credentials+新增 _refresh_task_credentials(ExecuteTask 入口从磁盘重载, GA 原生 reload_mykeys mtime 已接线); session 字段清理
- 验证: Go 18 包(DB)+race 4 包全绿; worker-python 116; 契约+安全+集成+冒烟 47; compose config; GOOS=linux build; diff check
- 关键不变式(已测试锁定): 签发 TTL 恒覆盖墙钟上限(MaxTaskWallClock+skew); 复用 Worker 任务边界轮换不挂旧任务 JTI; 无 ReloadCredentials RPC
