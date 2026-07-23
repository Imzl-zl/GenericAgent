# 多租户文档修订清单（已收敛）

**日期：** 2026-07-23  
**状态：** 架构与 PRD 已按方案 B 重写，并补充 2 vCPU / 4 GiB 部署验证门槛；本清单用于实现前自检。  
**文档：**
- PRD：`2026-07-21-multi-tenant-im-platform-design.md`
- 架构：`2026-07-21-multi-tenant-architecture.md`

---

## 1. 已确认决议

| ID | 决议 | 结论 |
|---|---|---|
| D1 | 部署隔离 | 单 Linux 主机 + rootless Podman Worker；禁止单进程执行隔离 |
| D2 | 工具能力 | Shell/Python 允许，仅在 Worker 内；默认无任意外网 |
| D3 | 准入 | 新账号默认 pending；人工批准后才可绑定和运行 |
| D4 | 数据库 | P0 PostgreSQL；不使用 SQLite 作为正式多 Worker 状态库 |
| D5 | 人设 | persona-in-task；团队按 `member.persona_id → team_persona_id → default` |
| D6 | 持久化 | 每个成功 task 采用文件与数据库分阶段 checkpoint；恢复只认最后一个 committed snapshot；崩溃 running task 不自动重放 |
| D7 | 密钥 | 真实 LLM Key 只在 LLM Proxy；bot/token/cursor 字段加密 |
| D8 | 绑定 | QR 后必须 `/activate` 配对 bot UUID 与 `from_user_id` |
| D9 | 中继 | 仅整句 `@username`；命令冲突用短 ID 状态机 |
| D10 | 团队邀请 | 邀请码和直接邀请都走双确认 |
| D11 | 团队 key_info | 共享；PRD 明示隐私边界 |
| D12 | `/stop` | 仅取消 requester 自己的 running task |
| D13 | 单消费者 | Gateway/Router 独占 Worker display stream；Worker Manager 独占 checkpoint control RPC；checkpoint 不抢读或复制流式队列 |
| D14 | 工具与审计 | 默认拒绝、按 capability 授权；原件只读、生成物分区；P0 配额与脱敏审计 |
| D15 | P0 物理部署 | 逻辑组件不等于进程；默认 `platform`、`worker-manager`、`llm-proxy` 三个应用进程加 PostgreSQL；platform 包含 Task Store/Checkpoint Coordinator 并拥有快照/task/delivery PostgreSQL 事务，worker-manager 只做 Worker 与受控 volume 文件协调；应用单元必须有健康检查与失败自动重启 |
| D16 | 容量准入 | `MAX_RUNNING_TASKS` 是管理员可调的运行并发上限；`MAX_ACTIVE_WORKERS` 仍是内部硬上限，调度以 cgroup `memory.current`、starting/task 预留和主机预算为准，并同时检查 CPU、PID、磁盘、模型并发和租户额度；指标缺失时保守回退，压力下直接淘汰已 checkpoint 的 idle Worker |
| D17 | 运维基线 | LLM Proxy 单点故障在 P0 通过进程监督、自动重启、可见错误和审计处理；日志/审计轮转、保留期、磁盘告警和 PostgreSQL 资源配置必须明确 |
| D18 | 隔离替代 | nsjail/bubblewrap 等轻量沙箱不作为 rootless Podman 的等价替代；任何替换都必须单独完成威胁模型、策略和逃逸测试评审 |
| D19 | 运行时调节 | 受保护运营操作可调高、调低或暂停 `MAX_RUNNING_TASKS`；降低不强杀运行中 task，调高唤醒 queued task，设置为 0 进入 drain/维护模式；每次修改审计并带 config version |
| D20 | 排队实现 | P0 使用 PostgreSQL `tasks` 表作为持久化事实来源；`LISTEN/NOTIFY` 只做唤醒，周期扫描兜底，不引入 RabbitMQ、Redis、Celery 等外部消息队列 |
| D21 | 实时内存与 swap | `memory.current` 用于当前准入，静态 P95 用于预留/回退，`memory.peak` 用于校准；swap 不计入容量，cgroup v1/v2 的 swap 策略显式配置并经目标主机验证 |
| D22 | 终态结果重发 | `task_complete` 及失败/取消/中断终态携带稳定 `delivery_id`、`snapshot_id`（成功时）、有上限的结果或错误 payload、digest 和 usage；`task_deliveries` 按 `(task_id, delivery_type)` 唯一，pending/未确认时重试、超过窗口进入 dead-letter，历史 `chunk` 不重放；carrier 不支持幂等时明确 at-least-once 风险 |
| D23 | 快照生命周期 | `workspace_snapshots` 元数据由 platform task store 唯一事务 owner；worker-manager/Workspace Store 只做 staging、fsync、rename、quarantine 文件动作并经幂等 RPC 协调；orphan 需满足 lease 过期和 grace，再 quarantine 后延迟删除；非 current 的 committed snapshot 在所有 delivery 终态且 `TASK_RESULT_RETENTION` 到期前不得清理 |
| D24 | idle 竞态 | Worker Manager 在 per-session 锁或等效 CAS 下互斥执行 idle→assigned 与 idle→evicting；淘汰胜出时 queued task 从最后 committed snapshot 重建 |

---

## 2. 旧硬伤收敛结果

| 旧问题 | 状态 | 落地位置 |
|---|---|---|
| 人设 `set_persona` 与 persona-in-task 混写 | 已消除 | 架构 §2/§4.2/§4.3 |
| 同 session “不接受下一条 put_task” 与排队冲突 | 已消除 | 架构 §2：允许有界排队 |
| 中继示例 `@B 你好` 与 R2 冲突 | 已消除 | PRD R2；架构 §6.2 |
| 白名单当主沙箱 | 已消除 | 架构 §1/§3；PRD S1 |
| 合规“无封号风险” | 已消除 | PRD S8；架构 §1.2 |
| 管理员批准前后不一致 | 已消除 | PRD A2/A5；架构 §2 |
| 团队 key_info 未声明 | 已消除 | PRD T7；架构 §2 |
| 成员人设无法建模 | 已消除 | PRD T6；架构 §2/§5 |
| display queue 双消费者 | 已消除 | 架构 §4.2/§8 |
| load_state 在 handler 创建前写 working | 已消除 | 架构 §4.3/§8 |
| `/stop` 无 owner | 已消除 | PRD G6；架构 §6.2 |
| `/同意` 命令冲突 | 已消除 | PRD §6/§7；架构 §6.2 |
| 邀请批准路径不一致 | 已消除 | PRD T2/T3；架构 §5.2 |
| SessionPool 不 pop / 假满 | 已替换 | 架构改用 Worker Manager 生命周期 |
| GA “改 2 行/45 行” | 已消除 | 架构 §4.3 改为 ManagedAgent 边界，不锁死行数 |
| 假定已有 `stop_loop`/`refresh_token`/`send_emoji` | 已消除 | 架构 §7.2；适配层显式补齐 |
| token 明文与共享 token 文件 | 已消除 | 架构 §5/§7.2；PRD X6/S4 |
| 进程模型与 PRD 非目标冲突 | 已消除 | 架构方案 B；PRD §1/§10 |
| 逻辑组件与部署进程未区分 | 已补充 | 架构 §3.1.1；默认 3 个应用进程 + PostgreSQL |
| 容量只按 Worker 数量估算 | 已补充 | 架构 §3.4/§8；改为内存预算、资源压力淘汰和压测派生 |
| 文件快照与数据库提交崩溃窗口未定义 | 已补充 | 架构 §8.1/§10；最后已提交 snapshot 为恢复权威 |
| history/output/log 无硬上限或轮转要求 | 已补充 | PRD Q7/Q8；架构 §4.3/§8.2 |
| 并发控制入口未定义 | 已补充 | PRD A6/Q9；架构 §3.4.1/§5.4 |
| 任务排队是否依赖外部 MQ 未定义 | 已补充 | PRD Q10/Q11；架构 §5.4 |
| task_complete 结果重发未定义 | 已补充 | PRD N1/N7、§9 P0 第 11 条；架构 §4.2/§8.1/§9，`task_deliveries` 唯一约束与恢复扫描 |
| orphan snapshot 判定与清理竞态未定义 | 已补充 | PRD N6；架构 `workspace_snapshots`、控制面 lease 与 §8.1/§10 |
| idle 淘汰二次 checkpoint 与 swap 表述含糊 | 已补充 | PRD Q5/Q6；架构 §3.4/§8.2/§8.3/§10 |
| Worker lease 与数据库信任边界未定义 | 已补充 | 架构 §3.1/§4.2/§8.1；platform task store 是唯一 PostgreSQL checkpoint owner，Worker 不接触 PostgreSQL，worker-manager 只执行受控文件动作 |
| “原子 checkpoint”与分阶段提交冲突 | 已消除 | 架构 §1.1/§2；D6；PRD N6 |

---

## 3. 实现前门槛（DoD）

- [ ] 目标主机验证 rootless Podman / user namespace / cgroup / seccomp；确认 cgroup v2 `memory.current/high/max`、`memory.swap.max` 或 v1 等效 memsw 策略和 swap 行为符合部署策略
- [ ] Worker 固定镜像与工具 policy 草案完成
- [ ] PostgreSQL schema / 迁移 / 密钥轮换草案完成
- [ ] BotTransportAdapter 对接现有 iLink 客户端的 stop、token 更新、cursor、绑定身份
- [ ] Worker RPC、platform Checkpoint Coordinator 与 Workspace Store 文件动作边界、platform task store 单 owner checkpoint 事务、`task_complete`/失败/取消/中断终态 delivery、`result_ref`/稳定 `delivery_id`、LLM Proxy session capability 接口草案完成；恢复扫描、重试/dead-letter、carrier 确认前崩溃语义明确
- [ ] 隔离、绑定、`/stop`、blocked 取消、崩溃恢复、代理鉴权的验收用例进入 implementation plan
- [ ] 压测计划定义真实 `worker_memory_budget`、starting/task 预留、`MAX_ACTIVE_WORKERS`、`WORKER_IDLE_TIMEOUT`、`DELIVERY_RETRY_WINDOW`、`TASK_RESULT_RETENTION` 与配额，覆盖实时指标缺失回退；不写未测数字
- [ ] capability policy、不可授权边界、源文件变更审批和审计事件 schema 完成
- [ ] P0 资源配额、跨租户公平调度和告警指标经压测确定
- [ ] P0 物理部署单元、systemd/等效监督器、健康检查、失败自动重启和无 HA 失败语义完成。
- [ ] 目标 2 vCPU / 4 GiB RAM 主机完成容量 spike：控制面/PostgreSQL/系统/page cache 基线、Worker 冷启动 p50/p95、cgroup memory current/peak/high/max、starting/task 预留、并发 P95、checkpoint/fsync、CPU/PID/磁盘、swap 和 OOM 均有记录，并据此派生 worker_memory_budget
- [ ] `MAX_BACKEND_HISTORY_BYTES`、`MAX_WORKING_BYTES`、`MAX_DISPLAY_HISTORY_BYTES`、`MAX_TASK_OUTPUT_BYTES`、`MAX_TOOL_STDOUT_BYTES`、`MAX_SNAPSHOT_BYTES` 的运行时限额和超限状态完成。
- [ ] 快照 writing/committed/quarantined、控制面 lease、文件 rename、目录 fsync、PostgreSQL 提交、`SNAPSHOT_ORPHAN_GRACE`、delivery 恢复/去重/dead-letter、成功/失败/取消/中断终态丢失、idle claim/evict 竞态、Worker OOM、磁盘不足和 orphan snapshot 清理验收完成
- [ ] 结构化日志与审计轮转、保留期、磁盘告警、备份恢复和 PostgreSQL 连接池/资源配置草案完成。
- [ ] `MAX_RUNNING_TASKS`、`MAX_LLM_INFLIGHT`、`PER_TENANT_RUNNING_LIMIT`、`PER_TENANT_QUEUE_LIMIT`、`MAX_ACTIVE_WORKERS`、`WORKER_IDLE_TIMEOUT`、`DELIVERY_RETRY_WINDOW`、`TASK_RESULT_RETENTION`、`SNAPSHOT_ORPHAN_GRACE`、队列上限与各类资源配额由压测或明确运维策略确定；未测/未定义数字不进入对外文档
- [ ] 受保护的管理员调度配置接口完成：`MAX_RUNNING_TASKS` 范围校验、旧值/新值/原因/actor/version 审计，以及调高、调低、暂停/drain 的生效语义完成。
- [ ] PostgreSQL-backed scheduler 完成：事务入队、同 session FIFO、跨租户公平、单 task 领取、队列上限、重启恢复、`LISTEN/NOTIFY` 唤醒和周期扫描兜底均有验收用例。
- [ ] 调度压测覆盖并发配置动态变化、platform/PostgreSQL/scheduler 重启、queued task 不丢失、不重复领取和 running task 不被错误强杀。
- [ ] P0 不依赖外部消息队列；内存队列只能作为唤醒缓存，不能作为 queued task 的唯一存储。

---

## 4. 明确不再讨论的选项

1. 单进程多线程作为租户隔离方案。  
2. 以路径白名单/命令黑名单作为主隔离。  
3. 自动重放可能执行过 Shell 的崩溃任务。  
4. 默认 auto-approve 公网注册。  
5. 在文档中承诺固定并发人数或零微信风险。  
6. 未经独立安全评审，以 nsjail、bubblewrap 或其他轻量沙箱直接替换 rootless Podman。
7. P0 引入 RabbitMQ、Redis、Celery 等外部消息队列作为任务事实来源。

**下一步：** 门槛通过后，再写 implementation plan（API 签名、模块任务拆解、测试矩阵）。
