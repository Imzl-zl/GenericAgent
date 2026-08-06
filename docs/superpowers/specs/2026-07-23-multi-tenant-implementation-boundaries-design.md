# GenericAgent 多租户平台实施边界设计
> [!IMPORTANT] 历史设计文档（2026-07 实施期产物，非当前真值）
> 其中部分命名/设计已随后续重构变更（如 DevToken→AdminToken、
> PER_TENANT→PER_REQUESTER、工具分级→静态 policy manifest、
> 常驻 Worker→任务即进程）。当前设计真值以
> `tenant_platform/docs/` 与 `tenant_platform/contracts/` 为准。


**日期：** 2026-07-23  
**状态：** 方案已确认，待写 implementation plan  
**目标：** 在不搬迁现有 GenericAgent 源码的前提下，建立 Go 控制面、Python Worker 和 React Web 的独立边界。

## 1. 已确认决策

1. 控制面使用 Go，部署为 `platform`、`worker-manager`、`llm-proxy` 三个进程。
2. 现有 Python GenericAgent 保留为 Worker 内核；第一阶段不把 `agentmain.py`、`ga.py`、`llmcore.py` 或 `frontends/` 搬入新包，也不改现有 import 路径。
3. Worker 通过版本化 RPC 暴露 `StartSession`、`ExecuteTask`、`CancelTask`、`Health`、`Shutdown` 及 checkpoint 控制；Worker 不访问 PostgreSQL。
4. `platform` 的 Task Store/Checkpoint Coordinator 是 `tasks`、`task_deliveries`、`workspace_snapshots` 的唯一 PostgreSQL 事务 owner。
5. `worker-manager` 负责 rootless Podman、Worker 生命周期和受控 volume 文件动作；不直接写 PostgreSQL。
6. Web 使用 React + TypeScript + Vite，只访问 platform 的 HTTP/WebSocket API，不复用桌面 Tauri bridge。
7. OpenAPI 描述浏览器 API；Protobuf/gRPC 描述内部 Worker、manager 和 LLM Proxy RPC。两者都必须版本化。

## 2. 现有代码边界

`agentmain.GenericAgent` 当前同时持有线程任务队列、会话 history、handler、LLM client、abort 状态和本地 runtime 路径。多个旧前端直接实例化它，不能把它当作无状态库函数重构。

现有 `frontends/` 是桌面、TUI 和各 IM 通道的适配集合；`frontends/desktop/static/app.js` 绑定本地 bridge、固定端口和桌面能力。`assets/ga_httpapp.py` 与 `frontends/conductor.py` 是单 Agent 服务，不能直接充当多租户控制面。

因此新平台只通过 Worker adapter 接入旧 runtime。旧入口继续按原方式运行，新平台的 import 不反向改变旧模块。

## 3. 目录布局

```text
GenericAgent/
├─ agentmain.py
├─ ga.py
├─ llmcore.py
├─ agent_loop.py
├─ frontends/                         # 旧桌面、TUI、IM，第一阶段不搬迁
│
├─ tenant_platform/
│  ├─ contracts/
│  │  ├─ openapi/                     # 浏览器 API 与错误/分页/事件模型
│  │  └─ proto/                       # 内部 RPC 与流式事件
│  ├─ backend-go/
│  │  ├─ go.mod
│  │  ├─ cmd/
│  │  │  ├─ platform/                 # Auth、Router、Task Store、API
│  │  │  ├─ worker-manager/           # Podman、资源准入、volume 协调
│  │  │  └─ llm-proxy/                # 唯一持有真实 LLM Key
│  │  └─ internal/
│  │     ├─ domain/                   # task/session/workspace 状态与不变量
│  │     ├─ application/              # 用例、事务边界、命令处理
│  │     ├─ api/                      # HTTP/WebSocket handlers
│  │     ├─ postgres/                 # 查询、迁移接口、事务实现
│  │     ├─ scheduler/                # FIFO、公平、准入与队列唤醒
│  │     ├─ delivery/                 # task_deliveries、重试、dead-letter
│  │     ├─ transport/                # iLink BotTransportAdapter 与传输状态
│  │     └─ config/                   # 部署配置与范围校验
│  ├─ worker-python/
│  │  ├─ pyproject.toml
│  │  ├─ src/ga_worker/
│  │  │  ├─ entrypoint.py             # 容器入口与生命周期
│  │  │  ├─ managed_agent.py          # 极薄 GenericAgent adapter
│  │  │  ├─ rpc_server.py             # Worker RPC
│  │  │  ├─ checkpoint.py             # bundle 导出/校验接口
│  │  │  └─ limits.py                 # runtime 字节/时长限制
│  │  └─ Dockerfile
│  ├─ web/
│  │  ├─ package.json
│  │  ├─ src/
│  │  │  ├─ app/                     # 路由、启动、鉴权状态
│  │  │  ├─ features/                # auth、approval、binding、chat、teams
│  │  │  ├─ api/                     # OpenAPI 生成 client 与 transport
│  │  │  └─ components/               # 跨页面基础组件
│  │  └─ public/
│  ├─ infra/
│  │  ├─ postgres/                   # migrations、备份/恢复脚本
│  │  ├─ podman/                     # 固定镜像与 policy
│  │  └─ systemd/                    # 三个服务单元与依赖
│  └─ tests/
│     ├─ contract/                   # OpenAPI/Protobuf 兼容性
│     ├─ integration/                # PostgreSQL、RPC、Podman
│     └─ e2e/                        # 注册到聊天/恢复的黑盒流程
```

`tenant_platform` 刻意不命名为顶层 `platform`，避免新 Python 工具、脚本或构建路径意外遮蔽 Python 标准库 `platform` 模块。

## 4. 进程与数据边界

### 4.1 `platform`

只处理身份、批准、绑定、路由、任务入队、PostgreSQL 事务、checkpoint 元数据和终态 delivery。它不导入 `agentmain`、`ga` 或 `llmcore`，不执行用户代码，不读取 Worker volume 文件。

### 4.2 `worker-manager`

通过本地受认证 RPC 管理 Worker 容器，执行资源准入、session Worker 状态转移、staging 文件 fsync/rename/quarantine，并把 `CommitCheckpoint` 请求交给 `platform`。它不保存真实 LLM Key，不直接连接 PostgreSQL。

### 4.3 Python Worker

容器内只运行现有 GenericAgent 和极薄 adapter。adapter 将旧的 `put_task`/display queue/`abort` 转换成版本化 RPC 事件；不在旧 runtime 内新增租户调度、数据库访问或跨 session 状态。

Worker 镜像构建初期可以在独立构建上下文中携带现有根目录 runtime 文件，但不修改这些文件的 import 契约。后续若要抽取 `legacy_core` 包，必须作为独立迁移任务，不与第一条垂直链路混做。

### 4.4 `llm-proxy`

只接受带 session capability 的请求，保存真实上游 Key，执行模型配额和 token usage 记录。Worker 只获得短时、单 session 凭证。

### 4.5 Web

浏览器只访问 `platform`。命令、状态、流式消息和错误都使用 OpenAPI/事件契约；不得读取 PostgreSQL、Worker RPC 或桌面 bridge。

## 5. 第一批垂直切片

### Slice 1：契约与 Worker loopback

- 建立 `worker.proto`、`llm_proxy.proto` 和覆盖第一条链路的最小可执行 OpenAPI contract。
- 生成 Go/Python RPC 类型，定义 task、chunk、terminal、checkpoint、error envelope。
- 新建 Python adapter，真实创建一个 `GenericAgent`，分别验证正常路径 `StartSession → ExecuteTask → chunk → task_complete` 和取消路径 `ExecuteTask → CancelTask → task_cancelled/interrupted`。
- 验收：现有 `agentmain.py`、`ga.py`、`llmcore.py` 零修改；同一 task_id 不重复执行；正常完成和取消都有唯一 terminal 状态。

### Slice 2：Go platform + PostgreSQL 单 session

- 建立 users、workspaces、tasks、task_events、task_deliveries、workspace_snapshots 的最小迁移。
- 实现一个 session 的入队、FIFO、幂等 message key、状态查询和终态 delivery outbox。
- 使用真实 Python Worker 进程的 loopback RPC 作为开发验收路径；不把内存 fake 或 Podman 假实现接入最终运行路径。
- Slice 2 仅用于本地开发验收；在 Slice 3 完成 Worker→LLM Proxy 路径前，不得形成可部署的 P0 运行配置。
- 验收：重启后 queued task 不丢失；重复消息不重复建 task；成功结果可从 result_ref 重读。

### Slice 3：Go LLM Proxy

- 实现短时 session capability、模型策略、真实 Key 注入、上游流式响应和 token usage。
- Python Worker 改为只访问 LLM Proxy，不读取真实 Key；旧本地入口仍保持原行为。
- 验收：过期、跨 session 和超配额请求被拒绝；日志和错误不泄漏 Key。

### Slice 4：真实 worker-manager/Podman

- 接入 rootless Podman、固定镜像、session volume 和 RPC 生命周期。
- 实现 memory.current/CPU/PID/磁盘准入、idle→assigned/evicting CAS、checkpoint staging 和 generation fencing。
- 在目标 2 vCPU/4 GiB 主机记录冷启动、RSS、OOM 和 fsync 数据。

### Slice 5：恢复与终态交付

- 实现 PostgreSQL 恢复扫描、sending lease 超时回退、retry window、dead-letter、quarantine 和 result retention。
- 覆盖 platform、worker-manager、Worker、PostgreSQL 分别重启的故障窗口。

### Slice 6：Go BotTransportAdapter 与绑定闭环

- 在 `backend-go/internal/transport/ilink` 实现多 bot token/cursor、长轮询、发送、媒体下载和 token 过期状态。
- 以现有 `frontends/wechatapp.py` 的可观察协议行为作为迁移参考，不复用其全局 Agent、共享 token 文件或进程级状态。
- 实现 Web QR 绑定（iLink 官方扫码 `confirmed` 时直接完成，无需 `/activate`）和 `ilink_user_id` 复核。详见 [iLink 绑定流程 SPEC](2026-07-25-ilink-official-binding-flow.md)。
- 验收：多 bot 状态互不覆盖；未批准、未绑定或身份不匹配的消息不能创建 task。

### Slice 7：React Web

- 按 OpenAPI 生成 client，实现注册、登录、人工审批、绑定、人设编辑和运行状态页面。
- P0 Web 不提供聊天入口；用户消息和终态回复走 BotTransportAdapter。Web 刷新后只从 platform 重新 hydrate 控制面状态。

## 6. 非目标

1. 第一阶段不搬迁根目录旧 runtime，不重写 GA 为 Go/Rust。
2. 不把现有 Tauri desktop UI 改造成多租户 Web 控制台。
3. 不先做完整后台、团队高级能力或性能优化；先证明一条真实 session vertical slice。
4. 不用内存队列替代 PostgreSQL task 事实来源。
5. P0 不提供 Web 聊天入口；Web 聊天仍属后续范围。

## 7. 进入 implementation plan 的门槛

implementation plan 必须给每个 Slice 写出具体文件、接口、迁移、红绿测试和 smoke command，并明确哪些步骤需要目标 Linux 主机。第一条 plan 不应包含 GA 重写；是否需要替换 Worker runtime，等真实 RSS/延迟/OOM 数据后单独决策。
