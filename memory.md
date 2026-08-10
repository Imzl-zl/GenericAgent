# GenericAgent 项目状态

> 本文件是当前状态快照 + 最近活跃窗口，允许覆盖更新。
> 完整历史归档见 `memory/archive/`，稳定规律见 `tools.md`。

## 当前基线

- CI 门禁（分支/PR 级）：Go（vet/build/test -p 1/race 5 关键包）、Python（根 + 平台 contract/security/smoke/integration + bot_poller + worker）、Web（lint/build）全绿
- 集成测试依赖真实 PostgreSQL（`TEST_DATABASE_URL`），缺失显式失败
- **IM 多渠道完成（2026-08-10，im-channel-binding epic 6/6 DONE）**：渠道配置统一模型（channel_configs）+ 飞书/钉钉/QQ 接入（poller BotAdapter 注册表）+ im-bindings API + Web 渠道绑定页
- **IM 流式输出完成（2026-08-10，im-streaming-delivery epic 7/7 DONE）**：StreamingSender 可选接口 + scheduler 500ms 节流转发管道 + 飞书消息编辑打字机 + QQ 单聊原生流式 + 群聊收敛（只发最终结果）+ im_streaming_mode 管理开关；真实渠道冒烟待用户凭据
- 最后更新：2026-08-10

## 已完成能力

- 根框架：自主执行循环 `agent_loop.py`（~100 行）+ 9 原子工具 + L0-L4 分层记忆系统 + 能力自扩展（code_run 动态装包/建工具）
- 多前端：TUI（tui_v3）、Streamlit、桌面端、IM 机器人（TG/QQ/飞书/微信/企微/钉钉）、conductor
- 租户平台：Go 后端（api/application/domain/infrastructure 分层）+ gRPC Python worker + React/Vite Web + bot poller + 契约（proto/openapi/policy）
- IM 多渠道（2026-08-10）：对话单元分桶（workspace 共享 + 每群/私聊一桶 + /new 桶级化）+ 渠道绑定（微信扫码 + 飞书/钉钉/QQ 凭据表单，凭据 JSON 加密入库）
- IM 流式输出（2026-08-10）：worker Chunk → scheduler 500ms 节流合并 → StreamingSender（飞书=占位消息+PUT 编辑打字机 / QQ 单聊=原生 stream{state,id,index,reset} 帧 / 钉钉微信=仅终态）；群聊统一只发最终结果（conversation_type 判定）；stream_final_at 抑制 delivery 文本重复（文件照发）；im_streaming_mode 管理开关（默认 streaming）
- sandbox-runner 重构（Round13）：生命周期不变量结构强制、沙箱安全加固、Foundation 垂直 E2E

## 进行中 / 未完成

- im-channel-binding epic 仅剩**真实渠道冒烟**（需用户提供飞书/钉钉/QQ 应用凭据）
- im-streaming-delivery epic 仅剩**真实渠道冒烟**（需用户提供凭据；重点=飞书编辑链路 + QQ 流式帧序列参数实测）
- 有意遗留（不产生功能缺陷，已评估）：C5 delivery_service 834 行未拆（纯结构债）；bundle 多文件 SOP 平台侧不支持（sophub 平台已上线 bundle，平台 proxy 有意收窄为 single-file，若需用要加支持）
- 残余验证（需真实 Linux 主机 + Docker/runsc）：runsc 运行时、mTLS 注入、六服务 compose 冒烟、共享卷跨 UID

## 关键决策（仍有效）

- CI 门禁分支/PR 级矩阵；集成测试必须真实 Postgres（`-p 1` 串行化规避 flaky）
- 生命周期不变量以结构强制（文件拆分）；跨语言契约（proto/openapi）为单一真值源
- 身份模型只有一类主体——用户（Bearer）；AdminToken（`PLATFORM_ADMIN_TOKEN`）仅管理面 + `/v1/router/messages` 服务入口
- **2026-08-10（IM 流式输出定案，im-streaming-delivery 已落地）**：按渠道分档实现流式（非统一路径）——飞书=消息编辑打字机（占位消息+PUT 全量替换+5 QPS 令牌桶）、QQ 单聊=官方原生流式接口（stream{state 1/10,id,index,reset} 帧序列+msg_id 被动锚点+append≤2 保护 4 次/条限制）、QQ 群聊/钉钉/微信=仅终态；群聊统一收敛只发最终结果（tasks.conversation_type 判定，入站契约新增字段）；stream_final_at 标记流式 commit 成功，delivery 文本 part 跳过（文件照发）；失败路径无标记 → delivery 兜底补发（IM 消息不可撤回）；im_streaming_mode=off|final_only|streaming 管理开关（默认 streaming，私聊默认开）；scheduler StreamingSender 可选接口 nil=关。设计真值：`tenant_platform/docs/IM_STREAMING_DELIVERY.zh-CN.md`。**QQ 流式接口参数与飞书编辑链路需真实凭据实测（残余风险）**。
- **2026-08-10（IM 多渠道定案，im-channel-binding 已落地）**：渠道配置统一模型——`bots` → `channel_configs`（migration 0053，RENAME 后 FK 自动跟随；存量微信行 channel_type='wechat'；每用户每渠道一行；(owner_id, channel_type) 唯一）；凭据 JSON 密文（微信={token}、新渠道={app_id, app_secret}），API 只回脱敏值；新渠道属主即 canonical user（无二次绑定）；群消息触发=平台硬规则（钉钉/QQ 必须 @，飞书只申请 group_at_msg，不申请收全部的敏感权限）；Poller BotAdapter 注册表 + SDK 惰性导入；**契约字段 ilink_user_id → channel_account_id（B3 命名债已清）**。设计真值：`tenant_platform/docs/IM_CHANNEL_BINDING.zh-CN.md` + `IM_CHANNEL_ARCHITECTURE.zh-CN.md`（会话模型已落地）。
- **2026-08-10（IM 多渠道会话模型，im-channel-session 已落地）**：数据隔离单元 = workspace；对话上下文按"对话单元"分桶（桶 key = `channel:chat_id`；微信固定单桶 `wechat:me`）；`/new` 清当前桶（conversation_resets 表）；TaskEnvelope.conversation_id=14；GA 核心零改动。
- **2026-08-06（Round16）**：GA 侧故障语义统一——`_retry_or_exit` 无 fatal 分支；`total_cd_tokens` 只累计本条消息增量；API 错误契约：队列满 429 / 越权 403 / 会话不存在 404；session key 解析唯一入口 `domain.ValidateWorkspaceKey`；team id 只接受 uuid
- **2026-08-06（Round16）**：poller 合并语义唯一实现在 `InboundCoalescingBuffer`——`coalesce_webhook_bodies` 已收敛为恒等 finalize，新增合并逻辑必须改 buffer 而非该函数
- **2026-08-06（Round15）**：workspace hash 推导/校验唯一实现在 `domain.WorkspaceDirHash`；`ctrl:` 控制 JTI 约定真值在 worker.proto TaskEnvelope.capability_jti；配额命名统一 per-requester；活跃任务状态集合/claim lease 谓词真值在 postgres 包常量
- **2026-08-06（Round15）**：上限常量真值在 `domain/limits.go`；UTF-8 安全截断唯一实现在 `domain.TruncateUTF8`；心跳部署契约 `PROGRESS_WINDOW_S(150) > llm-proxy 120s < TASK_IDLE_TIMEOUT_SECONDS(300)`；provider routing 竞争窗口（409→TASK_FAILED）是有意的 fail-closed 设计
- 2026-08-06（D1 去分级）：工具能力统一静态 policy manifest；2026-08-06（D2-D5）：成功路径 draining 闭合、BlockUser 会话撤销、delivery fencing、DB 时钟 lease、LLM_FAILED 结构化终态

## 仍需注意的坑点

- Python 3.14 与 pywebview 不兼容，用 3.11/3.12
- runsc/mTLS/真实 Docker 验证只能在 Linux 主机
- **`postgres.OpenTestPool` 有全局互斥锁，同一测试内二次调用死锁**——测试数据直插用 pgx 单连接
- **openapi platform.yaml 不要用 yaml.safe_dump 重写**——会丢全部中文注释并破坏 test_contract_sources 的文本格式断言；补路径用文本插入
- **git checkout 单文件会丢失未提交修改**——回滚前确认工作区版本是否含未提交变更
- 契约测试 `test_route_contract.py` 拦截后端新增路由未同步 OpenAPI；KNOWN_SPEC_GAPS 已清空，新增路由必须进 spec
- **表改名迁移坑（2026-08-10 新增）**：0003 的 marker 就是 bots 表本身——0053 RENAME 后必须重建仅作 marker 的 stub `bots` 表（否则 0003 被重放重建空表）；RENAME/列改名/约束改名无 IF NOT EXISTS，用 DO 块条件执行（0052 先例）
- **poller BotManager.start 构造必须在锁外**（2026-08-10）：WeChatAdapter 构造回调 coalesce_window_provider → 同一非重入锁死锁
- **凭据 upsert 前捕获旧配置**（2026-08-10）：ON CONFLICT DO UPDATE 覆盖 bot_uuid 后无法再取旧 UUID 停 poller 会话
- **微信绑定坑（2026-08-08）**：① `ILINK_BASE_URL` 为空时现路由已无条件注册返回 `501 FEATURE_DISABLED`；② iLink get_qrcode_status 是长轮询——status 请求每尝试 8s 封顶且超时不重试，PollStatus 超时返回 DB 最近状态（200 wait）
- **微信入站聚合（2026-08-08）**：`InboundCoalescingBuffer` 合并谓词必须是绝对时间间隔（`abs(a-b) <= window`），勿改回严格递增；文件+文字任务里 text 与 media_paths 都要处理
- **微信交付（2026-08-08）**：平台 delivery 层 `cleanIMMarkdown` 降级纯文本是平台职责；文档转换用 runner 镜像本地 pandoc/python-docx，勿恢复 MCP pandoc；`memory-template/wechat_delivery_sop.md` 新工作区自动预置
- **ga-runner 镜像坑（2026-08-08）**：镜像命名统一 `genericagent-*` 前缀；`docker compose build ga-runner` 产物名固定 `genericagent-ga-runner:local`；sandbox-manager 启动时 fail-fast 校验镜像存在；重建流程 `make build` → `make runner-digest` → 重启 sandbox-manager
- **runner-control 网络名坑**：实际名 `genericagent_runner-control`，用 `GA_RUNNER_NETWORK` 可配置
- **MCP 无公网出口架构**：外部 MCP Server 走 Platform 受控 proxy；改 MCP 相关需同时重建 platform + ga-runner 镜像
- **MCP stdio 链路（2026-08-08）**：stdio server 由 mcp-gateway 常驻服务托管；关键不变量 ①`domain.MCPServerGatewayURL` 唯一合成 ②proxy 转发白名单含 Mcp-Session-Id ③gateway 无状态 ④子进程环境白名单 ⑤锁序 g.mu → pool.mu ⑥max_instances 进程池上限 ⑦熔断后探活固定间隔
- **runsc DNS 坑**：gVisor netstack 与 Docker 内嵌 DNS 不兼容——manager 注入 `--add-host`（docker inspect 输出是 JSON 数组）
- **MCP 工具被策略过滤坑**：foundation.v1.json 的 allowed_tools 静态白名单——策略文件加 `mcp:*` 通配，两处过滤逻辑都要支持
- **部署配置注意**：`PER_REQUESTER_RUNNING_LIMIT`（旧名 PER_TENANT_RUNNING_LIMIT 失效）；`PLATFORM_ADMIN_*`（旧名 PLATFORM_DEV_* 失效）
- **改 proto 注释必须重跑 `generate_bindings.py`**
- **SQL 谓词收敛陷阱**：runner_leases 表有 status 列（多表查询必须保留别名前缀）
- **心跳基线**：`last_progress_at` 以 put_task 时刻为基线，勿改回 0.0
- **boundMsg/truncateBytes 已删**：截断统一用 `domain.TruncateUTF8(s, limit)`
- `.tasks/*/SUBTASKS.csv` 字段内含逗号须引号包裹

## 最近活跃窗口

- 2026-08-10：**im-streaming-delivery 全部落地**（7/7 DONE）：StreamingSender/StreamReply 接口 + StreamForwarder（500ms 节流合并 + open/append/commit/abort）+ scheduler 接入（Terminal commit + 失败 abort + 群聊收敛）+ 飞书编辑打字机 + QQ 单聊原生流式 + im_streaming_mode 开关 + Web 设置项；migration 0054（conversation_type + stream_final_at + text_value）；全量验证绿（存量失败 4 处与本次无关）；真实渠道冒烟待用户凭据
- 2026-08-10：**im-channel-binding 全部落地**（6/6 DONE）：migration 0053（bots→channel_configs）+ domain.Bot→ChannelConfig 全库改名 + im-bindings API（user+admin）+ Router 多渠道分桶（Source/ConversationKey）+ poller BotAdapter 注册表（飞书/钉钉/QQ adapter）+ Web 渠道绑定页；契约字段 ilink_user_id→channel_account_id（B3 命名债已清）；全量验证绿（存量失败 4 处与本次无关，base commit 复现）；真实渠道冒烟待用户凭据
- 2026-08-10：IM 多渠道架构落地（3 批提交）：契约 conversation_id + Go 分桶全链路 + /new 桶级化；epic im-channel-session 任务 1-5 DONE、任务 6 砍掉
- 2026-08-08：MCP Gateway 架构重构（stdio 支持 + 收敛）；2026-08-06：Round17 健康清理 4 commit；Sophub 集成全链路梳理；Round16 修复；Round15 P2-P4 真值源收敛；审查 I-4 鉴权统一；全项目健康/安全审查 11 findings + 14 修复；Round13 收尾；2026-08-06：初始化项目协作文件体系
