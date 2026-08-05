# Progress

- Shape: epic
- FinalizationStatus: active
- Truth: .tasks/sandbox-runner-refactor/SUBTASKS.csv
- Parent: (root epic)
- Current: 审查发现修复第二轮 12/12 DONE + 独立审查 7 findings 全部修复(占位符/列数/loopback 兼容/volume inspect/刷新权限/symlink 测试/非法 env 拒绝/renew 过期拒绝); 待提交
- Latest validation: Linux cross-build ./... OK; go vet ./... OK; Go 非 DB 测试全绿 + race; worker-python 109 passed; 契约+安全 16 passed; compose config OK; Docker 29.6.2 实证 volume-subpath inspect 格式; 数据库测试仍需 TEST_DATABASE_URL
- 残余风险(方案 §10 需真实 Linux 主机): runsc 运行时验证、mTLS 证书注入容器的端到端测试、真实 Sophub 端到端、六服务 compose 冒烟、共享卷跨 UID 真实读写需部署主机执行
- Next: 提交审查修复(建议独立 commit); 残余验证: TEST_DATABASE_URL 下的 DB 套件 + 真实 Docker/runsc 主机冒烟
# Round 5 fixes (2026-08-03, 审查发现修复批次)
- Truth: 本文件追加段 + SUBTASKS.csv 保持 active
- 目标: 修复审查 R5 的 10 项(C1/C2/C3 部署阻断, I1-I6, Minor), 全部先测试后实现
- 批次: R10 workspace-key 校验统一 → R1 inspect HostConfig.Mounts → R2 compose caps → R3/C6 fail-closed+config 回收 → R4 requeue 竞态 → R5 JTI → R6 outbox 文件绑定(0044 迁移) → R7 成员移除 → R8 checkpoint.py → 全量验证
- 测试库: genericagent_test (TEST_DATABASE_URL=postgres://admin:CHANGE_ME_BEFORE_RUN.@127.0.0.1:5432/genericagent_test?sslmode=disable)
# Round 5 fixes 完成(2026-08-03)
- R10 workspace-key 校验统一: domain.ValidateWorkspaceKey(team:<uuid>|personal:<int>), sandbox.WorkspaceDirHash 改签名共用校验
- R1(C1) inspect 改解析 docker inspect 完整 JSON(模板语法对 map 字段失效是另一实证缺陷), subpath 从 HostConfig.Mounts 按 Target 关联; 新增 3 个真实 Docker 集成测试(create/start/inspect 全链路、subpath 漂移拒绝、卷内子目录预建契约)
- R2(C2) compose cap_add +FOWNER/FSETID(实测 02770)
- R3(C3) stale destroy fail-closed(runtime 接管 + Manager 扫描路径), 不再并发创建
- C6 config 回收强化: CreateAndStart 失败清理、post-create inspect 失败走 DestroyRunner、按容器 ID 销毁从 label 恢复 hash(Destroy 前读取)、inspect 端点返回 workspace_hash
- R4(I1) dispatchHeartbeat.requeued 标记: ticker ErrLeaseExpired/重试耗尽静默退出, Stop fallback 跳过终态化; 单元+集成测试
- R5(I2) ensureWorker 先切 taskID 再 prepare(刷新绑定新任务); SetTaskCapabilityJTIs 加活跃 claim 条件+RowsAffected 检查(签名加 platformInstanceID)
- R6(I3) 0044_task_delivery_files 迁移: 成功事务内绑定输出文件快照; captureTaskDeliverableFiles(safefs 限长); delivery 从 DB 快照发送(子目录+marker 哈希隔离, 保留用户可见名); 30 天保留期定期清理; scheduler cfg 加 SessionFiles
- R7(I4) RemoveMember: starting/running 写 durable cancel_requested_at, active_contexts 清理限定当前团队; delivery 前 IsApprovedTeamMember 检查(MEMBER_REMOVED dead-letter)
- R8(I5) checkpoint.py 单 fd(O_NOFOLLOW+fstat+限读), 消除 stat/read TOCTOU; 3 新测试(超大/增长/symlink)
- 文档同步: config/ 为生命周期 subpath + 回收协议
- 自审修复: DestroyRunner label 读取移到 Destroy 之前(顺序 bug); buildPayload 同 basename 文件用 marker 哈希隔离
- 验证: go test -p 1 -count=1 ./... 全绿(含 TEST_DATABASE_URL 独立库 genericagent_test + 真实 Docker 集成); race 4 关键包全绿; python 132 passed; compose config OK; GOOS=linux build OK; git diff --check OK
- 残余: 两包并行共享测试库的既有 flaky(go test -p 1 规避); runsc/mTLS 端到端仍需真实 Linux 主机(方案 §10 声明)
# Round 6 fixes (2026-08-04, 审查 R5 第五轮修复批次)
- Truth: tasks/15-fix-review-findings/DESIGN.md + SUBTASKS.csv id=15
- 目标: 修复审查 R5 的 2C/8I/2M, 全部先测试后实现(TDD 红绿闭环)
- 批次: B1 workspace 负整数 + CompleteFailedTerminal fencing → B2 pendingRefresh 跨任务 + RemoveMember 未派发终态化 + task_started 取消 → B3 聚合限额 + 发送前成员复查 + 显式文件名全链路 → B4 Inspect Running + DestroyRunner ID manager label + AttachRunnerContainer owner/lease/immutable + CreateAndStart rm + sweep 聚合 → B5 控制 RPC capability(proto+Go+Python) → B6 双 listener(用户 D1 确认方案 A)
- 用户决策 D1(2026-08-04): Compose 网络选双 listener(内部 8082 只挂 capability-protected Sophub 路由)
# Round 6 fixes 完成(2026-08-04)
- B1: workspace.go team 整数分支 id<=0; CompleteFailedTerminal 加 owner 参数 + claim_owner/lease/status fencing(ErrTaskNotOwned), finalizeOrFail Warn 不覆盖新 owner
- B2: pendingCredentialRefresh.TaskID 跨任务丢弃重签(撤销旧 Next); RemoveMember 未派发 starting 直接终态化(修正 cancel_requested 先命中导致的新 SQL 落空——原代码 bug 链); 0045 迁移 status 加 cancelled + finalizeTerminal 取消 pending task_started
- B3: maxDeliverableFiles=32/maxTotalDeliverableBytes=64MiB 聚合上限; safefs maxBytes+1 读取检测(双平台实现)+ErrFileTooLarge; send closure 内 IsApprovedTeamMember 复查(MEMBER_REMOVED 直接死信); BotTransportAdapter.SendFile 加 fileName 全链路(Go→poller→wxbot_client 显式 file_name)
- B4: Inspect 校验 State.Running(ErrRunnerNotRunning), EnsureRunner 复用分支销毁重建停止容器; DestroyRunner ID 路径改 IsManagerRunner; AttachRunnerContainer 加 owner+expires+container_id 不可变条件(发现 container_id 默认 '' 非 NULL); CreateAndStart start 失败 rm -f; sweepOrphans 经 Manager.DestroyRunner 聚合错误(config 清理不绕过)
- B5: worker.proto BeginCheckpoint/CancelTask/Shutdown 加 capability_jti(protoc 重新生成 Go+Python); workerclient 传 firstJTI(entry.credentials); Python _assert_task_capability(有活跃集时拒绝空/过期 JTI)
- B6: ServeInternalContext + NewWorkerSophubHandler(只挂 /v1/worker/sophub/*); --worker-internal-listen flag 默认关闭; platform.Dockerfile CMD 改 127.0.0.1:8080 + 0.0.0.0:8082; compose/.env.example GA_SOPHUB_PROXY_ADDR=http://platform:8082
- 验证: Go 全量 -p 1 + TEST_DATABASE_URL 全绿; race 4 关键包全绿; worker-python 125 passed + bot_poller 13; 契约安全 13; compose config OK; GOOS=linux build OK; go vet OK; git diff --check OK
- 残余: runsc/mTLS 端到端仍需真实 Linux 主机(方案 §10 声明); 内部 listener 真实网络连通性需 compose 冒烟(本机无 platform 镜像)
# Round 7 fixes (2026-08-04, 独立审查 8 findings 修复批次)
- Truth: tasks/16-fix-review-round7/DESIGN.md + SUBTASKS.csv id=16
- 目标: 修复独立审查报告的 3C/5I, 全部先测试后实现(TDD 红绿闭环)
- 批次: C1 交付快照文件名穿越 → C2 code_run 进程组 → C3 state committed/results ro 遮蔽 → I4 JTI 追加去重 → I5 RemoveMember 统一终态 → I6 config generation 隔离+锁 → I7 timeout/Shutdown JTI → I8 推进点心跳 → 连锁修复(RuntimeConfigDir generation 化) → 文档同步
- 验证: Go 全量 18 包 + TEST_DATABASE_URL 全绿; race 4 关键包全绿; worker-python 106 passed; bot_poller 13; 契约+安全 35; compose config OK; GOOS=linux build OK; go vet OK; git diff --check OK; dbx 实测 PG16 追加去重 SQL
- 残余(仓库已声明): runsc/mTLS 端到端、真实六服务 compose 冒烟、真实 Sophub 需部署主机
# Round 7 reviewer-fix pass (2026-08-04, 独立审查后修复)
- 独立 reviewer(zhanggui-requesting-code-review)发现 3 Important + 4 Minor, 全部修复并有测试:
  - I8 接线 bug: last_progress_at 只在 cancel/timeout 分支更新 → 移到 drain 主循环(item 到达即刷新), 新增 drain 级集成测试
  - C2 Windows 回归: _kill_process_group 无 killpg 时回退 process.kill(不再是 no-op); 注册表改存 Popen 对象 + poll() 跳过已退出(防 PID 复用)
  - C2 emit_exception_terminal 补 cleanup_legacy_subprocesses(6/6 emit 全覆盖)+ 测试
  - C1 收紧: validateManifestEntry Clean 后拒绝中段 ..(outputs/../../x); displayName 剥离控制字符
  - I6 锁 get-or-create(workspaceLock helper), 消除 DestroyRunner 与 EnsureRunner 的锁创建竞态
- 验证: Go 全量 18 包 + race 4 关键包 + vet + linux build 全绿; worker-python 110 passed; bot_poller 13; 契约安全 35; compose config OK; git diff --check OK
- 残余(仓库已声明): runsc/mTLS 端到端、六服务 compose 冒烟、真实 Sophub 需部署主机; sandbox_runtime 续租失败路径 Shutdown 仍传空 JTI(best-effort, Manager.Destroy 兜底 fencing)

## Round10 修复(2026-08-04, e242c52)

9 项真实问题全部修复并验证:
- B1( Critical): idle 回收后 lease 残留容器 ID → ReleaseRunnerLease 清空容器字段 + Manager 对不存在受控容器幂等销毁。
- B2( Important): 稳定去重 ID(GA_PLATFORM_INSTANCE_ID)与每进程唯一 processID(claim/lease/checkpoint owner)拆分——重启后新进程接管旧 lease、递增 generation、销毁旧 CA 容器。
- B4( Important): ga.py 循环 /proc 扫描(3 轮); 成功任务清理不干净时 fail-closed(SUBPROCESS_CLEANUP_FAILED → Platform 销毁 Runner)。
- B5( Important): BlockUser 终态化未派发 starting(撤销 JTI/取消 task_started/清 claim); IsTaskCapabilityActive 增加用户状态校验。
- B6( Important): 交付快照 0o2770 目录 + 0o640 文件(setgid 继承共享组 10003)——容器复现 Poller 读取 OK。
- B7( Important): 任务+入站消息行同事务(SubmitTaskWithInboundMessage); 命令/relay 先 claim 后副作用(失败删行)。
- B8( Minor): delivery claim 排除同 task 未完成 task_started, 完成消息不再先于"正在处理"。
- B9a( Minor): checkpoint Commit 失败清理已物化 committed/result 文件。
- B1c( Critical): Compose 主 API 改 unix socket(nginx 经 platform_sock 卷代理 /v1/ 与 /healthz), webhook 指向 web:8088, API 面不暴露 runner-control。
# Round 11 fixes (2026-08-05, 独立审查第四轮 3C/7I 修复批次)
- Truth: SUBTASKS.csv id=20 (tasks/20-fix-round11)
- 目标: 修复审查确认的 3 Critical + 7 Important + 3 Minor, 全部先测试后实现
- C1 Compose socket 接线: platform 挂 platform_sock 卷 + web group_add 10001(compose config 渲染验证)
- C2 checkpoint 不确定删除窗口: classifyCommitError 区分确定回滚/ErrCommitOutcomeUnknown; 不确定时保留文件 + ReconcileOrphanCommittedFiles(DB 引用对账, 1h 孤儿年龄) 5 分钟节流接线; 4 分支单测 + 4 集成测试
- C3 全局 workerCallMu → per-entry startMu(StartSession/CancelTask 同 session 串行, 跨 session 并行) + executing 原子标记(未执行不发取消 RPC) + StartSessionTimeout 独立超时(默认 90s) + CancelTask RPC 30s 超时
- I1 附件先写后授权: ImportInbound 失败/提交失败回滚 RemoveInbound(manifest 同步清理, 幂等); router 提交失败自动回滚
- I2 CancelTask 失败重试: cancelCall 改 inflight/done/notify 状态机, 瞬时失败不缓存, 并发合并单 RPC
- I3 sweep 双路径: DestroyRunnerIf 锁内 re-inspect(created→running 竞态) + absTTL 判定基于最新状态; assertManagedRunner/runnerLabels 提取复用
- I4 control capability: IssueControlToken(aud=ga-worker-control, op=worker.control) 独立签发; ControlJTI 带 ctrl: 前缀(Worker 只持有 JTI 值)进同一撤销集合; 控制 RPC 全部改用 controlJTIFor; Worker 校验前缀+集合成员(LLM JTI 拒绝); 集成测试修复(真实 Go JWT aud 数组→前缀方案)
- I5 DOCX: install_export_docx_tool 真实实现(python-docx, 镜像已装), outputs/ 沙箱+逃逸拒绝+source_path/<file_content> 支持; 注入 TOOLS_SCHEMA 经 policy 过滤; 原"不注入"测试反转
- I6 config 清理强制化: cleanupWorkspaceConfig 返回 error + destroy/创建失败路径组合错误 + ReconcileOrphanWorkspaceConfigs(label 对账, 1h 年龄) 周期接线
- I7 memory 模板标记: .ga-memory-init 工作区根标记(不在 Runner 挂载), 用户清空 memory 不重灌; 老布局补标记不重灌
- M1 证书 TTL: minRunnerCertTTL 24h(lease 无限续租但证书不轮换, 长会话重连失败)
- M2 compose 卷/网络名项目前缀: ${COMPOSE_PROJECT_NAME:-ga}_runner_workspaces / _runner-control, GA_WORKSPACES_VOLUME 同步
- M3 死代码: DocumentPoolSettings 类型/校验/测试删除; settings_http documentPool 死类型删除; README 更新(不再提 Document Manager)
- 验证: Go 18 包全量(-p 1, DB 套件 genericagent_test)+ race 3 关键包; worker-python 116; bot_poller 16; 契约+安全+集成+冒烟 45(integration 6 连续两轮); GOOS=linux build; compose config; git diff --check; 全部通过
- 新增测试: classifyCommitError 2 / Reconcile 4 / completeSuccess 4 / 锁并发 3 / cancel 重试 3 / DestroyRunnerIf 6 / ReconcileConfig 2 / memory marker 2 / control JTI 2+1 / export_docx 2 / RemoveInbound 3+2 router 2
- 数据库: dbx 超级用户 admin 建 ga_r11_test 测试角色(genericagent_test), TEST_DATABASE_URL 可用
- 残余(仓库已声明): runsc/mTLS 端到端、真实六服务 compose 冒烟、真实 Sophub 需部署主机; integration 进程扫描测试偶发 flaky(Windows 进程枚举时机, 与改动无关)
# 架构决策 D1: "任务即进程"重构(2026-08-05, 用户确认开新会话执行)
- 背景: 11 轮审查修复后仍反复出现阻塞问题, 根因是封装层协议面过度设计——凭证热刷新协议
  (ReloadCredentials/generation/pendingRefresh/rollback) 与常驻 Worker 生命周期为"省冷启动"而存在,
  但 worker 冷启动实测 0.24s(Windows; Linux 容器预计 1-3s), 收益趋近于零, 成本是数百行状态机 + 每轮竞态。
- 用户决策: 该重新设计就重新设计; 架构要匹配项目实际(核心=GA 原生, 封装只做多租户管理)。
- 目标架构: 任务即进程——每任务全新 Worker(容器内) + checkpoint 快照恢复; 任务终态销毁。
- 砍掉: 凭证热刷新协议(ReloadCredentials/pendingRefresh/rollback/credentialsNeedRefresh/凭证 generation/checksum 校验);
  Worker 常驻生命周期(ensureWorker 复用逻辑/idle eviction/startOnce/executing 状态机)。
- 保留: checkpoint Prepare/Commit/恢复; capability JTI 签发/撤销(含 ctrl: 前缀 control JTI);
  Runner lease + generation fencing(简化语义); 消息/任务/交付事务链; Manager 控制面; 政策/session files/交付安全。
- 凭证新语义: 每任务签发(LLM+sophub+control), TTL 覆盖任务墙钟上限(默认 45min, 上限可配), 任务终态撤销, 无刷新。
- 阶段: P1 删热刷新协议 → P2 删常驻生命周期 → P3 容器生命周期简化; 每阶段 TDD + DB 集成测试 + 全量验证。
- 前置: round11 未提交改动(36 文件)需先提交, 再开重构分支。
- 验证基线: TEST_DATABASE_URL=postgres://ga_r11_test:REDACTED@127.0.0.1:5432/genericagent_test?sslmode=disable
  Go 18 包 -p 1 + race; worker-python 116; contract+security+integration 45(integration 6 条全链路); compose config; GOOS=linux build。
# 架构决策 D1 执行完成(2026-08-05, 合并 main)
- P1(11d5510) 删凭证热刷新协议 / P2(98b2cfe) 删常驻 Worker 生命周期 / P3(2d2a741) 任务容器语义
- 独立架构审查: 任务即进程架构适配性通过(删除的 ~1000 行均为常驻 Worker 时代机制, 剩余机制一一对应真实不变量); 1 Important(map 竞争)+5 Minor 全部修复(0af8228)
- 净删除 ~1600 行(56 文件 +858/-2424); 每阶段全量验证绿
- 分支 refactor/task-per-process 已合并 main(fast-forward)并删除
- 遗留: 无; push 未执行(用户未要求)
# Round 12 fixes (2026-08-05, 审查第十二轮 7I/3M 修复批次)
- Truth: tasks/24-fix-round12/DESIGN.md + SUBTASKS.csv id=24
- 目标: 修复 Round 12 审查的 7 Important + 3 Minor, 全部先测试后实现(TDD 红绿闭环)
- I1 dispatch 单出口 teardown: createTaskWorker 成功后统一 deferred 销毁(身份校验变体 destroyTaskWorkerEntry 防误毁同 session 新任务), 覆盖 panic/policy/并发终态/startSession 错误/无 coordinator 5 类泄漏出口; panic recovery 改用独立有界 ctx(心跳 defer 先取消 ctx 的连带缺陷); 6 新测试
- I2 pendingFinalize: finalizeOrFail 写库失败注册意图, tick 每轮 drain 重试(claim 由 tick heartbeat 续租保持有效; 进程崩溃由 claim 过期 + RecoverAfterRestart 兜底); 3 新测试
- I3 可取消等待: dispatchHeartbeat.Stop 改 cancel 先行 + 5s 超时等待; sandbox_runtime 续租改 renewCtx 可取消, cleanup 先 cancel 再带超时等待, Shutdown/Destroy/Release 全部 bounded timeout; DialControl 测试注入点; 2 新测试
- I4 Manager nonce 持久化: NewManagerServerWithNonceState 每次消费先原子落盘(fsync+rename)再放行, 失败 fail-closed 503; cmd 必填 GA_MANAGER_NONCE_STATE; compose 新增 manager_state 卷; 4 新测试(跨重启重放/重启后新 nonce/启动失败/途中失败)
- I5 ImportInbound 原子回滚: 复制失败/manifest 保存失败删除已复制文件; 2 新测试
- I6 delivery spool 统一清理: removePayloadFiles 单一清理所有权, process 成功 build 后立即 defer, buildPayload 中途失败自清理, 删除逐文件 defer; 4 新测试
- I7 路由单次解析 + fail-closed: 入口唯一解析, GetActiveContext 真实错误拒绝消息(不再静默降级个人), handleNormalMessage 复用 inboundSessionKey(任务/消息行/附件同 key); 2 新测试
- M1 cancelOnce 清理: workerEntry.taskID + 销毁时 Delete; 1 新测试
- M2 staging 对账: Commit 删除失败记 Warn(不阻断提交), ReconcileOrphanStagingFiles(无 writing 引用 + 1h 孤儿年龄)接线 5 分钟对账; 3 新测试
- M3 文档矛盾: 主设计 91/86/183/275/245 行 + compose README 97 行改任务即进程语义
- 新问题同步修复: 门禁测试卷白名单新增 manager_state(设计变更的一部分)
- 验证: Go 18 包 -p 1(DB 套件)全绿 + race 5 关键包(application/sandbox/postgres/worker/checkpoint)全绿 + vet + GOOS=linux build; worker-python 109 passed/2 skipped; tests/ 全套 47 passed; bot_poller 16; compose config; git diff --check
- 新增测试: dispatch teardown 6 / pendingFinalize 3 / heartbeat Stop 1 / renewer cleanup 1 / Manager nonce 4 / ImportInbound 2 / delivery spool 4 / router 2 / cancelOnce 1 / staging 3 = 27
# Round 12 补充(2026-08-05, 独立审查发现修复)
- 独立 reviewer 审查 fd60fa3: 2 Important 已修复 + 验证:
  - Important-1 替换窗口泄漏(预先存在, I1 补充): destroyTaskWorkerEntry/Locked 身份不匹配时仍清理旧 entry——completeSuccess 终态提交与销毁之间 map 被同 session 新任务替换时, 旧容器/凭据不再泄漏(只不触碰新 entry); 测试断言增强
  - Important-2 弱测试: TestFinalizeRetryDropsIntentWhenClaimLost 改 GetTask 包装真实覆盖 ClaimOwner 分支, 断言 drain 不再调用 CompleteFailedTerminal
  - Minor: Manager nonce 状态目录单写者前提文档化(多实例共享卷 last-writer-wins)
- 验证: Go 18 包全量 + race 5 关键包 + vet + linux build + tests 47 + worker-python 109 + compose config + diff check 全绿
