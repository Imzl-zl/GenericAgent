# 审查发现修复第三轮 Execution Spec

**Goal:** 修复 `GA_SANDBOX_RUNNER_REFACTOR.zh-CN.md` 独立审查报告的 16 项问题（1 Critical / 15 Important），覆盖调度一致性、lease fencing、安全隔离、GA 状态语义与部署契约，且修复后不引入新问题。

**Decisions（来自审查报告，2026-08-03）:**
- R1 checkpoint 失败路径在持有 lifecycleMu 时调用 evictWorkerAfterFailure 会自死锁；改为提供 Locked 变体，锁内直接调用。
- R2 checkpoint 提交与 heartbeat 必须校验 task claim 未过期；Runner lease 校验必须含 owner/expiry 且 FOR UPDATE。
- R3 工作区目录 chown/chmod 必须覆盖中间目录（temp/session_files 等祖先链）；目录操作前拒绝/替换符号链接。
- R4 idle/超时/租约丢失/cancel-after-dispatch 路径必须 fence Worker（evict），否则旧任务与下任务重叠。
- R5 dispatch 的撤销集合必须在 ensureWorker 成功后立即捕获（覆盖更早终态分支）；撤销失败进入 pendingRevocations 重试。
- R6 lease 获取后初始化失败必须释放；活跃 lease 的 owner 无活跃 task claim 时允许新实例接管（重启恢复）。
- R7 overlay 复用必须校验内容 digest（含镜像指纹），不匹配删除重建；LEGACY_PLUGINS 补 project_mode.py。
- R8 附件/输出布局统一到工作区 temp/attachments、temp/outputs（移除 session_files/<digest> 中间层与 cwd 重定向），恢复 GA 原生相对路径语义；项目激活态入 checkpoint working。
- R9 reload_credentials 必须刷新 session.sophub_proxy；compose 提供 GA_SOPHUB_PROXY_ADDR 默认值。
- R10 控制 RPC（Reload/Cancel/Shutdown）携带 workspace_key+runner_generation 并在 Worker 侧校验；capability 补 operation/budget claims。
- R11 容器身份用不可变 ContainerID attach lease；sandbox-manager sweep 不杀运行中容器（活跃 Runner 由 lease 生命周期管理）。
- R12 inspect 精确校验 GroupAdd 等长等值 + tmpfs 安全选项；测试浅拷贝 bug 修复。
- R13 移除 attachments 冗余挂载（附件已统一 temp），config 保留（12 轮 R2 已确认）。
- R14 ReadResult 限长读取；safefs openDirBeneath 修复 rootFD 泄漏。
- R15 Sophub 搜索过滤 approved/single/markdown；未绑定时 GET status 返回 200 configured:false；OpenAPI 删除孤儿模型。

**Constraints:**
- 工作树/索引/HEAD 不得回退（有大量未提交修改）；只读约束已解除。
- 保持 D1-D17 与第 12 轮 R1-R9 已确认决策。
- 后端测试 60s 超时；`go test ./...` 与 `-race`；Python pytest；`docker compose config`；proto 改动需同步生成 Go 与 Python。
- 不引入新依赖（protoc 已有，无新库）。

**Non-goals:**
- 不重做 12 轮已闭合项；不做 DB 测试（无 TEST_DATABASE_URL）；真实 Docker/runsc/mTLS 端到端冒烟需 Linux 主机。

**Architecture:** 按审查报告 16 项逐项修复：调度死锁与撤销时机（scheduler_*）→ 存储层 fencing（checkpoint_store/task_lifecycle/runner_lease_store）→ 安全（workspace.go/safefs/checkpoint workspace）→ Python 运行时（overlay/session_files/项目模式/SOPHub reload）→ 控制面契约（proto+token）→ 部署（docker_cli/inspect/sandbox-manager/compose/OpenAPI）。

**Final validation:** Go 非 DB 测试 + race + vet + GOOS=linux build 全绿；Python 全量 pytest 绿；compose config 可解析；每项有针对性回归测试或等价验证。
