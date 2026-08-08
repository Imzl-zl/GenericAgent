# MCP Gateway 设计：stdio transport 统一网关

> 状态：已实施（2025-08，真实环境验证通过 + 重构收敛）。目标：管理员注册
> MCP Server（http 或 stdio/uvx 型），全租户可用，架构统一、不堆补丁。
> 本文是 `mcp_servers` / WorkerMCPProxy / worker `_platform_mcp` 快照链路
> 的演进真值。
>
> **真实环境验证记录**：migration 0049 已应用；管理员注册并启用 stdio
> server（`/opt/mcp-tools/mcp-pandoc`）；mcp-gateway 容器 healthy；
> initialize/tools/list/tools/call 全链路真实转换验证通过（md→html）；
> 未知 server 404（fail-closed）；platform→gateway 内网连通。
>
> **2025-08 重构收敛（架构定案）**：修复代理链路会话头丢失与 JSON-RPC id
> 错位；gateway 会话无状态化；stdio 进程池 + 指数退避 + 熔断 + 配置热更新；
> 子进程白名单环境（不继承凭据）；stdio URL 由平台单一函数合成。详见 §8。

## 1. 背景与问题

现状 `mcp_servers` 支持两类 MCP Server：

- **http/https（Streamable HTTP）**：worker 经 Platform 的 WorkerMCPProxy
  （capability JWT + 预算计量）直接反代到第三方 URL。
- **stdio（uvx/npx 启动的本地进程）**：不是 URL，是进程。三个硬约束决定
  stdio 不能塞进 http 链路：
  1. **数据模型**：url 是 http 的唯一接入点，表达不了"进程"。
  2. **worker 客户端**：`MCPHTTPClient` 只会 POST JSON-RPC 到 URL。
  3. **Runner 不可信**：无公网出口、不持凭据、动态销毁。stdio 进程的宿主
     （spawn/监督/凭据注入）绝不能放 Runner 内——`uvx` 首次拉包需出网，
     带凭据工具进 Runner 违背"Runner 不持凭据"安全基线。

## 2. 核心决定

1. **http 保持 Platform proxy 直连；stdio 由新增 mcp-gateway 常驻服务托管**
   （可信区，与 sandbox-manager 同层级）。不做"http 统一迁入 gateway"：
   http 反代需出网，而 stdio 子进程必须无网——两者在同一容器内矛盾；
   拆双容器（带 egress 反代容器 + 无网宿主容器）是负收益重构，除非未来
   需要统一审计点再议。
2. **控制面与数据面分离**：
   - Platform（控制面）：管理员注册 server 定义（transport 无关的意图
     声明）、capability 签发、预算计量、启用/禁用白名单。
   - mcp-gateway（数据面，stdio 专用）：transport 适配、stdio 进程宿主
     （生命周期/队列/隔离）。白名单兜底：gateway 只接受 DB 中启用的
     server_id，未知一律 404。
3. **数据模型向后兼容**：`mcp_servers` 加 `transport` 字段，默认 `http`，
   现有行零迁移；worker 看到的快照仍只有 URL，transport 细节不出平台。
4. **租户语义不变且全局化**：管理员注册 → 全租户任务可用（沿用
   `install_global_mcp_tools` 现状）。隔离维度由 `isolation` 决定
   （v1 仅 shared；workspace 预留在迁移中，domain 校验先拒绝）。
5. **stdio 子进程离线运行**：工具集预装在 gateway 镜像（uv 离线缓存 /
   node 全局包），子进程无网络、无凭据、无持久盘。v1 只支持无状态
   无凭据工具（如 pandoc 内容转换）。

## 3. 目标架构

```text
GA Runner (每租户工作区, 不可信, 无公网出口)
  └─ worker MCPHTTPClient (Streamable HTTP, 零改动)
       │  POST /v1/worker/mcp/{server_id}  (capability JWT + 预算)
       ▼
Platform WorkerMCPProxy (鉴权 + JTI 预算计量 + 白名单)
       ├─ transport=http   → 反代到 DB 第三方 URL (仅 MCP 语义头, 身份头剥离)
       └─ transport=stdio  → 转发 {gatewayBase}/v1/mcp/{server_id}
                              (URL 由 domain.MCPServerGatewayURL 合成,
                              附内部头 X-MCP-Workspace, 只发自有 gateway)
                              ▼
                    mcp-gateway (常驻, 可信区, 仅 internal 网络)
                              └─ 受管子进程池 (≤max_instances)
                                 ├─ shared: 无状态工具跨租户共享进程
                                 └─ workspace: 预留 (阶段 3)
```

关键性质：

- **白名单双保险**：proxy 查 DB 确认 server enabled（现状）；gateway 只接受
  DB 中存在的 server_id，未知一律 404。
- **stdio URL 单一合成**：`domain.MCPServerGatewayURL(base, serverKey)` 是
  唯一实现（快照下发与 proxy resolve 共用）。管理员注册 stdio server 时
  url 必须为空——不感知、也不应感知 gateway 内部地址。
- **proxy 双超时**：第三方直连保留 30s 响应头保护（挂死服务器快速失败）；
  gateway 路由放宽到 MCP 超时上限 + 缓冲（stdio 调用由 gateway 按
  `timeout_seconds` 执行超时，最长 300s，proxy 不得截断）。
- **会话无状态**：gateway 不维护 MCP 会话表。进程的会话状态在子进程
  内存中；gateway 重启/进程重建不破坏任何客户端会话；worker 无需重握手。

## 4. 数据模型演进（migration 0049）

`mcp_servers` 新增列：

| 列 | 类型/默认 | 说明 |
| --- | --- | --- |
| `transport` | TEXT NOT NULL DEFAULT 'http' | `http` \| `stdio` |
| `command` | TEXT NULL | stdio 必填：白名单绝对路径（镜像预装工具集内） |
| `args` | JSONB NULL | stdio：如 `["--stdio"]` |
| `isolation` | TEXT NOT NULL DEFAULT 'shared' | `shared` \| `workspace` |
| `max_instances` | INT NOT NULL DEFAULT 1 | stdio 进程数上限（shared 池） |

约束（domain 层 `ValidateMCPServerInput`）：

- `http`：url 必填（现有校验不变）；command/args 必须为空。
- `stdio`：url 必须为空（gateway 路由由平台合成）；command 必须非空且命中
  白名单前缀（v1 固定为 `/opt/mcp-tools/` 下绝对路径）；args 必须为字符串
  数组且元素非空。
- v1 仅允许 `isolation=shared` + 无 env（无凭据工具）；`workspace` 与
  `env` 字段预留在迁移中，domain 校验先拒绝（fail-closed），后续阶段放开。

快照与 worker：`_platform_mcp.servers[].url` 对 stdio 统一改写为
`{GATEWAY_BASE}/v1/mcp/{server_id}`，`server_id` 保持不变；worker 无需
感知 transport。gateway base URL 由 Platform 配置注入 scheduler
（`MCPGatewayBaseURL`，与现有 `MCPProxyBaseURL` 同模式）；未配置时
stdio server 快照 fail-closed 不下发（并告警日志），http 不受影响。

## 5. mcp-gateway 服务设计

### 5.1 部署形态

- `cmd/mcp-gateway`，Go 实现（与 backend 同仓同语言），独立镜像，与
  sandbox-manager 同层级常驻。
- 网络：仅 `database`（internal，只读 mcp_servers 表 + 接收 proxy 转发）。
  **不接入任何 egress 网络**——stdio 子进程继承无网。
- 容器：read_only + cap_drop ALL + no-new-privileges + 非 root +
  pids/mem/cpu 限制（照抄现有服务模式）；tmpfs 工作目录。

### 5.2 stdio 进程宿主

每个 stdio server 的进程由 gateway 全权管理（`stdioPool`）：

- **进程池**：`max_instances` 上限；请求调度到最空闲进程（全部繁忙且有
  余量时扩容）；单进程内 JSON-RPC 串行（stdio 单通道，per-process 互斥）。
- **握手语义**：进程只握手一次，绑定首个 initialize 的参数；后续客户端
  initialize 返回缓存响应（按各自 id 改写），崩溃重建时自动用缓存的
  参数重放握手（真实 MCP server 拒绝未初始化请求）。
- **崩溃退避**：指数退避（1s 起，×2，上限 60s）；**熔断**：连续
  `circuitBreakThreshold`（8）次失败后进入熔断，只按探活间隔（30s）尝试
  重建，不随请求反复 spawn；探活成功自动复位。退避窗口内的失败只推进
  计数不刷新窗口（防止请求风暴永久重置退避）。
- **配置热更新**：catalog 带 revision，变化时排空旧进程滚动重建。
- **空闲回收**：idle TTL（默认 5m，`GA_MCP_GATEWAY_IDLE_TTL` 可配）后
  回收全部进程；下次请求重建并重放握手。
- **JSON-RPC id 透传**：客户端 id（number/string）原样发给进程，响应
  原样回传——gateway 不自造 id（曾用自增 id 导致多客户端错位）。
- **体量限制**：请求体 ≤1MiB；响应单行 ≤8MiB（超限视为响应流不可信，
  重建进程）。
- **超时**：请求超时 = `timeout_seconds`（进程挂死 → kill 重建）；客户端
  断连（ctx 取消）不 kill——响应晚到被后续请求按 id 匹配自然丢弃。

### 5.3 Streamable HTTP 桥

- 实现 MCP Streamable HTTP 服务端语义（2025-06-18）：`initialize` 建
  会话返回 `Mcp-Session-Id` 的协议变体——gateway 不返回会话头、不校验
  会话（无状态）；`notifications/*` 透传；`tools/list` / `tools/call`
  转发到进程 stdin，读 stdout 响应回传。
- 进程未初始化时调用非 initialize 方法：进程按协议返回错误帧（gateway
  不模拟会话拒绝——协议语义由进程兜底）。
- 错误映射：未知 server 404 / catalog 不可用 503 / 退避 502 / 熔断 503。

### 5.4 观测

- `/healthz` 存活探针；`/metrics` JSON：进程数、在途队列、请求/失败/崩溃
  计数；子进程 stderr 捕获（截断防刷屏）；所有错误带 trace id。

## 6. 安全边界（沿用审查基线）

| 威胁 | 对策 |
| --- | --- |
| 管理员注册任意命令 → RCE | 命令白名单：仅 `/opt/mcp-tools/` 下镜像预装二进制；工具集变更 = 镜像变更 |
| 子进程出网（数据外泄/拉包） | gateway 无 egress 网络，子进程继承无网；uv/npm 缓存构建期预装 |
| 子进程读租户数据 / 凭据 | 工作目录 tmpfs 空目录；**子进程环境为白名单（PATH/HOME/TMPDIR），绝不继承 gateway 环境（DATABASE_URL 等凭据不外泄给子进程）**；不挂载 workspace/config 卷 |
| 未知 server_id 直连 | 白名单双保险（proxy + gateway 各自查 DB） |
| 请求放大（内存/CPU） | 体量上限 + 串行队列 + max_instances + 容器级 pids/mem 限制 |
| 内部头外泄 | `X-MCP-Workspace` 只由 proxy 发给自有 gateway，绝不发往第三方（http 直连分支不携带） |

## 7. 与现有代码的变更清单

| 文件 | 说明 |
| --- | --- |
| `postgres/migrations/0049_mcp_gateway.sql` | 新增列 + CHECK 约束 |
| `domain/mcp_server.go` | transport 分支校验；`MCPServerGatewayURL` 合成函数 |
| `postgres/mcp_server_store.go` | 列读写扩展（command NULL 扫描修复） |
| `api/mcp_server.go` | create/update 透传新字段并校验 |
| `application/mcp_snapshot.go` | stdio 快照 URL 合成改写；gateway 未配置 fail-closed |
| `api/worker_mcp_proxy.go` | 转发白名单 + `Mcp-Session-Id`；双 http.Client 超时；`MCPTarget{ViaGateway}` |
| `cmd/platform/main.go` | resolve 合成 gateway 路由 |
| `cmd/mcp-gateway`（新）+ `internal/mcpgateway/` | 进程宿主 + 桥 + 池 + 熔断 + 指标 |
| `infra/compose/compose.yaml` + `mcp-gateway.Dockerfile`（新） | 常驻服务 + 镜像（预装 uv/离线缓存） |
| worker-python | **零改动** |

## 8. 演进阶段

- **阶段 0（现状）**：http-only。
- **阶段 1（已实施 ✅）**：stdio transport 支持（shared/无凭据/无文件），
  gateway 托管进程；http 保持 proxy 直连。
- **阶段 2（不再做，决策记录）**："http 统一迁入 gateway"需要带 egress 的
  反代容器与无网宿主容器并存（egress 约束矛盾），为负收益重构；如未来
  需要全流量统一审计点，以独立反代容器形式评估。
- **阶段 3**：workspace 隔离 + env 凭据（secret 管理就绪后）；受控文件
  交换（上传→转换→下载的文件区）。

阶段划分保证每步可独立验证、可回退。

## 9. 验证门

- Go：`go vet ./...`、`go build ./...`、`go test -p 1 -count=1 -timeout 300s ./...`
- 契约/安全/smoke：`python -m pytest tenant_platform/tests/contract tenant_platform/tests/security tenant_platform/tests/smoke -q`
- gateway 专项（真实 Linux + Docker）：stdio 冒烟（mock stdio server：
  initialize/tools/list/tools/call 全链路）、多客户端 id 透传、进程池扩容、
  崩溃退避/熔断、空闲回收、配置热更新、环境隔离（子进程无凭据）、
  无网验证、白名单拒绝未知 server_id。
- 集成：真实 Postgres + platform 子进程 + Worker，管理员注册 http 与
  stdio server 各一，租户任务可调用（`TEST_DATABASE_URL` 缺失显式失败）。
