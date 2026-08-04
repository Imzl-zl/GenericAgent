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
