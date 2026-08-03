# 审查发现修复第二轮 Execution Spec

**Goal:** 修复 `tenant_platform/docs/GA_SANDBOX_RUNNER_REFACTOR.zh-CN.md` 重构审查报告的 12 项问题（6 Critical / 5 Important / 1 Minor），使 Linux 生产构建、Runner 复用、容量排队、共享卷权限、重启恢复、checkpoint 绑定、终态撤销和六服务 Compose 验证门闭合。

**Decisions（来自审查报告，2026-08-02）:**
- R1 容量语义：任务并发（MAX_RUNNING_TASKS）与 Runner 容量（GA_RUNNER_MAX_ACTIVE）为两个独立不变量；已有健康 lease 的 runner_key 不消耗新容量；capacity/foreign-owner 结果必须把 task 保持/退回 queued，绝不终态化。
- R2 复用 Runner 的 credential 刷新必须写入 Runner 可见的 workspace `config/`（经 Manager 控制面或共享卷），且 credential generation 与 runner generation 分离。
- R3 Runner 容器必须以 `--group-add <ShareGID>` 启动；Platform 写附件/committed checkpoint 时确保目录初始化与共享组权限闭环。
- R4 所有 lease attach/renew/release/destroy 携带 owner+generation 条件；接管事务保留旧 container_id 供定向清理；Platform 启动时执行 lease/container reconcile。
- R5 checkpoint 全链路（Prepare/BeginCheckpoint/Commit/CompleteSucceeded）绑定 runner generation；bundle 校验 task_id/session_key；staging 限长流式读取；rename 后 fsync 父目录。
- R6 所有终态（成功/失败/取消/超时）立即持久撤销当前 credential 集；Sophub validator 要求 task_id/generation。
- R7 llm-proxy 注入 LLM_PROVIDER_KEY；监听地址与 loopback 校验一致；增加受控 egress 路径。
- R8 reset-dev.sh 卷名与 compose 显式卷名一致；ResetSchema foundation 列表补齐新表与 marker。
- R9 post-create inspect 必须精确校验 server-side 推导的 source（volume-subpath/bind canonical）、完整资源限制（Memory/CPU/Pids/Tmpfs）、security profile 与精确 user/group，而非仅比对尾缀。

**Constraints:**
- 审查只读约束已解除；工作树/索引/HEAD 不得回退（当前工作树有大量未提交修改）。
- 保持 D1-D17 已确认决策（复用 Runner、满载 queued、共享卷 setgid、safefs 路径隔离、generation fencing）。
- 后端测试 60s 超时；`go test ./...` 与 `-race`；Python pytest；`docker compose config`。
- 不引入新依赖、不做 schema 大改（0037 已有 generation 列）；proto 改动需同步生成 `gen/worker/v1` 与 `worker_pb2.py`。

**Non-goals:**
- 不重做已闭合的审查 9/10/11 项；不实现每工作区硬配额；不实现高安全部署形态。
- 不做数据库测试（无 TEST_DATABASE_URL）；真实 Docker/runsc/mTLS 端到端冒烟需 Linux 主机，不在本机执行。

**Architecture:** 逐项按审查定位修复：构建门（safefs `:=`/filepath import）→ 权限闭环（group-add + 初始化前置）→ 调度不变量（claim/lease 原子化 + queued 保持）→ 凭证卷（workspace config 写入 + generation 分离）→ checkpoint 绑定（generation 贯穿 + 限长 + fsync）→ 终态撤销 → 重启 reconcile → llm-proxy compose → reset/schema → manager env 接线。

**Final validation:** Linux cross-build 通过；非 DB Go 测试 + race 全绿；Python 契约/安全通过；compose config 可解析；每项有针对性回归测试。
