# 失败策略与错误域分类（Failure Policy & Error Taxonomy）

> 状态：设计真值（2026-08-12 落地）。这是横切约定，不属于单个模块——所有层（api / application / infrastructure）新增代码必须遵守。
> 关联：`MCP_GATEWAY_DESIGN.zh-CN.md`（配额/计量）、`LLM_PROVIDER_ARCHITECTURE.md`（capability/JTI）
> 结论先行：**严重性判定在设计期定死（错误域分类 + 失败策略矩阵），运行时只执行预定策略，不做现场判断。**

## 1. 错误域分类（Error Taxonomy）

所有错误分三域，handler 层据此映射 HTTP 状态码——**这是 api 层 `writeStoreError` 的单一真值源**：

| 错误域 | 语义 | HTTP | 客户端应如何对待 |
|---|---|---|---|
| 客户端校验错误 | 缺字段/格式错误/非法状态迁移（`domain.ErrValidation` 及域内校验） | 400 | 修正输入，不重试 |
| 业务拒绝 | 目标不存在（404）/冲突（409）——资源状态不允许操作 | 404 / 409 | 修正引用或处理冲突，重试无意义 |
| 基础设施故障 | DB 不可用、连接池耗尽、存储读写失败等 | 500 | 可退避重试；**不是客户端的错** |

强制规则：
1. **store/service 层返回业务拒绝必须用 domain 哨兵**（`errors.New` 于 `internal/domain/`，可 `%w` 包装上下文），禁止裸字符串错误表达业务拒绝——字符串无法 `errors.Is`。
2. **api 层一律经 `writeStoreError` 映射**（`internal/api/store_errors.go`），新增哨兵必须在该文件登记分支。禁止 handler 里散写 `writeErr(400, ...)` 承接 store 错误（历史遗留已清理：user/invite/llm-provider/mcp 域；im_bindings 域已知待硬化，见 §5）。
3. **基础设施错误不得降级为 4xx**（2026-08 审查实证：`mergeMaskedHeaders` 曾把 DB 故障映射 400，客户端把故障当输入错误无限重试）；业务拒绝也不得升为 5xx。
4. 参数校验错误（service 层）统一包 `domain.ErrValidation`。

现有哨兵清单（`internal/domain/`）：`ErrValidation`、`ErrUserNotFound`、`ErrUsernameExists`、`ErrInviteCodeInvalid`（team.go）、`ErrInviteNotFound`、`ErrProviderNotFound`、`ErrProviderStateConflict`、`ErrMCPServerNotFound`、`ErrMCPServerConflict`、`ErrChannelBindingNotFound` 等。

## 2. 失败策略矩阵（设计期定死的处理契约）

对每个外部依赖预先选定策略，审查时只检查"实现是否符合预定策略"，不现场讨论：

| 依赖/操作 | 失败时行为 | 策略 | 理由 |
|---|---|---|---|
| PostgreSQL 配额事务（ConsumeMCPQuotas） | 整体拒绝 429/503，无部分扣减 | fail-closed | 配额=资金类资源，守恒优先 |
| 配额预检（MCPQuotaAvailable） | 调度剔除全部 MCP server 不下发；proxy 预检 503 | fail-closed | 不冒泄漏风险 |
| 调度过滤属主解析（GetWorkspaceOwner） | 剔除全部 MCP server（不中断任务，任务无 MCP 继续跑） | fail-closed（降级可用） | 宁可无 MCP 不冒越权 |
| JTI 计量计数器（ConsumeCapabilityCall） | 503 拒绝，fail-closed | fail-closed | 防逃费 |
| MCP/Sophub 上游调用 | 30s 响应头超时 → 502；调用视为已发起（JTI/配额已消费） | 快速失败 | 挂死服务器不拖垮平台 |
| 任务状态 DB 瞬时故障（MarkDispatchStarted 等） | 销毁本地 Worker，任务保持 starting 下轮重派 | retry（自动） | 可恢复路径不终态化 |
| 租约/claim 冲突 | 当前轮放弃，下轮再试 | retry（自动） | 分布式正常竞争 |
| IM 流式转发端口（Streaming） | nil = 关闭，只发终态结果 | fail-soft | 降级可用，主路径不受影响 |
| 终态撤销 JTI 失败 | 尽力撤销 + 日志告警，不阻塞终态 | fail-soft | 撤销由恢复路径兜底 |

边界取舍（已文档化，勿"优化"掉）：
- **JTI 白扣窗口**：MCP proxy 中 JTI 消费在配额扣减之前——quotaConsume 阶段的 503/竞态 429 会白烧一次 JTI（短期任务预算，可接受）。方向性选择：用户配额从不错扣。
- **调度过滤失败静默降级**：与 proxy 的 503 显式失败不对称，但都 fail-closed。

## 3. 结构强制清单（不变量靠结构，不靠注释）

以下不变量历史上靠"代码位置/注释/约定"保证并出过事，现要求以结构或测试强制：

1. **装配顺序**（2026-08-11 B1）：Scheduler 构造时 Streaming 端口立即取值（值语义），`cmd/platform/main.go` 中 botTransport 必须在 `NewScheduler` 之前赋值。改动装配段后跑 `scheduler_stream_test.go`。
2. **配额属主单一来源**（2026-08-11 B2）：配额属主 = workspace owner（sessionKey → `GetWorkspaceOwner`），调度过滤与 proxy 扣减同源，禁止按 RequesterID 推断。
3. **配额扣减原子性**（2026-08 Y2）：day+month 单事务、固定顺序 FOR UPDATE、哨兵回滚。禁止恢复单周期 `ConsumeMCPQuota`（已弃用，仅测试）。
4. **业务拒绝必须类型化**（本文件 §1）：新 handler 的错误处理一律走 `writeStoreError`。

## 4. 审查闭环约定（修一个，扫一类）

每次症状修复后，review 必须追问两个问题：
1. **同病的其他实例在哪？**（示例：掩码 400/500 混用 → 扫描全部 handler 的 writeErr(400) 承接 store 错误 → 清理 user/invite/provider 域）
2. **这个修复改变了什么不变量？**（示例：Y5 计量后移 → 必须同步核对 JTI 白扣边界注释，避免后人误改顺序）

## 5. 已知待硬化清单（存量技术债，新增代码不得复制）

| 位置 | 现状 | 计划 |
|---|---|---|
| `internal/api/im_bindings_http.go` saveBinding | 直判 `pgx.ErrNoRows` → 404（api 层 import pgx，分层小瑕疵）；BINDING_SAVE_FAILED 400 混合业务拒绝与基础设施 | 业务拒绝哨兵化后改走 `writeStoreError` |
| `internal/application/user_service.go` DeleteUser 等 | 部分校验错误尚未包 `ErrValidation` | 触及时顺手补 |
| invite `DeleteInviteCodes` | 已用 `application.ErrInviteCodesRequired`（application 层哨兵，跨层不一致） | 迁移到 domain 哨兵 |
