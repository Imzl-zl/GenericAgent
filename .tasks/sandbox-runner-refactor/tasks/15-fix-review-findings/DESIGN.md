# Task 15: 审查发现修复第五轮（2C/8I/2M）

- version: zhanggui/v0.4
- goal: 修复 Round 5 审查报告的 2 Critical + 8 Important + 2 Minor，且不引入新问题
- intent: clear-change
- phase: design
- decision_mode: Mixed（D1 安全边界归 user，其余 model）
- task_root: .tasks/sandbox-runner-refactor（epic 第 15 子任务，ownership 已确认）

## 设计真值

### D1（confirmed 2026-08-04, owner: user → 选 A）Critical-1: Compose 部署 Platform 无法启动 / Runner 无法访问 Sophub proxy

事实：
- `platform.Dockerfile:55` CMD 传 `--listen=0.0.0.0:8080`；`api/middleware.go:73` ServeContext 拒绝非 loopback → 默认 Compose 直接启动失败。
- 即使改回 loopback，Runner 在独立 `runner-control` 网络命名空间，无法访问 Platform loopback 上的 `http://platform:8080`（compose.yaml:133）。
- llm-proxy 在 runner-control 网络且监听 0.0.0.0:8081，Runner 可正常访问——仅 Sophub proxy 端点不可达。
- web 容器 `network_mode: service:platform` 共享 netns，经 nginx(8088)→platform loopback(8080) 自洽，不涉及本问题。

候选方案：
- A（推荐）：双 listener。Platform 新增 `--worker-internal-listen`（默认关闭），内部 listener 只注册 `/v1/worker/sophub/*`（capability 鉴权 + 与主服务相同的 security middleware），不注册任何管理/用户 API；compose.yaml 显式启用并让 Runner 指向 `http://platform:8082`。安全面最小，职责不变。
- B：放宽 ServeContext guard 允许非 loopback。改动最小但把完整 API（含管理端）暴露到 application 网络；与既有"platform API must bind loopback"刻意 guard 冲突，不推荐。
- C：迁移 Sophub proxy 到 llm-proxy 容器。llm-proxy 已有 capability 校验与 runner-control 网络，但需迁移 DB store + handler + 加密 key，偏离"Platform 保留 Sophub proxy"职责（文档 §5.2），工程量大，不推荐。

用户确认选 A：双 listener。

### Critical-2: CompleteFailedTerminal 缺 claim owner/lease fencing

- 现状：`checkpoint_store.go:357` 只按 taskID 更新并清空 claim；`scheduler_dispatch.go:28/263/390` 调用（全部在 dispatch/调度器上下文）。
- 修复：签名增加 `owner string`（platformInstanceID）；SQL 加 `AND claim_owner=$owner AND claim_lease_until > timezone('utc', now()) AND status IN ('starting','running')`；RowsAffected==0 返回 sentinel `ErrTaskNotOwned`；`finalizeOrFail` 捕获后记 Warn（任务已由 RecoverAfterRestart/新 owner 接管，不得再写终态）。
- 管理型路径（CancelTask task_lifecycle.go:260、RemoveMember team_member_store.go）为独立 SQL 事务，不受影响。
- 文件：`internal/infrastructure/postgres/checkpoint_store.go`、`internal/application/task_store.go`（接口）、`scheduler_dispatch.go`；测试：postgres store_test（fencing 拒绝 + owner 匹配）、scheduler_test（双实例 takeover 后旧实例不得终态化）。

### Important-3: pendingRefresh 跨任务沿用旧 task 凭据

- 现状：`scheduler_worker.go:115` ensureWorker 先切 `entry.taskID` 再 prepare；`worker_credential.go:197` refreshWorkerCredentials 对已有 pendingRefresh 直接 ack 提升（token 绑定旧 task A），随后 generation 变化使 ensureWorker:128 跳过 B 的新签发 → B 使用已撤销的 A token。
- 修复：`pendingCredentialRefresh` 增加 `taskID` 字段（签发时记录）；`refreshWorkerCredentials` 开头若 `pending.taskID != entry.taskID`：撤销 `pending.Next`（best-effort）、清空 pending，按新任务重新签发（generation = credentials.Generation+1，绑定 entry.taskID）。Worker 已应用旧 Next 的场景由随后的新 ReloadCredentials(N) 整体替换覆盖。
- 文件：`scheduler_worker.go`（pending 结构 + refreshWorkerCredentials）、`worker_credential.go`（签发处记录 taskID）；测试：worker_credential_test 补"跨任务边界 pending 丢弃重签"。

### Important-4: RemoveMember 留下永久 starting 任务

- 现状：`team_member_store.go:151` 对 starting/running 只写 cancel_requested_at；`scheduler_dispatch.go:110` 对 starting+未派发直接 return nil（依赖 store 已终态化）。
- 修复：RemoveMember 事务中新增：`UPDATE tasks SET status='cancelled', claim 清空, terminal_*='TASK_CANCELLED'/'member removed from team', terminal_at=now() WHERE requester_user_id=$1 AND session_key=$2 AND status='starting' AND worker_dispatch_started_at IS NULL AND cancel_requested_at IS NULL`（先于 cancel_requested 更新）。已派发任务维持 durable cancel。
- 文件：`team_member_store.go`；测试：store_test（移除成员后 starting 未派发任务终态化）。

### Important-5: 交付文件缺聚合上限 + 静默截断窗口

- 现状：`delivery_capture.go:24` 每 marker 单独 8 MiB（defaultMaxDeliverableBytes），无 marker 数/总字节上限；`safefs_unix.go:86` fstat 后 LimitReader 读前 maxBytes，文件增长时静默截断。
- 修复：
  - `delivery_capture.go`：常量 `maxDeliverableFiles = 32`、`maxTotalDeliverableBytes = 64<<20`；累计计数与字节，超限返回明确错误（任务失败并提示，不做静默截断）。
  - `safefs_unix.go` ReadFileBeneathLimited：`io.LimitReader(f, maxBytes+1)`，读满 maxBytes+1 报 `ErrFileTooLarge`；读后再次 `f.Stat()` 比较 size（增长则报错，避免 TOCTOU 截断）。
- 文件：`delivery_capture.go`、`safefs_unix.go`；测试：delivery_capture_test（超文件数/超总字节）、safefs 测试（maxBytes+1 边界、读后 size 变化拒绝）。

### Important-6: Runner 容器身份 fencing 不完整

- 现状：`manager.go:282` DestroyRunner 的容器 ID 分支只查 `IsRunnerContainer`（runner=true label），不验证 manager label；`runner_lease_store.go:216` AttachRunnerContainer 允许同 generation 覆盖已有 container_id。
- 修复：
  - DestroyRunner ID 分支改走 `IsManagerRunner`（runner=true + manager label == 本实例），不匹配且存在则拒绝销毁（与 name 分支一致）。
  - AttachRunnerContainer：`WHERE runner_key=$1 AND generation=$2 AND owner=$3 AND expires_at > now()` 且 `(container_id IS NULL OR container_id=$4)`；RowsAffected==0 报错。
- 文件：`manager.go`、`runner_lease_store.go`；测试：manager_test（ID 路径跨 manager 拒绝）、runner_lease_store_test（非 owner/过期/非空 container_id 覆盖拒绝）。

### Important-7: 容器创建失败清理 + 复用要求 running

- 现状：`docker_cli.go:271-278` create 成功、start 失败返回空 Runner 不 rm；EnsureRunner 复用分支（manager.go:137/154）Inspect 通过即复用（Inspect 不检查 State.Running）；`cmd/sandbox-manager/main.go:175` sweepOrphans 直接 CLI.Destroy 且错误只记日志不聚合。
- 修复：
  - CreateAndStart：start 失败路径立即 `rm -f <containerID>`（best-effort，与 create 错误合并返回）。
  - Inspect（inspect.go:80）：JSON 增加 `State.Running` 校验（非 running 报错 → EnsureRunner 复用分支 fail-closed 销毁重建）。
  - sweepOrphans：错误聚合返回（调用方记录单条汇总日志）；销毁前从 label 恢复 workspace hash，调用 Manager 级 config 清理（新增公开方法 `CleanupWorkspaceConfig(hash)`，sweep 经 Manager 实例执行）。
- 文件：`docker_cli.go`、`inspect.go`、`manager.go`、`cmd/sandbox-manager/main.go`；测试：docker_cli_test（start 失败后 rm 调用）、inspect 测试（stopped 拒绝）、manager_test。

### Important-8: Worker 控制 RPC 无 task capability 校验

- 现状：ExecuteTask 已校验 capability_jti ∈ session.capability_jtis（task_runner.py:93-98）；BeginCheckpoint/CancelTask/Shutdown 只校验 workspace/generation。文档 §7（line 197）要求"task capability 任一不匹配均拒绝 StartSession、ExecuteTask、CancelTask、Checkpoint 与 Shutdown"。
- 修复（工程最小落地）：proto 为 `BeginCheckpointRequest`、`CancelTaskRequest`、`ShutdownRequest` 增加 `capability_jti`；Go workerclient 调用时传 entry 当前 JTI（entry.credentials.JTIs 首项，与 ExecuteTask 一致）；Python `_assert_task_capability(jti)`：非空且 ∈ session.capability_jtis，否则拒绝（FAILED_PRECONDITION）。StartSession/ReloadCredentials 保持 mTLS+generation（capability 在此之后签发，无法前置校验；由 ExecuteTask 首道校验覆盖）。
- 文件：`contracts/proto/.../worker.proto` + 重新生成 pb.go、`internal/infrastructure/workerclient/client.go`、`internal/infrastructure/worker/sandbox_runtime.go`（若有直连）、`worker-python/src/ga_worker/managed_agent.py`、`rpc_server.py`；测试：Python 单测（控制 RPC 带/不带 JTI）、Go workerclient 单测。
- 注意：proto 重新生成需 `protoc v35.1` + `protoc-gen-go v1.34.2` + `protoc-gen-go-grpc v1.5.1`（本机已具备）；生成后 `go build ./...` 全量确认。

### Important-9: delivery 成员检查 TOCTOU

- 现状：`delivery_service.go:259` process 开头查一次成员，`sendAndJournalPart` 的 send closure（:297/:329）发送前不再检查。
- 修复：send closure 内（SendMessage/SendFile 前）再次 `IsApprovedTeamMember`（team 任务），失败返回 MEMBER_REMOVED 错误 → 走既有 handleDeliveryPartError 死信路径。成员移除发生在两次检查之间的小窗口仍存在（外部 I/O 无法原子），但窗口从"检查→发送全过程"缩到发送前一刻；与 I4 的移除时取消配合。
- 文件：`delivery_service.go`；测试：delivery_service_test（发送前成员被移除 → MEMBER_REMOVED，不发送）。

### Important-10: 交付文件名暴露 marker hash 前缀

- 现状：`delivery_service.go:511` 快照临时文件 `<marker-hash>_<name>`；`wxbot_client.py:240` 用 `fp.name` 作为显示文件名。
- 修复：`BotTransportAdapter.SendFile` 增加 `fileName string` 参数；ILinkAdapter/LoopbackTransport 上传时显式使用 fileName；delivery_service 传 `file.displayName`；wxbot 侧（若独立实现）同样显式命名。全链路测试 mock 同步更新。
- 文件：`internal/infrastructure/transport/adapter.go`、`ilink.go`、`loopback.go`、`delivery_service.go`、`frontends/wxbot_client.py`（若参与上传）；测试：delivery_service_test、transport 单测。

### Minor-11: workspace team 整数分支接受负数

- 现状：`workspace.go:45` team 分支 `idText == "0"` 只拒绝 0，接受 `team:-1`。
- 修复：改为 ParseInt 后 `id <= 0` 拒绝（与 ParseWorkspaceKey 一致）。
- 文件：`domain/workspace.go`；测试：workspace_test 补负整数/0/正整数/uuid 用例。

### Minor-12: task_started 重试乱序

- 现状：task_started delivery 发送失败重试期间，终态 delivery 可先发出；恢复后用户先见完成再见"正在处理"。
- 修复：终态事务（`task_helpers.go:109` insertDelivery 调用前或 CompleteFailedTerminal/CompleteSucceeded 事务内）取消该任务未发送的 task_started：`UPDATE task_deliveries SET status='cancelled' WHERE task_id=$1 AND delivery_type='task_started' AND status='queued'`。sending 中的不可安全取消，维持现状。
- 文件：`task_helpers.go` / `checkpoint_store.go`；测试：postgres store_test（终态后 task_started queued 变 cancelled）。

## 实施批次（TDD，先测试后实现）

- B1: Minor-11 + Critical-2（fencing + sentinel）——调度状态机基础
- B2: Important-3（pending 跨任务）+ Important-4（RemoveMember）+ Minor-12（delivery 顺序）
- B3: Important-5（聚合限额）+ Important-9（TOCTOU）+ Important-10（文件名）
- B4: Important-6 + Important-7（Manager/CLI 生命周期）
- B5: Important-8（proto + Python + Go 控制 RPC capability）
- B6: Critical-1（按 D1 决策）+ compose 冒烟
- 每批：窄测试 → 实现 → 窄测试；全部完成后全量 Go（-p 1 + TEST_DATABASE_URL）+ Python + 契约/安全 + compose config + git diff --check

## 约束

- 只读审查结束，本任务进入写模式；工作树保留既有未提交改动（第 14 轮成果），提交由用户决定。
- 不引入新依赖；不改迁移 0044 之前 schema（如需新增列优先复用现有列；Important-8 只改 proto/代码）。
