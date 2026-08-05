# Round 12 审查发现修复 — DESIGN

- Task root: `.tasks/sandbox-runner-refactor`（.zhanggui-root 已确认，EPIC active）
- Subtask: SUBTASKS.csv id=24
- Truth: 本 DESIGN.md + SUBTASKS.csv id=24
- 目标: 修复 Round 12 审查的 7 Important + 3 Minor，全部先测试后实现（TDD 红绿闭环）
- 约束（用户指令）: 只修真实问题；不引入新问题（含竞态/回归）；架构性问题改设计不堆代码

## 问题与设计决策

### I1 调度器任务 Worker 销毁没有集中化（架构性 — 单出口设计）

根因: dispatch 是任务 Worker 的唯一 owner，但 teardown 分散在各分支，panic recovery、
POLICY_RESOLVE_FAILED、startSessionOnWorker 的 agentMaxTurns 错误、MarkRunning 后并发终态
return nil、completeSuccess 的 NO_COORDINATOR 分支共 5 类出口不销毁 Worker → 容器/lease
续租/workers entry 泄漏，同 generation 下一条任务会命中旧容器破坏串行不变量。

设计: dispatch 在 createTaskWorker 成功后立即注册统一的 deferred teardown，所有退出
路径（含 panic）恒销毁。新增身份校验变体 destroyTaskWorkerEntry(sessionKey, entry)：
仅当 s.workers[sessionKey] 仍指向该 entry 时删除+清理——防止旧任务收尾误毁同 session
新任务的 Worker（0af8228 同类竞争修复的延续）。destroyTaskWorker 改为其包装；
destroyTaskWorkerLocked 保留（持锁调用方）。幂等由 map 身份检查保证。

### I2 终态化写库失败任务永久卡 starting（设计 — 重试队列 + 现有恢复链兜底）

根因: finalizeOrFail 吞掉 CompleteFailedTerminal 错误；dispatch 退出后任务保持
starting + worker_dispatch_started_at，tick 续租 claim，无任何路径重试终态化。

设计: finalizeOrFail 失败（非 ErrTaskNotOwned）时把终态意图注册到 scheduler 的
pendingFinalize（sync.Map，taskID → finalizeIntent{status, deliveryType, code, message,
traceID}，message 先 boundMsg）。tick 每轮 drainPendingFinalize：GetTask → 已终态或
claim 非本实例 → 删除；否则重试 CompleteFailedTerminal → 成功/ErrTaskNotOwned → 删除；
DB 瞬时错误保留下轮。claim 由 tick heartbeat 持续续租，重试窗口内不会被恢复路径误判；
进程崩溃 → claim 过期 → RecoverAfterRestart 终态化（已有机制，每 tick 运行）。
覆盖全部 finalizeOrFail 调用点（dispatch 各路径、deadline、reaper、tick LEASE_EXPIRED）。

### I3 claim heartbeat 与 Runner lease 续租不可取消等待（设计修复）

根因: dispatchHeartbeat.Stop 先 <-done 后 cancel，卡在 DB 调用时无限等待；
sandbox_runtime cleanup 先 <-renewDone 后关 renewStop，且 RenewRunnerLease 用
context.Background()，清理内 Destroy/Release 也无超时。

设计: Stop 改为 cancel 先行 + done 带超时等待（heartbeatStopTimeout=5s，DB 调用全部
走 ctx 可取消）。sandbox_runtime 续租 goroutine 改用可取消 renewCtx；cleanup 先
renewCancel + close(renewStop)，再带超时等待 renewDone（workerShutdownTimeout），
超时记日志继续收尾（不永久阻塞）；Shutdown/Destroy/ReleaseRunnerLease 全部换
bounded timeout ctx。

### I4 Manager nonce 防重放重启丢失（设计 — 持久化一次性 nonce）

根因: seenNonces 仅进程内 map，compose 的 GA_MANAGER_SECRET 稳定不复位；Manager 在
5 分钟签名窗口内重启后旧签名请求可重放（可创建无 lease 的孤儿容器/控制面 DoS）。

设计: nonce 状态持久化到 Manager 自有状态卷。NewManagerServer 增加可选
nonceStateDir（新构造函数 NewManagerServerWithNonceState）；开启时每次 consumeNonce
原子落盘（temp+fsync+rename，JSON map），落盘失败 fail-closed 拒绝请求（503
NONCE_PERSIST_FAILED）；启动时加载并丢弃过期项；进程内实现保留给测试。cmd 增加
-nonce-state flag（GA_MANAGER_NONCE_STATE）且必填（空则启动失败——无静默回退）。
compose: sandbox-manager 挂载 manager_state 卷到 /var/lib/ga/manager-state。

### I5 ImportInbound 多附件非原子（设计 — 失败回滚已复制文件）

根因: 逐文件复制，中途失败返回 nil, err——已复制文件不进 manifest 也无引用，残留
团队工作区；saveManifest 失败同样残留。

设计: 函数内维护 imported refs 列表；任何错误路径（复制失败/saveManifest 失败）
先删除本次已复制文件（safefs.RemoveBeneath，manifest 最后才保存无需恢复）。

### I6 交付 spool 快照残留（设计 — 统一清理所有权）

根因: buildPayload 全量物化文件，但清理 defer 注册在逐文件发送循环内；文本发送失败
或前序文件失败时其余快照永久残留（≤64MiB/任务，可耗尽磁盘）。buildPayload 自身中途
失败同样残留前序文件。

设计: 新增 removePayloadFiles(payload)（删除全部 absPath + 空子目录，幂等）；
buildPayload 内部错误路径自清理；process() 在 buildPayload 成功后立即 defer 清理；
删除循环内逐文件 defer。

### I7 路由会话 key 双重解析可分叉（架构性 — 单次解析 + fail-closed）

根因: routeInboundMessage 先解析一次（GetActiveContext 任何错误静默降级个人），
handleNormalMessage 再 resolveSessionKey 一次；task 用第二个 key，message 行用第一个
key——瞬时错误/并发切换可使团队任务消息写入个人历史（审计/隐私分叉）。

设计: 路由入口唯一解析一次；GetActiveContext 错误区分“无行”（→个人，既有语义）
与真实错误（→ 拒绝消息返回 error，Poller 重试，fail-closed）；解析结果作为
inboundSessionKey 贯穿 routeBoundMessage/handleNormalMessage，任务与消息行共用。
命令处理器（/stop /new /status 等）保留各自 resolveSessionKey——它们操作的是
“当前上下文”最新语义，与消息审计归属无关。

### M1 cancelOnce 进程内无界增长（设计 — 随 Worker 销毁清理）

根因: cancelOnce sync.Map 按 taskID LoadOrStore，永不 Delete，按取消过的任务数增长。

设计: workerEntry 增加 taskID 字段（createTaskWorker 时设置）；destroyTaskWorker /
destroyTaskWorkerEntry / destroyTaskWorkerLocked 销毁时 s.cancelOnce.Delete(taskID)。
删除后同任务残余 cancel 调用走新条目：entry 已不存在 → 幂等成功，无重复 RPC。

### M2 checkpoint staging 删除失败被吞（设计 — 对账回收兜底）

根因: Commit 成功后 `_ = safefs.RemoveBeneath(staging)` 忽略错误；提交后 lease 被消费，
SweepExpiredCheckpoints 不再覆盖该 staging 文件 → 永久残留。

设计: Commit 中 staging 删除失败记 Warn（不阻断提交，保留可用性）；新增
ReconcileOrphanStagingFiles：遍历 workspaces/<hash>/state/staging/*.bundle.json，
token 无活动 writing lease 且文件超过 1h 年龄 → 删除（与 ReconcileOrphanCommittedFiles
同模式，防进行中 Prepare 竞态）；接线到 scheduler 5 分钟对账块。需要 checkpoint store
提供“staging token 是否仍被 writing lease 引用”查询。

### M3 文档矛盾（事实修正）

- docs/GA_SANDBOX_RUNNER_REFACTOR.zh-CN.md:91 “禁止退化为每 task 一个容器” →
  改为任务即进程语义（决策 D1 已确认）。
- infra/compose/README.zh-CN.md:97 “后续消息复用该 Runner” → 每任务新容器、
  终态销毁；GA_RUNNER_IDLE_TTL 实为 lease TTL/异常兜底。

## 验证基线

- TEST_DATABASE_URL=postgres://ga_r11_test:REDACTED@127.0.0.1:5432/genericagent_test?sslmode=disable
- Go 18 包 -p 1 + race 关键包（application/sandbox/postgres/worker）
- worker-python 单测 + bot_poller + 契约/安全/集成 + compose config + GOOS=linux build
- 新增测试: dispatch 泄漏 5 路径 / pendingFinalize 重试 / heartbeat Stop 阻塞 /
  renewer 阻塞清理 / Manager 双实例重放 / ImportInbound 回滚 / delivery spool 清理 /
  router fail-closed 单 key / cancelOnce 清理 / staging 对账

## Non-goals

- 第二渠道身份合并门（无第二渠道 ingress，超出本批次）
- runsc/mTLS 端到端、六服务 compose 冒烟、真实 Sophub（需真实 Linux 部署主机，EPIC 已声明）
- 已有 GA_RUNNER_IDLE_TTL 配置键名保留（语义注释修正即可）
