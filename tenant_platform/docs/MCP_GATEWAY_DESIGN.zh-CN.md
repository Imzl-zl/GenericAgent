# MCP Gateway 设计：stdio/HTTP 统一 transport 网关

> 状态：阶段 1 已实施（2025-08，真实环境验证通过）。目标：管理员注册 MCP Server
> （http 或 stdio/uvx 型），全租户可用，架构统一、不堆补丁。本文是 `mcp_servers` /
> WorkerMCPProxy / worker `_platform_mcp` 快照链路的演进真值。
>
> **阶段 1 验证记录（真实服务器）**：migration 0049 已应用；管理员注册并启用
> stdio server（`/opt/mcp-tools/mcp-pandoc`）；mcp-gateway 容器 healthy；
> initialize/tools/list/tools/call 全链路真实转换验证通过（md→html）；未知
> server 404 / 未初始化调用 400（fail-closed）；platform→gateway 内网连通。

## 1. 背景与问题

现状 `mcp_servers` 只支持 http/https URL 的 MCP Server（Streamable HTTP），
worker 经 Platform 的 WorkerMCPProxy（capability JWT + 预算计量）访问。管理员
只能注册"一个 URL"，无法注册 `uvx mcp-pandoc`、`npx xxx` 这类 **stdio 型**
MCP Server——它们不是 URL，是本地进程。

三个硬约束决定 stdio 不能塞进现有链路：

1. **数据模型**：`MCPServerCreate.URL` 是唯一接入点，domain 校验强制
   `http/https` 绝对 URL，表达不了"进程"。
2. **worker 客户端**：`MCPHTTPClient` 只会 POST JSON-RPC 到 URL，不认识
   stdin/stdout。
3. **Runner 不可信**：无公网出口、不持凭据、动态销毁。stdio 进程的宿主
   （spawn/监督/凭据注入）绝不能放 Runner 内——`uvx` 首次拉包需出网，
   带凭据工具进 Runner 违背"Runner 不持凭据"安全基线。

## 2. 核心决定

1. **新增 mcp-gateway 常驻服务**（可信区，与 sandbox-manager 同层级）：
   唯一的 transport 适配层。stdio 与 http 的差异全部收敛在 gateway 一处，
   worker 客户端与 MCP 协议层零改动。
2. **控制面与数据面分离**：
   - Platform（控制面）：管理员注册 server 定义（transport 无关的意图声明）、
     capability 签发、预算计量、启用/禁用白名单。
   - mcp-gateway（数据面）：transport 适配、stdio 进程宿主（生命周期/队列/
     隔离）、http 反代。
3. **数据模型向后兼容**：`mcp_servers` 加 `transport` 字段，默认 `http`，
   现有行零迁移；worker 看到的快照仍只有 URL（指向 gateway），
   transport 细节不出平台。
4. **租户语义不变且全局化**：管理员注册 → 全租户任务可用（沿用
   `install_global_mcp_tools` 现状）。隔离维度由 server 定义的
   `isolation` 决定（shared 共享进程 / workspace 每工作区进程）。
5. **stdio 子进程离线运行**：工具集预装在 gateway 镜像（uv 离线缓存 /
   node 全局包），子进程无网络、无凭据、无持久盘。v1 只支持无状态
   无凭据工具（如 pandoc contents 转换）。

## 3. 目标架构

```text
GA Runner (每租户工作区, 不可信, 无公网出口)
  └─ worker MCPHTTPClient (Streamable HTTP, 零改动)
       │  POST /v1/worker/mcp/{server_id}  (capability JWT + 预算)
       ▼
Platform WorkerMCPProxy (鉴权 + JTI 预算计量 + 内部转发, 逻辑不变)
       │  只转发 MCP 语义头 + X-MCP-Workspace (内部头, 不达第三方)
       ▼
mcp-gateway (新常驻服务, 可信区, internal 网络)
       ├─ transport=http   → 反代到第三方 URL (现有行为, 语义不变)
       └─ transport=stdio  → 受管子进程 (uvx/npx) ←→ Streamable HTTP 桥
                              ├─ shared    : 每 server 进程池, 请求串行队列
                              └─ workspace : 每 (server, workspace_key) 进程
```

关键性质：

- **白名单双保险**：proxy 查 DB 确认 server enabled（现状）；gateway 只接受
  DB 中存在的 server_id，未知一律 404。proxy 不再持有第三方 URL（resolve
  返回 gateway 内部地址），第三方地址只在 gateway 内存/DB 中。
- **会话**：gateway 为 worker 的每个 MCP 会话分配 gateway 层 session id；
  shared 模式下多会话请求经 per-process 串行队列复用进程。
- **workspace 路由**：proxy 鉴权后从 capability claims 取 `SessionKey`
  （如 `personal:42`），以内部头 `X-MCP-Workspace` 传给 gateway，用于
  workspace 隔离路由（头绝不出平台）。

## 4. 数据模型演进（migration 0030）

`mcp_servers` 新增列：

| 列 | 类型/默认 | 说明 |
| --- | --- | --- |
| `transport` | TEXT NOT NULL DEFAULT 'http' | `http` \| `stdio` |
| `command` | TEXT NULL | stdio 必填：白名单绝对路径（镜像预装工具集内） |
| `args` | JSONB NULL | stdio：如 `["mcp-pandoc"]` |
| `isolation` | TEXT NOT NULL DEFAULT 'shared' | `shared` \| `workspace` |
| `max_instances` | INT NOT NULL DEFAULT 1 | stdio 进程数上限（shared 池 / workspace 每工作区上限） |

约束（domain 层 `ValidateMCPServerInput`）：

- `http`：url 必填（现有校验不变）；command/args 必须为空。
- `stdio`：url 允许为空；command 必须非空且命中白名单前缀（v1 固定为
  `/opt/mcp-tools/` 下绝对路径）；args 必须为字符串数组且元素非空。
- v1 仅允许 `isolation=shared` + 无 env（无凭据工具）；`workspace` 与
  `env` 字段预留在迁移中，domain 校验先拒绝（fail-closed），后续阶段放开。

快照与 worker：`_platform_mcp.servers[].url` 统一改写为
`{GATEWAY_BASE}/v1/mcp/{server_id}`，`server_id` 保持不变；worker 无需
感知 transport。gateway base URL 由 Platform 配置注入 scheduler
（`MCPGatewayBaseURL`，与现有 `MCPProxyBaseURL` 同模式）。

## 5. mcp-gateway 服务设计

### 5.1 部署形态

- `cmd/mcp-gateway`，Go 实现（与 backend 同仓同语言，复用 llmproxy 校验
  与审查基线），独立镜像，与 sandbox-manager 同层级常驻。
- 网络：`runner-control`（internal，接收 proxy 转发）+ `database`
  （internal，只读 mcp_servers 表，只读角色）。**不接入任何 egress 网络**。
- 容器：read_only + cap_drop ALL + no-new-privileges + 非 root +
  pids/mem/cpu 限制（照抄现有服务模式）。

### 5.2 stdio 进程宿主

每个 stdio server 的进程由 gateway 全权管理：

- **spawn**：按 `(server_id, workspace_key?)` 路由；启动超时（默认 30s）；
  首次 initialize 必须在该窗口内完成，否则视为失败。
- **生命周期**：崩溃指数退避 + 熔断（连续 N 次失败停用并告警，管理员可
  重新 enable 复位）；空闲 TTL 回收（SIGTERM → 5s → SIGKILL）；
  配置 revision 变化 → 排空旧进程滚动重建。
- **串行化**：stdio 单通道，per-process 互斥 + 请求队列；排队超时 =
  `timeout_seconds`；`max_instances` 限制并发进程数。
- **资源**：子进程以 gateway 内受限 UID 运行（no-new-privileges）、
  工作目录为 tmpfs 空目录、rlimit（文件大小/进程数）继承容器限制。

### 5.3 Streamable HTTP 桥

- 实现 MCP Streamable HTTP 服务端语义（2025-06-18）：`initialize` 建会话
  返回 `Mcp-Session-Id`，后续请求携带该头；`notifications/*` 透传；
  `tools/list` / `tools/call` 转发到进程 stdin，读 stdout 响应回传。
- session → 进程映射：shared 模式多 session 复用进程（请求入队）；
  workspace 模式 session 固定到该工作区的进程。
- 请求体量上限（防内存放大）、响应体量上限、超时对齐
  `timeout_seconds`；超出返回 MCP 错误帧而非杀死进程。

### 5.4 观测

- 健康端点 `/healthz`；指标：进程数、队列深度、请求/失败计数、崩溃次数；
- 子进程 stdout/stderr 捕获进滚动日志（截断，防刷屏）；
- 所有错误带 trace id，对齐平台日志风格。

## 6. 安全边界（沿用审查基线）

| 威胁 | 对策 |
| --- | --- |
| 管理员注册任意命令 → RCE | 命令白名单：仅 `/opt/mcp-tools/` 下镜像预装二进制；工具集变更 = 镜像变更（与 Runner 镜像能力同哲学） |
| 子进程出网（数据外泄/拉包） | gateway 无 egress 网络，子进程继承无网；uv/npm 缓存构建期预装 |
| 子进程读租户数据 | 工作目录 tmpfs 空目录；不挂载 workspace/config 卷 |
| 凭据泄漏 | v1 仅无凭据工具（shared 无 env）；env/workspace 隔离留待 secret 管理就绪 |
| 未知 server_id 直连 | 白名单双保险（proxy + gateway 各自查 DB） |
| 请求放大（内存/CPU） | 体量上限 + 串行队列 + max_instances + 容器级 pids/mem 限制 |

## 7. 与现有代码的变更清单

| 文件 | 变更 |
| --- | --- |
| `postgres/migrations/0030_mcp_gateway.sql`（新） | 新增列 + 约束 |
| `domain/mcp_server.go` | 按 transport 分支校验；新增字段 |
| `postgres/mcp_server_store.go` | 列读写扩展 |
| `api/admin.go` + admin 相关 | create/update 透传新字段并校验 |
| `application/scheduler.go` | 快照 url 改写为 gateway 地址；`MCPGatewayBaseURL` 配置 |
| `api/worker_mcp_proxy.go` | resolve 返回 gateway 内部地址（含 server_id）；注入 `X-MCP-Workspace` 内部头（基于 claims.SessionKey） |
| `cmd/mcp-gateway`（新）+ `internal/mcpgateway/` | 进程宿主 + 桥 + 队列 + 指标 |
| `infra/compose/compose.yaml` + `mcp-gateway.Dockerfile`（新） | 常驻服务 + 镜像（预装 uv/离线缓存） |
| worker-python | **零改动** |

## 8. 演进阶段

- **阶段 0（现状）**：http-only。
- **阶段 1（已实施 ✅）**：gateway 上线，stdio transport 支持（shared/无凭据/无文件），
  http 仍直连（proxy resolve 保持现状，仅新增 gateway 路由分支）。
- **阶段 2**：http 统一迁入 gateway（反代分支），所有 MCP 流量单一入口；
  此时 resolve 不再持有第三方 URL。
- **阶段 3**：workspace 隔离 + env 凭据（secret 管理就绪后）；
  受控文件交换（上传→转换→下载的文件区，transport 与数据面解耦）。

阶段划分保证每步可独立验证、可回退；阶段 2 是纯内部重构（无用户可见
变化），阶段 3 是能力增量。

## 9. 验证门

- Go：`go vet ./...`、`go build ./...`、`go test -p 1 -count=1 -timeout 300s ./...`
- 契约/安全/smoke：`python -m pytest tenant_platform/tests/contract tenant_platform/tests/security tenant_platform/tests/smoke -q`
- gateway 专项（真实 Linux + Docker）：stdio 冒烟（mock stdio server：
  initialize/tools/list/tools/call 全链路）、崩溃退避、并发队列、
  无网验证（子进程网络隔离）、白名单拒绝未知 server_id。
- 集成：真实 Postgres + platform 子进程 + Worker，管理员注册 http 与
  stdio server 各一，租户任务可调用（`TEST_DATABASE_URL` 缺失显式失败）。
