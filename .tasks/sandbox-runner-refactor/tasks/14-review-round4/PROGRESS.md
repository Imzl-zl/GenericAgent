# Progress

## 恢复区

- FinalizationStatus: active
- Truth: TODO.csv
- Parent: .tasks/sandbox-runner-refactor (epic)
- Current: 14/14 DONE
- Latest validation: Go `go test ./...` + `-race -p 1`（DB 测试经 TEST_DATABASE_URL=genericagent_test 全绿）; worker-python 90 passed; 契约+安全 21 passed; web build OK; compose config OK; GOOS=linux cross-build OK; go vet ./... OK
- Next: 整体完成门（最终 code review + verification）

## 关键决策

- 修复顺序按文件域分组；每个修复 TDD 闭环或现有测试扩展。
- 用 dbx（超级用户 admin）创建 ga_test 角色，TEST_DATABASE_URL 指向已存在的 genericagent_test 库，跑通全部 DB 依赖测试。
- 已知残余（未修复，记录）：孤儿 committed/result 文件（文件写完但 DB 事务失败的极窄窗口）无文件系统级 sweep；memory-template 历史模板 trailing whitespace 非本任务引入。

## 本轮修复清单（14 项）

| # | 问题 | 修复 |
|---|------|------|
| 1 | Web/OpenAPI 孤儿 | AdminLayout 恢复 sops 导航; platform.yaml 删除 3 个孤儿 schema |
| 2 | manifest 无界读取/顺序 | ReadFileBeneathLimited(1MiB)+条目数校验; RecordOutbound 先校验 outputs/ |
| 3 | Worker 启动竞态 | _session 先赋值再 start; agent_failed 检查移到 fencing 之后 |
| 4 | checkpoint 限长 | proto 加 max_bundle_bytes 端到端; stat 先查限再读 |
| 5 | bundle 身份校验 | session_key 必填+generation 比对(纯函数+Local 同步) |
| 6 | attach fail-closed | 失败销毁容器+释放 lease+返回错误 |
| 7 | budget 计量 | migration 0043+ConsumeCapabilityCall+handler 429/403/503+撤销联动清理 |
| 8 | runsc fail-closed | Profile.Validate 强制 runsc 或 AllowRunc |
| 9 | overlay tmpfs | GA_OVERLAY_ROOT=/ga/overlay tmpfs 128m; inspect 校验更新 |
| 10 | 重复派发 | dispatchInFlight sync.Map gate |
| 11 | dispatch 裸 return | MarkDispatchStarted/MarkRunning/GetTask 失败 evict+尽力终态化 |
| 12 | JTI 原子 | finalizeTerminal/CompleteSucceeded 事务内撤销; SetTaskCapabilityJTIs fail-closed |
| 13 | /new 语义 | ResetWorkspaceForNewSession 取消 queued; reset_at 保留到 fresh 成功 |
| 14 | checkpoint 清理 | SweepExpiredCheckpoints(tick 5min 节流)+QuarantineExpiredWritingSnapshots |
