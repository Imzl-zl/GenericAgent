# 多租户 IM 个人助手平台 — 架构设计

**日期：** 2026-07-23  
**状态：** 设计已确认；Foundation 垂直切片已实现（2026-07-24）；LLM Proxy + capability_token 切片已实现（2026-07-25，Slice 2a — 真实上游 Key 仅存于 `cmd/llm-proxy`）；Worker 容器隔离切片已实现（2026-07-25，Slice 2b — `WorkerRuntime` + `worker-manager` + rootless Podman，见 [Slice 2b SPEC](../../../.codex-tasks/20260725-worker-containerization-slice-2b/SPEC.md)）；iLink 官方绑定流程 + 媒体收发（任意文件格式）+ Windows loopback 端到端验证已完成（2026-07-25，Slice 3c — 见 [iLink 绑定流程 SPEC](2026-07-25-ilink-official-binding-flow.md)）；P0 剩余：Linux Podman 端到端验证、资源配额/准入控制、文件上传/工作区  
**对应 PRD：** [2026-07-21-multi-tenant-im-platform-design.md](2026-07-21-multi-tenant-im-platform-design.md)  
**试运行目标：** 一台 Linux 服务器，先服务约 10–20 名已批准用户；容量由部署前压测结果决定，不作未经验证的并发承诺。
**当前 P0 验证基线：** 目标主机为 2 vCPU / 4 GiB RAM；该硬件只用于容量实验，不构成用户数、并发数或响应时间承诺。

---

## 1. 目标与边界

平台让每位用户通过 Web 注册、扫码绑定自己的微信 bot，在私人或团队工作区中使用 GenericAgent。平台必须允许受控的 Shell/Python 文档处理，同时保证任一租户不能访问宿主机、平台密钥、其他租户的文件、状态或输出。

### 1.1 P0 目标

1. 个人模式闭环：注册、人工批准、微信绑定、私人对话、文件处理、`/stop`、`/new`。
2. 每个个人或团队工作区具有可证明的执行隔离，而不是仅靠路径过滤。
3. Shell/Python 仅在会话隔离 Worker 内运行。
4. 每个完成任务都有分阶段持久化 checkpoint；恢复只认最后一个 committed 快照，崩溃不会重放可能已执行 Shell 的任务。
5. 平台真实 LLM Key、bot token、数据库凭证永不进入 Worker。

### 1.2 非目标

- 不在 P0 承诺固定活跃 session 数、响应时间或资源用量。
- 不在 P0 提供任意公网访问、宿主机桌面控制、Docker/Podman 控制权或完整管理员后台。
- 不以单进程多线程作为执行隔离方案。
- 不让字符串黑名单、路径白名单或 system prompt 承担安全边界职责。
- 不承诺微信/iLink 通道零风险或永久兼容；需由运营方持续评估服务条款和协议变化。

---

## 2. 不可降级的设计决议

| 决议 | 结论 |
|---|---|
| 部署基线 | 单 Linux 主机 + rootless Podman Worker；Docker 仅在能提供等效 rootless 隔离时可替换 |
| 运行单元 | 每个活跃 `personal:{user_id}` 或 `team:{team_id}` 对应一个短生命周期 Worker 容器 |
| 并发 | 同一 session 串行且有界排队；不同 session 并行；不以 session lock 阻止入队 |
| 人设 | persona-in-task：persona snapshot 写入 task envelope，Worker 取任务时才设置 |
| 准入 | 新账号默认 `pending`；仅 `approved` 用户可启动 Worker 或消耗模型 |
| 数据库 | P0 使用 PostgreSQL；SQLite 不作为多 Worker 的正式状态库 |
| 状态持久化 | 每个成功完成 task 采用文件与数据库分阶段 checkpoint；运行中任务崩溃后标记 `interrupted`，禁止自动重放 |
| LLM Key | 真实 Key 只存在于控制面 LLM Proxy；Worker 只持有短时、单 session、可撤销凭证 |
| 工具 | Shell/Python 可用，但仅在 Worker；默认无任意外网、无宿主机挂载、无容器管理 socket |
| 团队人设 | `team_members.persona_id → teams.team_persona_id → platform default` |
| 团队 key_info | 团队会话内共享；PRD 必须明示该隐私边界 |
| 中继 | `@username <消息内容>` 直接转发给目标 bot，不调用 AI，不进 task 队列；仅个人上下文触发，团队上下文里 `@username` 作为普通消息给 AI；接收方可用 `/relay_off` 拒收 |
| P0 物理部署 | §3.1 的组件是逻辑边界；默认部署为 `platform`、`worker-manager`、`llm-proxy` 三个应用进程加 PostgreSQL，不要求一组件一进程 |
| 容量准入 | `MAX_ACTIVE_WORKERS` 只是硬上限；调度必须同时检查 running/idle Worker 内存、CPU、PID、磁盘、模型并发和租户配额 |
| 管理员并发配置 | 受保护的运营操作可运行时调整 `MAX_RUNNING_TASKS`；值不得超过目标主机测得硬上限；每次修改记录 actor、原因和 config version |
| P0 任务队列 | PostgreSQL `tasks` 表是持久化事实来源；`LISTEN/NOTIFY` 只做唤醒提示；P0 不引入 RabbitMQ、Redis 等外部消息队列 |

---

## 3. 总体拓扑与信任边界

```text
用户微信 / Web
      │ TLS、登录会话、消息身份
      ▼
┌───────────────────────────────────────────────────────────────┐
│ 控制面（宿主机；独立服务账号）                                 │
│  Web/Auth │ IM Gateway │ Router │ PostgreSQL │ Audit/Quota     │
│  Binding Service │ Bot Transport Adapter │ Worker Manager      │
│  LLM Proxy（唯一持有真实上游 Key）                              │
└───────────────┬───────────────────────────┬───────────────────┘
                │ 受认证的固定 RPC          │ 私有、按 session 授权
                ▼                           ▼
      ┌─────────────────┐         ┌────────────────────────────┐
      │ Worker Manager  │         │ LLM Proxy                  │
      │ rootless Podman │         │ 校验 session 凭证、配额、  │
      │ 固定镜像和挂载  │         │ 模型策略；附加真实 API Key │
      └───────┬─────────┘         └────────────────────────────┘
              │
   ┌──────────┼──────────────────────────────────────┐
   ▼          ▼                                      ▼
┌────────┐ ┌────────┐                           ┌────────┐
│Worker  │ │Worker  │                           │Worker  │
│user_42 │ │team_7  │                           │user_9  │
└────────┘ └────────┘                           └────────┘
```

控制面是身份、策略、密钥、队列和审计的唯一权威。Worker 只执行其 session 的任务，不能直接读控制面数据库，不能访问其他 Worker，不能读取平台秘密。

### 3.1 控制面组件

| 组件 | 职责 | 不可承担的职责 |
|---|---|---|
| Web/Auth | 注册、登录、审批、绑定和团队管理 | 不直接执行用户代码 |
| IM Gateway | 多 bot 收消息、消息去重、回复发送 | 不直接持有 Agent 状态 |
| Router | 身份验证、命令状态机、session/task 路由 | 不直接读写 GA 私有字段 |
| Bot Transport Adapter | 封装 iLink 客户端、游标、token 生命周期 | 不使用共享 `~/.wxbot/token.json` |
| Worker Manager | 创建、鉴权、监测、终止 rootless Worker | 不接受用户提供的镜像、挂载或容器参数 |
| LLM Proxy | 模型路由、真实 Key、配额、上游错误转换 | 不信任 Worker 自带的 tenant 标识 |
| PostgreSQL | 事务性元数据、任务和审计状态 | 不保存明文业务秘密 |
| Workspace Store | 每 session 受控 volume 的 staging、fsync、rename、quarantine 与 snapshot 文件读写；通过 platform task store 的版本化 RPC 获取/更新元数据 | 不由 Gateway 直接暴露宿主机路径，不直接写 PostgreSQL，不把数据库或宿主机路径交给 Worker |

### 3.1.1 P0 单机部署进程

§3.1 的表描述逻辑组件，不等于需要启动同等数量的进程。当前 P0 在单 Linux 主机上的默认部署形态如下；所有应用单元由 systemd 或等效进程监督器管理，并配置健康检查与失败自动重启。P0 不承诺高可用，LLM Proxy 故障时必须向任务返回可见错误，不得静默成功或自动重放可能产生副作用的任务。

| 部署单元 | 默认包含的逻辑组件 | 边界与职责 |
|---|---|---|
| `platform` | Web/Auth、IM Gateway、Router、Binding Service、Bot Transport Adapter、Audit/Quota、Task Store/Checkpoint Coordinator | 只处理身份、路由、队列、快照元数据和审计；是 `tasks`、`task_deliveries`、`workspace_snapshots` 的唯一 PostgreSQL 事务 owner；不执行用户代码，不持有 GA runtime state |
| `worker-manager` | Worker Manager、Workspace Store 文件协调 | 只使用固定镜像、固定挂载和版本化 RPC 管理 Worker，执行受控 volume 文件动作；不直接写 PostgreSQL，不持有真实 LLM Key |
| `llm-proxy` | LLM Proxy | 独立持有真实上游 Key，校验 session capability、模型策略和用量配额 |
| PostgreSQL | PostgreSQL | 事务性元数据、任务、快照指针和审计状态；不保存明文业务秘密 |

逻辑模块仍应在代码中保持职责边界。若压测证明 IM 长轮询或 Web 请求会阻塞 `platform`，可把 Bot Transport Adapter 拆成独立部署单元；这属于性能调优，不得改变 Worker 与 LLM Proxy 的安全边界。

### 3.2 Worker 隔离要求

Worker Manager 只能以固定镜像、固定入口点和经校验的 session volume 启动容器。每个 Worker 必须满足：

- rootless Podman，独立非特权 UID；不共享宿主机 root 权限。
- 只读 root filesystem；`/tmp` 为 tmpfs；仅挂载本 session workspace 读写。
- `cap-drop=ALL`、`no-new-privileges`、seccomp、PID/CPU/内存/磁盘配额和任务超时。
- 不挂载 Docker/Podman socket、宿主机 `/proc`、用户目录、服务凭证或数据库 socket。
- 默认禁止任意网络；仅允许访问私有 LLM Proxy 与显式批准的平台代理能力。
- Worker 可读到的 LLM 凭证必须是短时、仅限该 session 的能力凭证；不能用于读取真实 Key 或跨 session 计费。
- Worker 被强制终止时，影响范围最多是当前 session 的未完成任务和工作区半成品。

若服务器无法提供 rootless user namespace、cgroup 和所需 seccomp 能力，则不启用 Shell/Python 工具；不得降级到宿主机进程执行。

### 3.3 能力授权、文件保护与审计

安全判断以能力和隔离边界为准，不以命令文本猜测恶意性。有效权限为：

```text
平台不可授权拒绝 ∩ 部署策略 ∩ 用户/团队 capability grant ∩ 当前 session 状态
```

拒绝优先。grant 必须有授予者、理由、创建/到期时间、策略版本和审计事件；Owner 不能绕过平台不可授权拒绝。

| 类别 | P0 默认 | P1 及以后显式授权 |
|---|---|---|
| 文件 | 读取 `/workspace/input`；写入 `/workspace/output` | 修改源文件、删除文件、团队文件写入 |
| 代码 | Worker 内 Shell/Python、文档解析 | 更高 CPU/内存/时长配额、长任务 |
| 网络 | 仅私有 LLM Proxy | 经出口代理的域名/协议白名单、外部 API |
| 模型 | 平台允许模型与基础额度 | 高价模型、更高 token/并发额度 |

上传原件放入只读 `input`；助手生成物写入 `output`；临时内容位于可清理 `scratch`。P0 不向 Agent 授予删除原件或直接修改源文件的能力。P1 的源文件变更必须遵循：生成变更清单 → 用户确认 → 平台原子应用或移入可恢复回收站。Shell 不得绕过该流程。

以下能力永久不可授权：宿主机和其他 workspace 访问、真实 Key/数据库凭证读取、Docker/Podman socket、提权、挂载/namespace 管理、任意公网、任意删除源文件。Worker 内的容器边界负责阻断；路径规则和命令检查仅提供更早的用户提示。

P0 审计必须记录：actor、team/session/task、工具与 capability 决策、策略版本、脱敏后的命令摘要哈希、逻辑 cwd、输入/输出字节、退出码、超时/OOM/PID/磁盘事件、网络/越界拒绝、LLM token 用量、审批与授权变更。审计日志不得包含真实 Key、完整 token 或完整敏感命令参数。

检测用于告警和追责，不能替代隔离：Worker 本就没有宿主机 Key、其他租户文件和任意外网；尝试读取或外传时应被策略拒绝并审计。后续可增加出口目的地审计、文件变更 manifest、异常资源规则和主机级遥测。

### 3.4 性能与资源公平性

控制面必须分别限制每 user/team 的排队长度、单任务时长、CPU、内存、PID、磁盘、输出字节和模型 token；同时限制全局活跃 Worker 与 LLM 并发。调度采用 per-session FIFO 与跨租户公平配额，避免单一用户占满 Worker 或上游模型额度。

P0 的全局准入不能只按 Worker 数量判断。`worker_memory_budget` 必须由目标主机实测派生：

```text
worker_memory_budget = host_memory_limit
  - system_and_kernel_reserve
  - control_plane_memory_p95
  - postgres_memory_p95
  - page_cache_reserve
  - safety_margin
```

调度器以各 Worker cgroup `memory.current` 的实时总和作为当前占用，并为 starting Worker 和下一 task 保留静态预算：

```text
sum(worker_memory_current)
+ starting_worker_reservations
+ next_task_reservation
<= worker_memory_budget
```

running/idle P95 用于新 Worker 尚未启动时的预留、实时指标不可用时的保守回退和容量校准；`memory.peak` 用于更新预留模型，不能代替当前占用。`starting_worker_reservations` 和 `next_task_reservation` 只计算尚未被已观测 `memory.current` 覆盖的剩余 headroom，避免重复计费。实时指标不可用时必须回退完整静态预留，不得静默放宽。Worker Manager 还必须用 cgroup `memory.high`/`memory.max` 和原子准入预留防止并发检查竞态。

`MAX_ACTIVE_WORKERS` 是最终硬上限，但不是唯一准入条件；调度器必须取内存、CPU、PID、磁盘、LLM 并发、租户额度和队列额度的共同约束。swap 不计入可用 Worker 容量，但不等同于统一关闭宿主机 swap：cgroup v2 的 `memory.swap.max` 或 v1 等效 memsw 策略必须显式配置并在目标主机验证，是否设为 0 由压测和延迟目标决定。

所有具体数值均由目标主机压测写入部署配置；未测得前不得写死为产品承诺。

### 3.4.1 运行时并发配置

管理员可通过受保护的运营操作调整调度配置；配置更新写入 PostgreSQL 的 `runtime_settings`，带 `version`、`updated_by`、`updated_at`、`reason` 和审计事件。不可由管理员突破的主机硬上限、Worker 内存预算和安全策略仍来自部署配置与 Worker Manager。

P0 至少区分以下语义：

| 配置 | 作用 |
|---|---|
| `MAX_RUNNING_TASKS` | 全局同时处于 `starting/running` 的 task 数，是管理员的主要并发旋钮 |
| `MAX_LLM_INFLIGHT` | 同时请求上游 LLM 的数量，防止模型限流或代理过载 |
| `PER_TENANT_RUNNING_LIMIT` | 单 user/team 同时运行的 task 上限 |
| `PER_TENANT_QUEUE_LIMIT` | 单 user/team 可保留的 queued task 上限 |
| `MAX_ACTIVE_WORKERS` | running + idle Worker 的容器与资源硬上限，不等同于运行中 task 数 |
| `WORKER_IDLE_TIMEOUT` | Worker 完成 checkpoint 后允许复用的 idle 宽限期 |

有效运行并发取所有约束的最小值：

```text
effective_running_tasks = min(
  admin_max_running_tasks,
  measured_host_task_cap,
  memory_admission_limit,
  cpu_admission_limit,
  max_llm_inflight,
  tenant_quota
)
```

调高 `MAX_RUNNING_TASKS` 后调度器唤醒并尝试领取 queued task；调低时不强杀已经运行的 task，现有 task 自然结束，idle Worker 优先淘汰，新 task 等待；设置为 `0` 表示暂停启动新 task 的 drain/维护模式。所有修改都必须校验范围、审计并支持按版本追踪。


---

## 4. 会话、Worker 与 GenericAgent 契约

### 4.1 会话键与工作区

| 上下文 | session_key | 工作区 |
|---|---|---|
| 个人 | `personal:{user_id}` | `workspaces/personal/{user_id}/` |
| 团队 | `team:{team_id}` | `workspaces/team/{team_id}/` |

个人与团队永不共享工作区、history、working、快照或任务队列。团队成员仅共享团队工作区，不共享任何成员的个人工作区。

表中的 `workspaces/...` 路径为逻辑标识，用于快照元数据与审计定位；实际存储由 Worker Manager 按 `workspaces.volume_id` 挂载为容器内 `/workspace`，控制面不直接暴露宿主机路径。

### 4.2 Worker RPC

控制面只能经版本化 RPC 调度 Worker：

```text
StartSession(session_key, snapshot_ref, runtime_policy)
ExecuteTask(TaskEnvelope)
CancelTask(task_id)
Health()
Shutdown(reason)
```
快照控制使用同一条版本化 RPC 通道：platform Checkpoint Coordinator 通过 `worker-manager` 调用 `BeginCheckpoint(task_id, manifest)`，platform task store 创建 writing 元数据和 lease，Worker 在 token 指定的 session staging ref 中写入 bundle 后返回 `CheckpointReady(token, checksum, result_digest)`；`worker-manager` 完成受控文件动作后调用幂等 `CommitCheckpoint(token, file_ref, checksum)`，platform task store 在单个 PostgreSQL 事务中完成元数据、task 和 delivery 提交，再返回 `CheckpointCommitted(token)`。Worker 不访问 PostgreSQL、数据库 socket 或宿主机路径；`lease_owner` 是 platform 控制面身份。

`TaskEnvelope` 至少包含：

```json
{
  "task_id": "uuid",
  "session_key": "personal:42",
  "requester_user_id": 42,
  "source": "wechat|web",
  "message_id": "carrier-idempotency-key",
  "prompt": "...",
  "persona_snapshot": ["..."],
  "tool_policy_version": "v1",
  "created_at": "RFC3339"
}
```

`source` 协议层保留 `wechat|web` 两个取值；P0 实际只接收 `wechat` 通道产生的 task，Web 在 P0 仅用于注册、审批、绑定和人设编辑，不产生聊天 task。Web 聊天通道属 P2 范围。

Worker 的 display stream 只允许 Gateway/Router 这一控制面消费者读取：`chunk`、`tool_progress`、`task_complete`、`task_failed`、`task_cancelled`、`task_interrupted`。`checkpoint_ready` 不进入 display stream，只由 Worker Manager 在同一版本化 checkpoint RPC 通道接收；因此 checkpoint 不会抢读或复制流式队列。

`chunk` 和 `tool_progress` 是临时流，不要求持久化或重放；`checkpoint_ready` 只携带受控 staging ref、checksum 和结果摘要；`task_complete` 是成功终态交付，Gateway/Router 以稳定 delivery_id 处理并可按未确认状态重试，至少包含：

```json
{
  "delivery_type": "task_complete",
  "delivery_id": "task_id:task_complete",
  "task_id": "uuid",
  "status": "succeeded",
  "snapshot_id": "uuid",
  "result_ref": "workspace-result-ref",
  "result_digest": "sha256:...",
  "usage": {"input_tokens": 0, "output_tokens": 0}
}
```

成功 task 的最终用户可见结果必须在 `task=succeeded` 提交前作为有字节上限的不可变 payload 持久化；`result_ref` 是指向 committed snapshot bundle 内该 payload 的不透明引用，不得是任意宿主机路径，读取时必须校验 `result_digest`。pending 或未确认的 delivery 每次重试都从 `result_ref` 读取并重发，不重放已消费的 chunk。

`task_failed`、`task_cancelled` 和恢复生成的 `task_interrupted` 也是终态交付类型，使用各自稳定的 `delivery_id`，在 `task_deliveries` 中保存有字节上限的脱敏错误码/用户提示；它们不需要 `snapshot_id` 或 `result_ref`，但同样按确认状态重试或进入 dead-letter，不重新执行 task。

控制面 task store 为每个 `(task_id, delivery_type)` 建立唯一的 `task_deliveries` 记录和稳定 `delivery_id`；所有 terminal task 的恢复扫描都可补建 pending delivery，失败/取消/中断终态必须在对应 task 状态事务中一并写入 bounded terminal payload。重试沿用同一个 `delivery_id`；平台自己的 outbox/状态更新必须去重。BotTransportAdapter 若底层 carrier 支持幂等键，必须传递该 ID；carrier 不支持幂等时，发送确认前的崩溃窗口只能提供 at-least-once 语义，重复消息风险须可见并审计，但不得重新执行 task。

### 4.3 GA 适配边界

GenericAgent 仍是 Worker 内的任务引擎；平台不直接读写 `handler`、`history`、`task_queue`、`extra_sys_prompts` 或 LLM backend 私有字段。Worker 内必须提供明确、可版本化的 `ManagedAgent` 适配层。

`ManagedAgent` 必须保证：

1. `work_dir` 覆盖所有租户可写路径：长 prompt、task 文件、模型日志、历史、working、临时文件和 peer hint；静态 assets 只读。
2. 每个 task 的 persona 进入 task envelope，Worker 消费该 task 时设置；禁止会话级 `set_persona` 造成排队 race。
3. Worker 在新 handler 创建后，先从 session snapshot 注入完整 working，再运行 task；任务结束后将完整 working 写回 runtime snapshot。
4. snapshot 仅在任务完全静止的边界生成，包含 `backend_history`、`working`、`display_history` 与 schema version。
5. 工具 schema 是 runtime policy 的子集。P0 只保留会话内文件、Shell/Python 与文档解析所需工具；宿主机桌面、全局浏览器、容器管理和任意网络工具默认不可用。
6. `shutdown` 先请求协作取消；超过宽限期由 Worker Manager 销毁容器。不能把 `abort()` 描述为对任意阻塞调用的即时终止保证。
7. Worker 必须在流式读取工具 stdout/stderr 时执行字节上限；超限立即终止对应子进程并返回可见的配额错误，不能等任务结束后再截断结果。

---

## 5. 数据模型与不变量

P0 使用 PostgreSQL。每个状态变更在事务中完成，并附加审计事件；业务秘密为应用层字段加密，带 `key_version`。

| 实体 | 关键字段与不变量 |
|---|---|
| `users` | `username` 规范化后唯一；`status ∈ pending, approved, blocked`；密码使用强 KDF 哈希；blocked 立即撤销会话和调度权 |
| `auth_sessions` | 仅存哈希后的 session/refresh token；到期、撤销和设备审计 |
| `wechat_qr_sessions` | QR 会话 token、平台 user、状态、过期时间；iLink 官方扫码 `confirmed` 时直接返回 `ilink_bot_id`/`bot_token`/`ilink_user_id`/`baseurl` 顶层字段完成绑定，无需 `/activate`（见 [iLink 绑定流程 SPEC](2026-07-25-ilink-official-binding-flow.md)） |
| `bots` | `owner_id` 唯一；`bot_uuid` 唯一；`ilink_user_id` 在激活后绑定；token 密文、状态、密钥版本 |
| `bot_transport_state` | 每 bot 独立加密 update cursor、重连状态、错误时间；禁止共享本地 token 文件 |
| `context_tokens` | `(bot_id, ilink_user_id)` 唯一；加密、到期、最后使用时间；视为能力凭证 |
| `workspaces` | session_key 唯一；personal 与 team 的归属列互斥；只保存受控 volume ID 和 snapshot 元数据，不保存任意宿主机路径 |
| `workspace_snapshots` | snapshot_id、workspace、task、file_ref、checksum、state ∈ writing/committed/quarantined、lease_owner（控制面身份）、lease_until、created_at、committed_at；committed snapshot 是否保留由 retention policy 决定，不以 current 指针为唯一存活条件 |
| `teams` | owner、名称、默认人设、状态 |
| `team_members` | `(team_id,user_id)` 唯一；`status`、role、nullable `persona_id`；踢人/封禁由路由每条消息复核 |
| `active_contexts` | personal 时 `team_id IS NULL`；team 时 `team_id IS NOT NULL`，且成员关系必须 approved |
| `tasks` | task_id、session、requester、message idempotency key、状态、顺序号、result_ref/result_digest、最终用量、succeeded_at、terminal_at；每 session 同时最多一个 running task |
| `task_deliveries` | `(task_id, delivery_type)` 唯一；delivery_id、状态 ∈ pending/sending/acked/dead_letter、payload_ref/payload_digest 或 bounded error payload、尝试次数、next_attempt_at、attempt_lease_until、delivery_deadline_at、sent_at、acked_at、terminal_at；用于终态结果恢复与去重，不作为 task 事实来源 |
| `task_events` | 状态转换、Worker ID、错误码、时间；不存模型 Key 或完整敏感 prompt |
| `relay_preferences` | user_id、opt_out bool、updated_at；控制是否接收 `@username` 转发消息，默认 opt_out=false |
| `audit_events` | 登录、审批、绑定、策略拒绝、密钥访问、任务生命周期和管理操作；默认不记录中继正文 |

### 5.1 绑定状态机

```text
requested → qr_pending → awaiting_activation → active
                        ├→ expired
                        └→ revoked
```

Web 创建 `wechat_qr_session` 后展示二维码（前端用 `qrcode.react` 本地生成，不直接用 iLink 返回的 URL 做 `<img>`）。平台轮询 iLink `get_qrcode_status`，`confirmed` 时直接拿到 `ilink_bot_id`/`bot_token`/`ilink_user_id`/`baseurl` 顶层字段，原子写入 `bots` 表完成绑定，无需用户额外发 `/activate`。之后每条消息都必须同时匹配 bot 与绑定的 `ilink_user_id`。

### 5.2 团队邀请状态机

- 邀请码：用户主动提交邀请码 → `pending_owner` → owner `approved` 或 `rejected`。
- 直接邀请：owner 发起 → `pending_member` → 成员接受 → `pending_owner` → owner `approved` 或 `rejected`。
- 成员被移除或用户被封禁后，Router 在下一条消息立即拒绝进入团队；运行任务按策略取消并记录审计。

### 5.3 任务状态机

```text
queued → starting → running → succeeded | failed | cancelled | interrupted
```

任务先持久化，再由该 session 的 Worker 按顺序领取。Worker 或主机崩溃时，`running` 任务转为 `interrupted`；不得自动重放，因为 Shell/Python 可能已产生不可逆副作用。`queued` 任务保留顺序，可在恢复后继续执行。

**blocked 用户运行中任务取消机制：** `users.status` 变更为 `blocked` 时，控制面在同一事务中发布取消事件，Worker Manager 取消该用户作为 `requester_user_id` 的所有 `running` 任务，并按 §3.2 Worker 销毁策略终止对应容器。`queued` 任务直接标记 `cancelled`。取消必须产生审计事件，记录触发原因（`user_blocked`）、受影响 task 列表与资源回收结果。该机制不依赖用户下一条消息触发，确保 blocked 即时生效。

### 5.4 任务队列与调度

任务进入 PostgreSQL 后先以 `status=queued` 持久化；调度器在事务中锁定候选 session/task，确认该 session 没有 `starting/running` task 且全局、租户和资源预算允许后，才将任务推进到 `starting`。领取不能只依赖进程内计数，避免重启或多进程造成超额并发。

调度选择遵循同 session FIFO 与跨租户公平轮转；队列满时在入队事务中明确拒绝。P0 可以只有一个逻辑 scheduler loop；未来增加 scheduler 实例时，必须继续使用数据库行锁或等效租约保证同一 task 只被领取一次。

新任务和运行时配置变化可以通过 PostgreSQL `LISTEN/NOTIFY` 唤醒调度器，但通知不是任务数据，也不是可靠队列；断线、重启或通知丢失后必须通过周期扫描恢复。Worker 的 `chunk` 和 `tool_progress` 仍走版本化 RPC 流，由单一控制面消费者读取，不写入任务队列表。

P0 不引入 RabbitMQ、Redis、Celery 等外部消息队列。PostgreSQL `tasks` 表负责任务事实、幂等、恢复和审计关联；内存队列只可作为降低唤醒延迟的缓存，不能作为 queued task 的唯一存储。

---

## 6. 入站路由与命令优先级

### 6.1 每条入站消息的固定流程

1. Bot Transport Adapter 验证来源并以 `(bot_id, message_id)` 幂等记录消息。
2. 查询 active bot，验证 `bot_uuid` 与已绑定 `from_user_id`；未绑定身份只允许执行绑定激活，不可消耗 Agent。
3. 检查 `users.status=approved`；blocked/pending 不可启动 Worker。
4. 加密缓存对应 context token，并更新 bot 可达状态。
5. 解析平台命令和 relay 状态；只有通过授权的普通消息才会创建 Task。
6. 对团队上下文，每条消息重查 `team_members.status=approved` 和成员人设。
7. 在单一事务中创建 task、幂等键和审计事件，再通知 Worker Manager。

### 6.2 命令规则

| 优先级 | 条件 | 动作 |
|---|---|---|
| 1 | 绑定激活 | 已废弃：iLink 官方扫码 `confirmed` 时直接完成绑定，无需 `/activate`（见 [iLink 绑定流程 SPEC](2026-07-25-ilink-official-binding-flow.md) §6.5） |
| 2 | pending team 流程 | `/同意 t-456`（成员接受直接邀请）、`/批准 t-456`（Owner 批准成员加入）、`/拒绝 t-456`（任一方拒绝）；Router 按发送者角色与邀请当前状态分发；仅一个候选时可省短 ID |
| 3 | session 命令 | `/个人`、`/团队`、`/new`、`/stop`、`/邀请码 <code>`、`/relay_on`、`/relay_off`、`/移除 @username`（Owner）、`/我的身份`、`/状态` |
| 4 | relay 转发 | 个人上下文下 `@username <消息内容>`：直接转发给目标 bot，不调 AI，不进 task 队列；团队上下文里 `@username <消息内容>` 作为普通消息给 AI |
| 5 | 普通消息 | 创建 task 并入队 |

命令语义补充：

- `/我的身份` 查询身份绑定、账号状态、当前个人/团队上下文与 bot 状态；`/状态` 查询当前 session 的任务队列、资源用量与上下文状态。两者均为只读查询，互不重叠。
- `/邀请码 <code>` 由用户提交团队邀请码，创建 `pending_owner` 申请，等待 Owner 批准；可经 Web 表单或 IM 提交，状态机一致。
- `/批准 t-456` 仅在邀请处于 `pending_owner` 时由该团队 Owner 使用；`/同意 t-456` 仅在邀请处于 `pending_member` 时由被邀请成员使用；`/拒绝 t-456` 在两个阶段均有效，按发送者角色解释。
- `/移除 @username` 由 Owner 移除团队成员，立即生效；被移除成员下一条消息不能进入团队，运行中的成员任务按 §5.2 策略取消并审计。

`/stop` 只能取消 `requester_user_id` 等于调用者的 running task。队列长度、运行中任务数、磁盘和模型用量均由可配置的 tenant/session 配额限制；满额时明确拒绝或提示等待，不无限排队。

---

## 7. LLM、凭证与网络策略

### 7.1 LLM Proxy

真实上游 Key 只由 LLM Proxy 读取，使用宿主机凭证注入而非仓库 `mykey.py`。Worker 镜像内仅有 Proxy 地址与短时 session capability；Proxy 必须验证：

- capability 未过期且绑定到请求的 session；
- 模型与用量策略允许；
- 请求来自私有 Worker 网络；
- 上游 429/5xx 被转换成可观测、可重试但不静默成功的错误。

Worker 不能通过伪造 `X-Session-Key` 获取其他租户配额。session capability 只允许消耗自身额度，不可读取 Key 或调用管理 API。

### 7.2 Bot 凭证与传输状态

`WxBotClient` 必须由 `BotTransportAdapter` 包装：

- 显式传入每 bot 的持久化状态，而不是默认 `~/.wxbot/token.json`；
- 支持显式停止、原子 token 替换、cursor 恢复和 `errcode=-14` 状态迁移；
- token 失效时 bot 转为 `expired`，停止轮询，向 Web 显示重新绑定，不覆盖其他 bot 状态；
- 当前传输层没有 `refresh_token`、`stop_loop`、`send_emoji` 的假定 API；平台实现必须提供适配方法，表情能力以实际 `send_image` 等传输能力为准。

---

## 8. 持久化、恢复与可观测性

### 8.1 成功任务 checkpoint

Workspace Store 是受控 snapshot 文件 staging、fsync、rename、quarantine 的唯一文件操作 owner；platform task store 是 snapshot 元数据、task 和 delivery outbox 的唯一 PostgreSQL 事务 owner；Worker 只读取 GA runtime state 并通过版本化 RPC 提供 staging bundle。每个成功 task 的顺序为：
```text
Platform Checkpoint Coordinator 调用 BeginCheckpoint；platform task store 创建 snapshot_id、state=writing、lease_owner=platform:<instance>、lease_until，并返回带 generation 的 opaque checkpoint token
→ Worker 深拷贝 backend_history / working / display_history，生成有字节上限的最终用户结果 payload
→ Worker 将 runtime state 与最终结果写入 token 指定的 session staging bundle
→ Worker 返回 CheckpointReady(token, checksum, result_digest)
→ Worker Manager/Workspace Store 校验本地 staging manifest 并 fsync staging bundle
→ 原子 rename 为带 snapshot_id 和 checksum 的不可变快照
→ fsync 快照目录，生成 result_ref / result_digest
→ platform task store 以单个 PostgreSQL 连接执行事务：将 snapshot 标为 committed，更新 current_snapshot_id、result_ref、task=succeeded、succeeded_at、terminal_at，并插入/幂等更新 task_deliveries=pending；该事务不跨进程包含文件操作
→ platform 返回 CheckpointCommitted；Gateway/Router 继续作为 display stream 唯一消费者，delivery outbox 可从 task 行重建终态交付
```

Worker Manager 根据同一 RPC 会话和心跳向 platform task store 请求续租 writing lease；Worker 本身不续租、不写数据库。lease token 带 generation，过期或续租失败即失效，Worker Manager 必须停止该 staging session；`CommitCheckpoint` 只接受当前 generation，失败或 Worker 断连时不得提交 `succeeded`，让 lease 过期后进入 orphan 流程。

数据库中的 `current_snapshot_id` 是恢复当前会话的权威指针；`workspace_snapshots` 是所有 snapshot 生命周期的权威登记。文件已 rename 但提交事务未完成时，旧指针仍有效，writing 记录及其控制面 lease 防止清理器误删；数据库已提交但 `task_complete` 尚未送达时，任务保持 `succeeded`，恢复扫描按 task_id 补建或重试 `task_deliveries`，按未确认状态重发终态事件或最终结果，不重新执行任务，也不重放 chunk。

orphan 候选必须同时满足：没有 committed 记录，且 writing lease 已过期或不存在，并且文件年龄超过可配置的 `SNAPSHOT_ORPHAN_GRACE`。platform task store 锁定并复核元数据、引用和 token generation 后，使 token 失效；worker-manager/Workspace Store 执行受控文件 quarantine，不能由清理器直接改数据库状态。

quarantine 是文件与元数据的分阶段动作：worker-manager 先原子 rename 到 quarantine 目录并回报 file_ref，platform task store 再在事务中将状态记为 `quarantined` 并审计；只有再次延迟确认未被引用后才 unlink。已过期但没有文件的 writing 记录也由 platform task store 按审计后的清理流程回收。

committed snapshot 即使不是 current，也只能按明确 retention policy 清理。对被 `result_ref` 引用的 snapshot，必须在所有关联 `task_deliveries` 进入 `acked` 或 `dead_letter` 后才开始结果保留计时；`acked_at` 取 BotTransportAdapter/carrier 接受交付的控制面时间，`terminal_at` 取 deadline dead-letter 的控制面时间，令 `delivery_terminal_at = max(COALESCE(acked_at, terminal_at))`，`TASK_RESULT_RETENTION` 从该时间起算，snapshot 的最早清理时间为 `max(普通 snapshot retention 截止时间, delivery_terminal_at + TASK_RESULT_RETENTION)`。delivery 未终态、引用关系不明或时间字段缺失时禁止清理；不能按“未被 current 指针引用”判为 orphan。

### 8.2 状态上限与可观测性

Worker 必须对下列值设置部署配置上限，具体数值由目标主机压测决定：`MAX_BACKEND_HISTORY_BYTES`、`MAX_WORKING_BYTES`、`MAX_DISPLAY_HISTORY_BYTES`、`MAX_TASK_OUTPUT_BYTES`、`MAX_TOOL_STDOUT_BYTES`、`MAX_SNAPSHOT_BYTES`。上限应在运行时和 checkpoint 前共同执行；超限时保留最后一个成功快照、停止继续累积数据并向用户报告原因。

每个 Worker、任务和控制面服务必须记录结构化日志、trace ID、session/task ID、资源用量和策略拒绝原因。至少观测 cgroup `memory.current`/`memory.peak`、`memory.high`/`memory.max` 触发、swap current/max、OOM 事件、启动预留、CPU、PID、磁盘、队列长度、冷启动耗时、checkpoint 耗时、pending delivery/重试/dead-letter 数量和 LLM token 用量。日志与审计输出必须配置轮转、保留期和磁盘告警；不得让单个 session 或单个模型响应无限增长日志文件。
同时记录 `runtime_settings.version`、当前 running/queued 数量、队列年龄 P95、调度唤醒延迟、配置导致的拒绝原因和管理员调节事件。
`DELIVERY_RETRY_WINDOW` 从 `tasks.terminal_at` 起算，超过后未确认 delivery 进入 `dead_letter` 并产生可见错误和审计；`TASK_RESULT_RETENTION` 与普通 snapshot retention 均为显式部署配置，不在架构中写死数值。恢复扫描必须把 `sending` 且 `attempt_lease_until` 已过期的 delivery 原子退回 `pending`，不得留下永久卡住的交付。
### 8.3 Worker 生命周期

Worker 在 task 完成并写入 checkpoint 后进入 idle 状态，保留一段可配置的 idle 宽限期（`WORKER_IDLE_TIMEOUT`）。同 session 新 task 复用前，Worker Manager 必须在 per-session 锁或等效 CAS 下将 Worker 从 `idle` 原子转为 `assigned`；宽限期满或容量压力淘汰则先将其从 `idle` 原子转为 `evicting`。两种转移互斥，不能同时领取和销毁同一 Worker。

`MAX_ACTIVE_WORKERS` 同时计入 idle 与 running 状态；idle Worker 不占用模型并发额度，但占用容器名额和内存预算。容量压力下只有成功取得 `idle→evicting` 转移的 Worker 才能执行 `Shutdown(reason=capacity_pressure)` 并销毁，不重复 checkpoint；queued task 若发现 Worker 已进入 `evicting`，必须从最近的已提交快照重新 `StartSession`，不能向正在淘汰的 Worker 投递。所有任务队列仍必须有界；预算不足时向用户返回排队、配额不足或资源不足的明确状态。

### 8.4 `/new` 快照行为

`/new` 仅清空内存中的 `history` 与 `working`，不生成额外快照，不删除工作区文件。若 `/new` 后立即崩溃，恢复到 `/new` 前最后一个成功 task 的已提交快照；用户可再次发送 `/new` 清空。该设计不引入清理前快照的额外存储路径。

---

## 9. P0 验收矩阵

| 类别 | 必须证明的行为 |
|---|---|
| 身份 | 未绑定或 pending 用户不能创建 task；bot 与 `from_user_id` 不匹配时拒绝 |
| 工作区 | Worker A 无法读取 Worker B、团队或宿主机文件；无容器 socket 与平台凭证 |
| 状态 | personal/team history、working、输出和 snapshot 不串；团队成员人设按 task 生效 |
| 任务 | 同一 session 串行且有界；`/stop` 不误伤其他请求者；取消能在宽限期后终止 Worker |
| 恢复 | 完成任务的快照恢复完整；崩溃中的任务标记 interrupted，不被自动重放 |
| 传输 | 多 bot 的 token/cursor 相互独立；token 过期不会清除其他 bot；消息按 bot/message ID 去重 |
| 结果交付 | `task_deliveries` 按 `(task_id, delivery_type)` 唯一；成功/失败/取消/中断终态均有稳定 delivery_id，pending 或未确认时重复读取 durable payload 重试；历史 chunk 不重放，carrier 不支持幂等时的 at-least-once 风险可见且不触发 task 重跑 |
| Worker 生命周期竞态 | idle→assigned 与 idle→evicting 在 Worker Manager 锁/CAS 下互斥；淘汰胜出时 queued task 从最后已提交 snapshot 重建，不向 evicting Worker 投递 |
| 运维 | 资源上限、队列满、上游限流、LLM Proxy 重启、磁盘不足、日志轮转和 Worker 崩溃都产生可见、可审计错误 |
| 容量 | 准入同时使用 cgroup `memory.current`、starting/task 预留与主机预算；指标缺失时保守回退；idle 压力淘汰不重复 checkpoint，也不等待宿主机 OOM |
| 快照一致性 | 覆盖 snapshot lease 的控制面续租、文件 rename、目录持久化、数据库提交、quarantine、结果保留期、延迟删除和 task_complete/task_failed/task_cancelled 丢失；恢复使用最后一个已提交 snapshot |
| 调度 | 管理员调高/调低/暂停并发后，任务领取遵守新上限；降低不强杀运行中 task，调高会唤醒 queued task |
| 队列 | PostgreSQL 重启或 platform 重启后 queued task 不丢失、不重复领取；LISTEN/NOTIFY 丢失时周期扫描仍能恢复 |

中继（`@username` 文字中继）属 P1 范围，其验收见 PRD §9 P1 第 18 条，不进入 P0 矩阵。

---

## 10. 实现前门槛

实现计划必须先列出并验证：

1. Linux 主机支持 rootless Podman、user namespace、cgroup、seccomp 与所需文件系统挂载策略；确认 rootless storage、overlayfs/fuse-overlayfs、`memory.current/high/max`、cgroup v2 `memory.swap.max` 或 v1 等效 memsw 策略、临时目录和工作区目录均按预期生效。
2. P0 物理部署单元完成：`platform`、`worker-manager`、`llm-proxy` 与 PostgreSQL 的启动依赖、健康检查、失败自动重启和资源边界明确；不把逻辑组件数量当作进程数量。
3. Worker 固定镜像与工具 policy 草案完成；记录镜像压缩/展开后的磁盘占用、rootless graphroot 可用空间和冷启动观测方法。
4. PostgreSQL schema、迁移、加密密钥轮换、备份恢复演练、连接池上限和目标主机资源配置草案完成；具体数据库参数不得脱离压测直接复制。
5. BotTransportAdapter 对当前 iLink 客户端的 stop、token 更新、cursor 和绑定身份适配。
6. Worker RPC、platform Checkpoint Coordinator 与 Workspace Store 文件动作边界、platform task store 的单 owner checkpoint 事务、LLM Proxy session capability、终态 delivery outbox、审计字段及端到端隔离测试完成；Proxy 故障、重启和上游限流的失败语义明确。
7. 隔离、绑定、`/stop`、blocked 取消、崩溃恢复、代理鉴权、队列满和资源不足的验收用例进入 implementation plan。
8. 明确并实现队列、history、working、display history、task output、tool stdout、snapshot、CPU、内存、PID、磁盘和模型 token 的硬限制；限制必须在数据累积过程中生效。
9. 容量 spike 在目标 2 vCPU / 4 GiB RAM 主机执行，至少记录控制面/PostgreSQL 基线、系统与 page cache 预留、Worker 冷启动 p50/p95、cgroup memory current/peak、starting/task 预留、1..N 并发下的 P95 延迟、checkpoint/fsync 延迟、CPU/PID/磁盘、swap、delivery retry/dead-letter 和 OOM 行为，并据此派生 worker_memory_budget。
10. 资源公平调度压测完成：验证 per-session FIFO、跨租户配额、idle 压力淘汰、idle claim/evict 互锁、队列上限、LLM 并发上限和恢复后的重新准入。
11. 快照恢复演练覆盖 workspace_snapshots writing/committed/quarantined、控制面 lease 续租/到期、grace 判定、quarantine 后延迟删除、文件 rename 后数据库提交前、数据库提交后事件发送前、task_deliveries 恢复/去重/dead-letter、成功/失败/取消/中断终态补发、chunk 不重放、carrier 确认前崩溃、Worker OOM、磁盘不足和 schema/checksum 不兼容；日志/审计轮转与磁盘告警同时验证。
12. 压测与容量/运维演练后确定真实 `MAX_RUNNING_TASKS`、`MAX_LLM_INFLIGHT`、`PER_TENANT_RUNNING_LIMIT`、`PER_TENANT_QUEUE_LIMIT`、`MAX_ACTIVE_WORKERS`、`WORKER_IDLE_TIMEOUT`、`DELIVERY_RETRY_WINDOW`、`TASK_RESULT_RETENTION`、CPU/内存/PID/磁盘配额和状态字节上限；未测得或未定义的数字不得写入对外承诺。
13. 受保护的管理员调度配置接口完成：范围校验、`MAX_RUNNING_TASKS` 更新、暂停/drain、版本审计和配置生效语义明确。
14. PostgreSQL-backed scheduler 完成：任务事务入队、同 session FIFO、跨租户公平、单 task 领取、队列上限、`LISTEN/NOTIFY` 唤醒和周期扫描兜底均有验收用例。
15. 运行时调高、调低、设为 0、platform 重启、PostgreSQL 重启和 scheduler 重启的行为完成压测与恢复演练。

**只有上述门槛、PRD 和修订清单一致时，才开始 implementation plan。**
