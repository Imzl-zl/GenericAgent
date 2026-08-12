# MCP 治理 Epic：key 平台侧注入 + 每用户配额 + stdio 演进

> **2026-08-12 修订**：D10 已落地——stdio transport 接入方式为 **Worker 沙箱内进程宿主**
> （主流 mcp.json 方式，command/args 直通，预装 / npx -y / uvx 全支持），runner 经
> runner-control 出网，npx/uvx/pip 共享缓存卷（GA_PKG_CACHE_VOLUME），运行时镜像提供
> （node 20 LTS + uv）。本 EPIC 中涉及 stdio 的决策以 D10 修订版为准。

**Goal:** 管理员 web 端以 mcp.json 风格 JSON 直接配置 MCP（url + headers），key 平台侧持有（proxy 注入，worker 快照不见 key）；按用户 × 每 MCP server × 周期配额控制调用次数（proxy 原子强制）；stdio 接入（Worker 沙箱内进程宿主）；补文档转换 SOP。

**Decisions（全部 user confirmed，2026-08-11）：**
- D1 可信部署（非公开多租户对抗）；D2 保留 proxy（计量 + key 注入）
- D4' 配置 = mcp.json 风格 JSON 直接编辑（无独立 key 表单字段）；回显 headers 掩码（留空=不变、填写=更新）
- D8' 移除 url-key 兼容；headers 唯一注入形态（Tavily/Exa/Firecrawl 官方均支持 header 鉴权）；url_query 扩展点不做
- D9 DB 存储（mcp_servers 表）+ web JSON 编辑；不引入配置文件
- D6 配额粒度 = 每用户 × 每 server × 周期（day/month）；无配额行默认放行；D7 记账维度 = 用户（经 task 联查）；D3 proxy 先校验后放行（429 MCP_QUOTA_EXCEEDED）
- D10 stdio 分发已落地(2026-08-12 修订): **Worker 沙箱内进程宿主(主流 mcp.json 方式, 管理员配置 command/args; 预装命令 / npx -y / uvx 全部直通)** + **npx/uvx/pip 共享缓存卷(GA_PKG_CACHE_VOLUME, 全租户复用一份)**; runner 经 runner-control 出网(可信部署 D1 主流模型, 按需拉包); 运行时由镜像提供(node 20 LTS + uv, ga-runner.Dockerfile 固定版本+sha256)。原"registry 白名单/受控代理"方案废弃——可信部署下过度设计(agent 数据本就走 LLM 通道出网, 且 HTTP MCP 仍走 proxy 的 key 托管+配额计量不受影响)。stdio 调用不参与 proxy 按调用计量; 配额按快照签发 MCPQuotaAvailable 门控 server 可用性。
- D12 PDF 引擎保持 pandoc→docx→LibreOffice 渲染式；D13 字体保持镜像预装；D11 文档转换 SOP **已存在**（memory-template/document_conversion_sop.md，2026-08-08 实测版），本轮仅核对一致性，无需新增

**Constraints:**
- ~~worker-python 零改动 / 快照 schema 冻结~~（2026-08-12 修订：stdio 恢复需快照携带 transport/command/args，worker 同步演进）
- 契约先行：openapi 同步（test_route_contract 门禁）；migration 0055（最新 0054）
- 安全测试 `test_container_deployment_bundle.py` 8→7 服务断言必须同步，否则 CI 红
- 集成测试需真实 Postgres（TEST_DATABASE_URL，缺失显式失败）；Go 测试 `-p 1` 串行
- 不留死代码：stdio 相关旧路径（domain 校验、snapshot 分支、proxy 旧字段、web 表单、独立宿主包）整体清理

**Non-goals:**
- 不做 registry 白名单/stdio 受控代理(2026-08-12 废弃: 可信部署下过度设计, runner 直接出网为主流模型)
- 不做 url_query 注入扩展点（遇 query-only 服务再加）
- 不做公开多租户对抗模型
- 不改 SOP 体系/预置机制（复制语义已定案）
- 不引入 TeX Live/weasyprint

**Architecture:**
管理员 web JSON 编辑 → DB（mcp_servers.headers + 配额两表）→ scheduler 签发 budget 按剩余配额 → 快照无 key → proxy 每请求：JWT 校验 + JTI 任务级计量 + 配额原子扣减 + headers 注入 + 转发（HTTP MCP）；stdio MCP 由 Worker 沙箱内 MCPStdioClient 进程宿主（快照携带 command/args，runner 出网拉包，共享缓存卷）。

**Final validation:**
Go vet/build/test（-p 1）全绿；契约/安全/smoke 全绿（bundle 7 服务断言）；集成：headers 注入后第三方收到正确凭据、配额超限 429、快照不含 key、admin API 不回显 key；web lint/build 绿；compose config 校验通过；SOP 预置文件存在。

## Deliverables

1. migration 0055 + store（headers/配额）+ 单测
2. domain 改造（headers 校验 + transport 模型）+ 单测
3. application 改造（配额 budget + 快照纯 URL）+ 单测
4. api 改造（headers/掩码/配额/脱敏/去旧网关字段）+ 单测
5. 独立 stdio 宿主服务清理（cmd/compose/infra/安全测试）
6. web（JSON 编辑 + 配额管理）
7. SOP 核对（已存在文件与 D11 一致性）
8. 集成验证全绿
