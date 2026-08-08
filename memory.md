# GenericAgent 项目状态

> 本文件是当前状态快照 + 最近活跃窗口，允许覆盖更新。
> 完整历史归档见 `memory/archive/`，稳定规律见 `tools.md`。

## 当前基线

- CI 门禁（分支/PR 级）：Go（vet/build/test -p 1/race 4 关键包）、Python（根 + 平台 contract/security/smoke/integration + bot_poller + worker）、Web（lint/build）全绿
- 集成测试依赖真实 PostgreSQL（`TEST_DATABASE_URL`），缺失显式失败
- **Round17 完成（2026-08-06，已提交 4 commit）**：全项目健康清理——backend-go 死代码（token_revoker 整文件 / 12 死 store 方法 / domain 死符号 / migrations 残留 / 一次性脚本）+ worker/web 废弃源码（旧 Dockerfile / grace_seconds / 空块 / 4 无引用资源）+ 平台文档校准（认证头 / 方法表 / 方案口径 / superpowers 历史标注）+ .tasks 进度真值刷新；范围按用户决策收敛：GA 根项目黑盒一律不动、fudankw.cn（sophub）保留
- 最后更新：2026-08-08

## 已完成能力

- 根框架：自主执行循环 `agent_loop.py`（~100 行）+ 9 原子工具 + L0-L4 分层记忆系统 + 能力自扩展（code_run 动态装包/建工具）
- 多前端：TUI（tui_v3）、Streamlit、桌面端、IM 机器人（TG/QQ/飞书/微信/企微/钉钉）、conductor
- 租户平台：Go 后端（api/application/domain/infrastructure 分层）+ gRPC Python worker + React/Vite Web + bot poller + 契约（proto/openapi/policy）
- sandbox-runner 重构（Round13）：生命周期不变量结构强制、沙箱安全加固、Foundation 垂直 E2E

## 进行中 / 未完成

- Round16/17 全部修复已交付并提交（Round16: e6fce4ad 之前；Round17: 4 commit）；遗留仅部署主机残余验证
- 有意遗留（不产生功能缺陷，已评估）：B3 wechat/ilink 命名债（`ilink_user_id` 是 API 契约字段，改名需联动 openapi/web，等契约变更窗口）；C5 delivery_service 834 行未拆（纯结构债，等 delivery 实质改动时顺手拆）；bundle 多文件 SOP 平台侧不支持（sophub 平台已上线 bundle，平台 proxy 有意收窄为 single-file，若需用要加支持）
- 残余验证（需真实 Linux 主机 + Docker/runsc）：runsc 运行时、mTLS 注入、六服务 compose 冒烟、共享卷跨 UID

## 关键决策（仍有效）

- CI 门禁分支/PR 级矩阵；集成测试必须真实 Postgres（`-p 1` 串行化规避 flaky）
- 生命周期不变量以结构强制（文件拆分）；跨语言契约（proto/openapi）为单一真值源
- 身份模型只有一类主体——用户（Bearer）；AdminToken（`PLATFORM_ADMIN_TOKEN`）仅管理面 + `/v1/router/messages` 服务入口
- **2026-08-06（Round16）**：GA 侧故障语义统一——`_retry_or_exit` 无 fatal 分支，3 次空响应/流异常/max_tokens 统一 LLM_FAILED；`total_cd_tokens` 只累计本条消息增量（勿改回累积拼接）；API 错误契约：队列满 429 / 越权 403 / 会话不存在 404（domain 哨兵 `ErrSessionAccessDenied`/`ErrWorkspaceNotFound`）；session key 解析唯一入口 `domain.ValidateWorkspaceKey`（Sscanf 宽松解析已删，勿恢复）；team id 只接受 uuid（`teamSessionKey`/`validateSessionAccess` 一致）
- **2026-08-06（Round16）**：poller 合并语义唯一实现在 `InboundCoalescingBuffer`——`coalesce_webhook_bodies` 已收敛为恒等 finalize（无 window 参数），新增合并逻辑必须改 buffer 而非该函数
- **2026-08-06（Round15）**：workspace hash 推导/校验唯一实现在 `domain.WorkspaceDirHash`（改算法只动一处）；`ctrl:` 控制 JTI 约定真值在 worker.proto TaskEnvelope.capability_jti（ExecuteTask 不强制 ctrl: 前缀是**有意不对称**，勿"修复"）；配额命名统一 per-requester（env `PER_REQUESTER_RUNNING_LIMIT`，旧名失效）；活跃任务状态集合/claim lease 谓词真值在 postgres 包常量（状态值来自 domain.TaskStatus）
- **2026-08-06（Round15）**：上限常量真值在 `domain/limits.go`（postgres/api 引用）；UTF-8 安全截断唯一实现在 `domain.TruncateUTF8`；心跳部署契约 `PROGRESS_WINDOW_S(150) > llm-proxy 120s < TASK_IDLE_TIMEOUT_SECONDS(300)`；provider routing 竞争窗口（409→TASK_FAILED）是**有意的 fail-closed 设计**，勿放宽 revision 校验（LLM_PROVIDER_ARCHITECTURE.md 已记录）
- **2026-08-06（Round15）**：tools_schema_cn.json 是 GA 永不加载的过时副本（平台 overlay 已删），GA 根资产文件不动（黑盒）
- 2026-08-06（D1 去分级）：工具能力统一静态 policy manifest，`tool_policies` 表/API 已停用
- 2026-08-06（D2-D5）：成功路径 draining 闭合、BlockUser 会话撤销、delivery fencing、DB 时钟 lease、LLM_FAILED 结构化终态

## 仍需注意的坑点

- Python 3.14 与 pywebview 不兼容，用 3.11/3.12
- runsc/mTLS/真实 Docker 验证只能在 Linux 主机
- **`postgres.OpenTestPool` 有全局互斥锁，同一测试内二次调用死锁**——测试数据直插用 pgx 单连接
- **openapi platform.yaml 不要用 yaml.safe_dump 重写**——会丢全部中文注释并破坏 test_contract_sources 的文本格式断言；补路径用文本插入
- **git checkout 单文件会丢失未提交修改**——回滚前确认工作区版本是否含未提交变更
- 契约测试 `test_route_contract.py` 拦截后端新增路由未同步 OpenAPI；KNOWN_SPEC_GAPS 已清空，新增路由必须进 spec
- **微信绑定坑（2026-08-08 更新）**：① `ILINK_BASE_URL` 为空时 main.go 不创建 wechatBindingSvc——现路由已无条件注册返回 `501 FEATURE_DISABLED`（不再裸 404），compose 透传 ILINK_* 变量（.env 与两个 .env.example 模板已含 `ILINK_BASE_URL=https://ilinkai.weixin.qq.com`，官方网关需微信 iLink Bot 应用资格）；② **iLink get_qrcode_status 是长轮询**（QR 待扫时挂连接 ~30s 才回 wait）——status 请求每尝试 8s 封顶且超时不重试（`ilink.Client.StatusRequestTimeout` 可配，默认 8s），PollStatus 超时返回 DB 最近状态（200 wait）而非报错，避免 3 次重试叠超时 + cloudflared 前置 ~35s 超时 → 504 打死前端轮询
- **微信入站聚合（2026-08-08）**：配置热更新链路完整（PUT settings → 写 DB + 推 poller + 后台重试 + platform 启动下发）；poller `InboundCoalescingBuffer` 合并谓词**必须是绝对时间间隔**（`abs(a-b) <= window`），勿改回严格递增（微信文件+文字同时发送时批次内顺序/时间戳乱序，严格递增会拆成两个任务）；文件+文字任务里 text 与 media_paths 都要处理
- **微信交付（2026-08-08）**：① 平台 delivery 层 `cleanIMMarkdown` 把回复 md 标记（**/反引号/#/链接/表格分隔线）降级纯文本——IM 渠道 md 清洗是平台职责，勿依赖 LLM 自觉；② 文档转换用 **runner 镜像本地 pandoc/python-docx**（ga-runner 已内置 pandoc 2.17），**勿恢复 MCP pandoc**（stdio server 与 runner 工作区文件系统隔离，传路径不可达、contents 无法落盘，架构性不可用）；③ `memory-template/wechat_delivery_sop.md` 是微信交付排版 SOP（纯文本铁律/pandoc 优先/质量门槛），新工作区自动预置，已有工作区需手动复制
- **ga-runner 镜像坑（2026-08-08 架构收敛）**：镜像命名已统一 `genericagent-*` 前缀（Makefile build/push/pull + compose 默认值 + .env.example* + manager 默认值五处一致，不允许再引入例外）；`docker compose build ga-runner` 产物名固定 `genericagent-ga-runner:local`（与运行时引用 GA_RUNNER_IMAGE 解耦，digest 模式不再炸）；`make runner-digest` 自动写 .env；**sandbox-manager 启动时 fail-fast 校验镜像存在**（配置漂移在启动时暴露，不再任务运行时 409）+ docker create 失败附带可用镜像列表诊断。重建镜像流程：`make build`（或 `docker build -f tenant_platform/infra/compose/ga-runner.Dockerfile -t genericagent-ga-runner:local .`，context=仓库根，先确保 memory-template/ 已生成）→ `make runner-digest`（自动更新 .env）→ `docker compose up -d sandbox-manager`
- **runner-control 网络名坑**：round11 M2 给内部网络加项目名前缀（实际名 `genericagent_runner-control`），manager 曾硬编码 `runner-control`，docker create 成功但 start 报 "network runner-control not found"——已修：`GA_RUNNER_NETWORK` 可配置（compose 已透传，默认 runner-control 兼容旧部署）
- **MCP 无公网出口架构**：Runner 仅 internal 网络，外部 MCP Server（如 mcp.exa.ai）无法直连——已实现 Platform 受控 MCP proxy（照 Sophub proxy 模式）：worker 拨号地址改写为 `{proxy}/v1/worker/mcp/{server_id}` + Bearer capability，server_id→URL 映射即白名单；`_platform_mcp.proxy` 随签名运行时配置下发。改 MCP 相关需同时重建 platform + ga-runner 镜像（worker-python 在 runner 镜像里，digest 变化后要更新 .env 的 GA_RUNNER_IMAGE 并重启 sandbox-manager）
- **MCP stdio 链路（2026-08-08 重构定案）**：stdio server 由 mcp-gateway 常驻服务托管（`internal/mcpgateway`，无 egress 网络）；http 保持 proxy 直连（不迁 gateway——egress 约束矛盾，决策见 MCP_GATEWAY_DESIGN.zh-CN.md §8）。关键不变量：① stdio URL 唯一合成 `domain.MCPServerGatewayURL`（快照与 proxy resolve 共用，管理员注册 stdio 时 url 必须为空）；② proxy 转发白名单必须含 `Mcp-Session-Id`（曾缺失导致 gateway 第二跳 400）；③ gateway 无状态（不维护会话表/不返回会话头），JSON-RPC id 原样透传（gateway 自造 id 会让第二客户端错位）；④ 子进程环境白名单（PATH/HOME/TMPDIR），绝不继承 gateway 环境（DATABASE_URL 不外泄）；⑤ 锁序 g.mu → pool.mu 单向（spawnLocked 内取内部 id 用 atomic，曾死锁）；⑥ `max_instances` 是进程池上限（全忙且有余量才扩容），退避窗口内失败只推进计数不刷新窗口；⑦ 熔断后探活间隔固定，请求不得刷新探活
- **runsc DNS 坑**：gVisor netstack 与 Docker 内嵌 DNS(127.0.0.11) 不兼容（UDP 查询持续 EAI_AGAIN，runc 正常）——runner 内 platform/llm-proxy 等内部服务名全部解析失败。已修：manager 创建 runner 时注入 `--add-host`（docker network inspect 取成员 IP + 逐容器 inspect 取 compose 别名；注意 **docker inspect 输出是 JSON 数组**，单对象解析会静默失败丢别名）；runner 无出站需求（外部全走代理），无需上游 DNS
- **MCP 工具被策略过滤坑**：foundation.v1.json 的 allowed_tools 是静态白名单，动态 MCP 工具名（exa__web_search_exa）会被 apply_tool_policy 和 install_dispatch_guard **两处**过滤——策略文件加 `mcp:*` 通配，两处过滤逻辑都要支持（只放行 _tenant_global_mcp_tool_names，其余仍精确匹配）
- **部署配置注意**：`PER_TENANT_RUNNING_LIMIT` 已改名 `PER_REQUESTER_RUNNING_LIMIT`（compose 双模板同步）；`PLATFORM_DEV_*` 已改名 `PLATFORM_ADMIN_*`
- **改 proto 注释必须重跑 `generate_bindings.py`**——注释会进入生成代码，不重生成则产物与 proto 漂移
- **SQL 谓词收敛陷阱**：runner_leases 表有 status 列（多表查询必须保留别名前缀）；task_lifecycle 容量门禁的 status 谓词故意不含 lease 条件
- **心跳基线**：`last_progress_at` 以 put_task 时刻为基线（task_runner 构造传入），勿改回 0.0（启动后首轮长思考会被 idle reaper 误收割）
- **boundMsg/truncateBytes 已删**：截断统一用 `domain.TruncateUTF8(s, limit)`（需显式 limit，旧 boundMsg 内置上限）
- `.tasks/*/SUBTASKS.csv` 字段内含逗号须引号包裹

## 最近活跃窗口

- 2026-08-08：**MCP Gateway 架构重构**（3b834bb 的 stdio 支持修复+收敛，未提交）：编译修复（缺 types.go/常量定义）+ proxy 会话头丢失 + JSON-RPC id 错位 + stdio URL 语义统一（`MCPServerGatewayURL`）+ gateway 无状态会话化 + 进程池/指数退避/熔断/热更新 + 子进程白名单环境（DATABASE_URL 泄漏修复）+ 双超时客户端 + openapi/web/文档同步；真实 E2E 验证（worker MCPHTTPClient ↔ gateway ↔ 子进程，含 gateway 重启恢复）；详见 memory/archive/2026-08.md

- 2026-08-06：Round17 健康清理完成并提交 4 commit（backend-go/worker/web 死代码 + 平台文档校准 + .tasks 刷新）；范围决策：GA 根项目黑盒不动、fudankw.cn(sophub)保留
- 2026-08-06：Sophub 集成全链路梳理（单路径 proxy 架构确认；bundle SOP 缺口；README badge 无尾斜杠超时）——详见 memory/archive/2026-08.md
- 2026-08-06：Round16 修复（G1/G2 GA 侧语义 + SubmitTask 错误分级 + session key 严格解析 + C2/C3/C6/F5-F7/B5/B8 遗留清理）
- 2026-08-06：Round15 P3/P4（E 组文档校准 + F 组分层/UTF-8 截断/心跳基线 + routing 竞争窗口确认）
- 2026-08-06：Round15 P2 真值源收敛（workspace hash 唯一实现 + ctrl: JTI 进 proto + per-requester 命名 + 决策编号 D1 + SQL 谓词常量 + tools_schema_cn 删除）
- 2026-08-06：审查 I-4 鉴权统一（语义改名、任务端点 userAuth + owner 校验、pending 用户门禁、OpenAPI 32 条 gap 补齐）
- 2026-08-06：全项目健康/安全审查 11 项 findings + 14 项修复实施
- 2026-08-05：Round13 收尾 CI 全量模拟验证 + Foundation 垂直 E2E 纳入 CI
- 2026-08-06：初始化项目协作文件体系
