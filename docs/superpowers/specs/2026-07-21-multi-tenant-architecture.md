# 多租户 IM 个人助手平台 — 架构设计

**日期：** 2026-07-22  
**状态：** 设计已确认；未进入实现  
**对应 PRD：** [2026-07-21-multi-tenant-im-platform-design.md](2026-07-21-multi-tenant-im-platform-design.md)  
**试运行目标：** 一台 Linux 服务器，先服务约 10–20 名已批准用户；容量由部署前压测结果决定，不作未经验证的并发承诺。

---

## 1. 目标与边界

平台让每位用户通过 Web 注册、扫码绑定自己的微信 bot，在私人或团队工作区中使用 GenericAgent。平台必须允许受控的 Shell/Python 文档处理，同时保证任一租户不能访问宿主机、平台密钥、其他租户的文件、状态或输出。

### 1.1 P0 目标

1. 个人模式闭环：注册、人工批准、微信绑定、私人对话、文件处理、`/stop`、`/new`。
2. 每个个人或团队工作区具有可证明的执行隔离，而不是仅靠路径过滤。
3. Shell/Python 仅在会话隔离 Worker 内运行。
4. 每个完成任务都有原子持久化快照；崩溃不会重放可能已执行 Shell 的任务。
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
| 状态持久化 | 每个成功完成 task 原子 checkpoint；运行中任务崩溃后标记 `interrupted`，禁止自动重放 |
| LLM Key | 真实 Key 只存在于控制面 LLM Proxy；Worker 只持有短时、单 session、可撤销凭证 |
| 工具 | Shell/Python 可用，但仅在 Worker；默认无任意外网、无宿主机挂载、无容器管理 socket |
| 团队人设 | `team_members.persona_id → teams.team_persona_id → platform default` |
| 团队 key_info | 团队会话内共享；PRD 必须明示该隐私边界 |
| 中继 | 仅整条消息为 `@username` 时发起；不调用 AI；中继和团队邀请使用带短 ID 的确定性状态机 |

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
| Workspace Store | 每 session 工作区与快照 | 不由 Gateway 直接暴露为宿主机路径 |

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

控制面必须分别限制每 user/team 的排队长度、单任务时长、CPU、内存、PID、磁盘、输出字节和模型 token；同时限制全局活跃 Worker 与 LLM 并发。调度采用 per-session FIFO 与跨租户公平配额，避免单一用户占满 Worker 或上游模型额度。所有具体数值均由压测写入部署配置；未测得前不得写死为产品承诺。

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

Worker 事件只允许一个控制面消费者读取：`chunk`、`tool_progress`、`task_complete`、`task_failed`、`task_cancelled`。checkpoint 由 Worker 在产生最终完成事件前执行，绝不通过第二个消费者抢读流式 Queue。

### 4.3 GA 适配边界

GenericAgent 仍是 Worker 内的任务引擎；平台不直接读写 `handler`、`history`、`task_queue`、`extra_sys_prompts` 或 LLM backend 私有字段。Worker 内必须提供明确、可版本化的 `ManagedAgent` 适配层。

`ManagedAgent` 必须保证：

1. `work_dir` 覆盖所有租户可写路径：长 prompt、task 文件、模型日志、历史、working、临时文件和 peer hint；静态 assets 只读。
2. 每个 task 的 persona 进入 task envelope，Worker 消费该 task 时设置；禁止会话级 `set_persona` 造成排队 race。
3. Worker 在新 handler 创建后，先从 session snapshot 注入完整 working，再运行 task；任务结束后将完整 working 写回 runtime snapshot。
4. snapshot 仅在任务完全静止的边界生成，包含 `backend_history`、`working`、`display_history` 与 schema version。
5. 工具 schema 是 runtime policy 的子集。P0 只保留会话内文件、Shell/Python 与文档解析所需工具；宿主机桌面、全局浏览器、容器管理和任意网络工具默认不可用。
6. `shutdown` 先请求协作取消；超过宽限期由 Worker Manager 销毁容器。不能把 `abort()` 描述为对任意阻塞调用的即时终止保证。

---

## 5. 数据模型与不变量

P0 使用 PostgreSQL。每个状态变更在事务中完成，并附加审计事件；业务秘密为应用层字段加密，带 `key_version`。

| 实体 | 关键字段与不变量 |
|---|---|
| `users` | `username` 规范化后唯一；`status ∈ pending, approved, blocked`；密码使用强 KDF 哈希；blocked 立即撤销会话和调度权 |
| `auth_sessions` | 仅存哈希后的 session/refresh token；到期、撤销和设备审计 |
| `binding_attempts` | 绑定码哈希、平台 user、状态、过期时间；一次性 `/activate` 完成微信身份配对 |
| `bots` | `owner_id` 唯一；`bot_uuid` 唯一；`ilink_user_id` 在激活后绑定；token 密文、状态、密钥版本 |
| `bot_transport_state` | 每 bot 独立加密 update cursor、重连状态、错误时间；禁止共享本地 token 文件 |
| `context_tokens` | `(bot_id, ilink_user_id)` 唯一；加密、到期、最后使用时间；视为能力凭证 |
| `workspaces` | session_key 唯一；personal 与 team 的归属列互斥；只保存受控 volume ID 和 snapshot 元数据，不保存任意宿主机路径 |
| `teams` | owner、名称、默认人设、状态 |
| `team_members` | `(team_id,user_id)` 唯一；`status`、role、nullable `persona_id`；踢人/封禁由路由每条消息复核 |
| `active_contexts` | personal 时 `team_id IS NULL`；team 时 `team_id IS NOT NULL`，且成员关系必须 approved |
| `tasks` | task_id、session、requester、message idempotency key、状态、顺序号；每 session 同时最多一个 running task |
| `task_events` | 状态转换、Worker ID、错误码、时间；不存模型 Key 或完整敏感 prompt |
| `relay_sessions` | 发起人/接收人、状态、过期时间、短 ID；每参与者同时至多一个 pending/active relay，由事务约束保证 |
| `relay_blocks` | blocker、blocked、创建时间 |
| `audit_events` | 登录、审批、绑定、策略拒绝、密钥访问、任务生命周期和管理操作；默认不记录中继正文 |

### 5.1 绑定状态机

```text
requested → qr_pending → awaiting_activation → active
                        ├→ expired
                        └→ revoked
```

Web 创建 `BindingAttempt` 后展示二维码。二维码确认仅证明获得 bot token，不假定回包可靠含有用户 iLink ID。用户必须从该 bot 发送一次性 `/activate <code>`；Gateway 以 `bot_uuid + from_user_id + code hash` 原子绑定 `ilink_user_id`。之后每条消息都必须同时匹配 bot 与绑定的 `from_user_id`。

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
| 1 | 绑定激活 | `/activate <one-time-code>` 仅在 `awaiting_activation` 使用 |
| 2 | 活跃 relay | `/断开` 结束；其它非平台保留命令转发；不进入 AI |
| 3 | pending relay | `/同意 r-123`、`/拒绝 r-123`；仅一个候选时可省短 ID |
| 4 | pending team 流程 | `/同意 t-456`（成员接受直接邀请）、`/批准 t-456`（Owner 批准成员加入）、`/拒绝 t-456`（任一方拒绝）；Router 按发送者角色与邀请当前状态分发；仅一个候选时可省短 ID |
| 5 | session 命令 | `/个人`、`/团队`、`/new`、`/stop`、`/邀请码 <code>`、`/移除 @username`（Owner）、`/我的身份`、`/状态` |
| 6 | relay 发起 | 整条消息严格等于 `@username` |
| 7 | 普通消息 | 创建 task 并入队 |

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

Worker 是唯一读取 GA runtime state 的组件。每个成功 task 的顺序为：

```text
Agent 状态稳定
→ Worker 深拷贝 backend_history / working / display_history
→ 写入 workspace 临时快照
→ fsync + 原子 replace
→ 更新 snapshot metadata 与 task=succeeded 的事务
→ 发送 task_complete
```

因此控制面不会从 display queue 推测 checkpoint，也不会在 handler 尚未创建时写入 working。快照 schema 不兼容时，恢复必须失败为可见错误，不允许静默丢字段。

每个 Worker、任务和控制面服务必须记录结构化日志、trace ID、session/task ID、资源用量和策略拒绝原因。日志默认脱敏；秘密、完整 token、原始中继正文和未授权用户内容不可写入审计日志。

容量由部署配置和压测控制：`MAX_ACTIVE_WORKERS`、每 session queue 上限、CPU/内存/PID/磁盘配额均须在上线前测得并设置。

**Worker 生命周期与 idle timeout：** Worker 在 task 完成并写入 checkpoint 后进入 idle 状态，保留一段可配置的 idle 宽限期（`WORKER_IDLE_TIMEOUT`，由部署配置）。宽限期内收到同 session 的新 task 直接复用该 Worker，避免冷启动；宽限期满后 Worker Manager 执行 `Shutdown(reason=idle_timeout)`，先请求协作取消，超过宽限期销毁容器。下一条 task 从快照恢复重建 Worker。`MAX_ACTIVE_WORKERS` 同时计入 idle 与 running 状态的 Worker；idle Worker 不占用模型并发额度，但占用容器名额，调度器在名额不足时优先销毁最早进入 idle 的 Worker。

**`/new` 快照行为：** `/new` 仅清空内存中的 `history` 与 `working`，不生成额外快照，不删除工作区文件。若 `/new` 后立即崩溃，恢复到 `/new` 前最后一个成功 task 的快照；用户可再次发送 `/new` 清空。该设计保证不丢数据且实现最简，不引入"清理前快照"的额外存储路径。

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
| 代理 | Worker 拿不到真实 Key；Proxy 拒绝过期、跨 session 或超配额请求 |
| 运维 | 资源上限、队列满、上游限流和 Worker 崩溃都产生可见、可审计错误 |

中继（`@username` 文字中继）属 P1 范围，其验收见 PRD §9 P1 第 12 条，不进入 P0 矩阵。

---

## 10. 实现前门槛

实现计划必须先列出并验证：

1. Linux 主机支持 rootless Podman、user namespace、cgroup、seccomp 与所需文件系统挂载策略。
2. Worker 固定镜像与工具 policy；Shell/Python 以外的危险工具默认禁用。
3. PostgreSQL schema、迁移、加密密钥轮换和备份恢复演练。
4. BotTransportAdapter 对当前 iLink 客户端的 stop、token 更新、cursor 和绑定身份适配。
5. Worker RPC、LLM Proxy session capability、审计字段及端到端隔离测试。
6. 隔离、绑定、`/stop`、blocked 取消、崩溃恢复和代理鉴权的验收用例进入 implementation plan。
7. capability policy 草案完成：默认拒绝清单、不可授权边界、高风险授权字段（授予者、理由、到期时间、策略版本）、源文件变更审批流程和审计事件 schema。
8. P0 资源配额、跨租户公平调度策略和告警指标定义完成；具体数值待压测填入。
9. 压测后确定真实容量配置（`MAX_ACTIVE_WORKERS`、`WORKER_IDLE_TIMEOUT`、每 session queue 上限、CPU/内存/PID/磁盘配额）；未测得的数字不得写入对外承诺。

**只有上述门槛、PRD 和修订清单一致时，才开始 implementation plan。**
