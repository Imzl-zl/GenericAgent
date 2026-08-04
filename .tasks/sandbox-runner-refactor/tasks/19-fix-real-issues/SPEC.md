# 19-fix-real-issues — 修复独立审查确认的 9 个真实问题

## Goal
修复 d004966..99e85bb 审查中确认仍然存在的 9 个问题(1 Critical 部署、1 Critical lease、
4 Important、2 Important 语义、1 Minor 顺序、1 Minor 磁盘残留),不引入新问题。

## 问题清单与修复方向

### P1 — lease / fencing / checkpoint(问题 2、3、9 的 store 部分)
- **B1(Critical)**: idle 回收后 `container_id` 残留 → 下次 takeover 移入
  `stale_container_id` → Manager 对已删除容器 ID 返回 NAME_REJECTED → 该工作区
  永久 WORKER_START_FAILED。
  - 修复 a: `ReleaseRunnerLease` 同时清空 `container_id`/`stale_container_id`/
    `control_endpoint`(发布语义:容器已销毁)。
  - 修复 b: Manager 销毁路径对"不存在的受控 Runner"幂等成功(与名称路径一致):
    `ManagerServer.handleDestroy` 的 ID 分支与 `Manager.DestroyRunner` 的 ID 分支
    先 `ContainerExists` 检查,不存在即成功。
- **B2(Important)**: 稳定 `GA_PLATFORM_INSTANCE_ID` 同时承担"渠道去重 ID"与
  "claim/lease owner",重启后新进程把旧进程的 running claim 当自己的续租,
  且同 owner 未过期 lease 不递增 generation → 复用持有旧 CA 的 Runner,mTLS 失败。
  - 修复: 拆分两个身份:
    - `sourceInstanceID`(稳定,env 或默认): Router.SourceInstance、task 去重键。
    - `processID`(每进程随机): claim_owner、runner lease owner、checkpoint lease
      owner、scheduler/taskService 的 PlatformInstanceID、JTI 持久化 owner。
    - 重启后新进程 RecoverAfterRestart 把旧进程过期 claim 中断/requeue;
      AcquireRunnerLease 异主且旧 owner 无活跃 claim → takeover,generation+1,
      stale_container_id=旧容器 → 销毁 → 新容器注入新 CA。mTLS 恢复。
- **B9a(Minor)**: checkpoint `Commit` 写 committed/result 后 `CompleteSucceeded`
  失败 → 文件永久残留。
  - 修复: scheduler_checkpoint.go 失败路径(任务确认未终态)调用 coordinator
    清理对应 committed/result 文件;Commit 内部错误路径也清理已写文件。

### P2 — 封禁用户(问题 5,Important)
- BlockUser 对未派发 starting 只写 cancel_requested_at → dispatch 直接 return nil
  → 任务永久 starting 占串行槽;个人任务 capability 在线校验不检查用户状态。
  - 修复 a: BlockUser 事务内对 `starting AND worker_dispatch_started_at IS NULL`
    走终态化(复用 RemoveMember 的 cancelRemovedMemberTasks 等价逻辑:撤销 JTI、
    写事件、取消 pending task_started、清 claim)。
  - 修复 b: `IsTaskCapabilityActive` 增加用户状态检查(personal 也要求
    users.status='approved'),封禁后下一次 LLM/Sophub 调用即被拒绝。

### P3 — 交付(问题 6、8,Important + Minor)
- **B6(Important)**: 共享卷上 Platform(10001)写 0700/0600 快照,Poller(10002,
  共享组 10003)必 EACCES。
  - 修复: delivery_service.go buildPayload 的目录 0o770/文件 0o640;
    deliverable_snapshot_unix.go 同样 0o770/0o640(setgid 卷上组继承 10003)。
- **B8(Minor)**: task_started 与 task_complete 并发发送,完成可能先于"处理中"。
  - 修复: ClaimPendingDeliveries 排除"同 task 存在 pending/sending 的
    task_started"的行(终态事务已把 pending task_started 置 cancelled,不会卡死)。

### P4 — 入站幂等(问题 7,Important)
- 消息行在副作用后写入 → 并发/崩溃窗口内 relay/团队命令重复执行。
  - 修复 a: 任务提交与入站消息行同事务(Store.SubmitTask 扩展可选 inbound
    message 同事务插入),任务路径零窗口。
  - 修复 b: 命令/relay 路径先 InsertInboundMessage(claim)再执行副作用,
    副作用失败删除消息行(best-effort)并返回 error 让 Poller 重试。

### P5 — 进程清理(问题 4,Important)
- ga.py 单次 /proc 快照可被持续 fork 绕过;task_terminal 吞异常仍产出成功。
  - 修复 a: ga.py 循环扫描(最多 3 轮,/proc 中非自身进程杀净),返回是否干净。
  - 修复 b: task_terminal.py 清理不干净时产出 TASK_FAILED
    error_code=SUBPROCESS_CLEANUP_FAILED 终态;Platform dispatch 对该错误码
    evict Worker(销毁 Runner),不复用。

### P6 — Compose 网络(问题 1,Critical)
- Platform 仅监听 127.0.0.1:8080;nginx 在独立容器代理 platform:8080 不可达;
  webhook 指向 platform:8088 不存在。
  - 修复: Platform 增加 unix socket 监听(共享卷 platform_sock),
    nginx proxy_pass 到 unix socket;webhook 改 http://web:8088;权限
    socket 0660 group 10001,web 容器 group_add 10001;healthcheck 不变。

## Non-goals
- 不引入数据库 schema 迁移(除非绝对必要;当前修复全部可在现有表上完成)。
- 不改动多实例部署拓扑(仍是单实例 compose);仅保证单实例重启正确。
- 不实现 cgroup.kill(容器内非 root 无法挂载 cgroupfs),采用循环扫描+fail-closed。

## Validation
- Go 单测 + `TEST_DATABASE_URL=postgres://admin:REDACTED@127.0.0.1:5432/genericagent_test?sslmode=disable`
  (该库可被测试 DROP SCHEMA 重建;genericagent 库不动)。
- worker-python pytest。
- 进程清理竞态:复用 ga-runner:local 镜像复现脚本,修复后 SURVIVED=0。
- Compose: `docker compose config` 语法 + 实际 `up -d` 冒烟(nginx→platform socket、
  webhook 地址可达)。
- 全量回归: go test ./... + python pytest(安全/契约/worker)。
