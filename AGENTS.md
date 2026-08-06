# GenericAgent AGENTS

## 0. 实施基线

- 环境：跨平台（Windows/macOS/Linux）；CI 为 GitHub Actions（ubuntu-latest + PostgreSQL 16 service）；沙箱类验证需真实 Linux 主机 + Docker/runsc
- 语言/运行时：Python >=3.10,<3.14（推荐 3.11/3.12，不要用 3.14）；Go（tenant_platform/backend-go，版本见 go.mod）；Node 20（tenant_platform/web）
- 主要技术栈：根项目 = 极简纯 Python（requests / beautifulsoup4 / bottle / simple-websocket-server / aiohttp，可选 extras：`ui`、`all-frontends`）；租户平台 = Go 后端 + protobuf/gRPC Python worker + React/Vite Web + bot poller
- 配置根目录：`tenant_platform/config/`、`tenant_platform/infra/`；API Key 配置：`mykey.py`（模板 `mykey_template.py`）
- 标准验证命令：见 `## 5`（与 `.github/workflows/ci.yml` 对齐，CI 是分支/PR 级门禁）

## 1. 作用

- 本文件是项目级硬规则，只保留长期约束实现结构的内容。
- 详细设计、字段说明、流程细节统一回到正式文档（`tenant_platform/docs/`、`docs/`、`tenant_platform/contracts/`）。

## 2. Source of Truth

- 根项目：当前代码 > `README.md` > `docs/`（安装/上手文档，非设计真值）
- 租户平台：`tenant_platform/docs/`（`LLM_PROVIDER_ARCHITECTURE.md`、`GA_SANDBOX_RUNNER_REFACTOR.zh-CN.md`、`IMPLEMENTATION_SUMMARY.md`、`bot-poller-auth.md`）与 `tenant_platform/contracts/`（`openapi/`、`policy/`、`proto/`）是设计真值
- 跨语言契约（protobuf / openapi）是 Go、worker-python、web 之间的单一真值源，改动需同步生成/实现
- 任务进度追踪：`.tasks/<epic>/SUBTASKS.csv` 是进度真值，`.tasks/<epic>/PROGRESS.md` 是配套说明
- `tools.md` / `memory.md` / `memory/archive/` 只做协作辅助，不覆盖正式设计真值

## 3. 架构与项目结构

- 根目录 = 自进化 agent 框架本体：`agent_loop.py`（核心执行循环，~100 行）、`agentmain.py`、`ga.py`、`llmcore.py`（LLM 核心）、`mykey.py`（密钥配置）、`TMWebDriver.py`（浏览器工具）、`simphtml.py`；CLI 入口 `ga`（`ga_cli.cli:main`）
- `frontends/` = 多前端：TUI（`tui_v3.py` 推荐、`tuiapp_v2.py`）、Streamlit（`stapp.py`/`stapp2.py`）、桌面（`desktop/`、`qtapp.py`、`hub.pyw`/`launch.pyw`）、IM 机器人（tg/qq/feishu/wechat/wecom/dingtalk `*app.py`）、`conductor.py`
- `memory/` = **应用运行时分层记忆系统**（L0 元规则 / L1 索引 / L2 全局事实 / L3 SOP / L4 会话归档 + SOP 文件），与根目录协作文件 `memory.md`、`memory/archive/` 无关
- `tenant_platform/` = 多语言租户平台：
  - `backend-go/`：Go 后端，分层 `api → application → domain` + `infrastructure`，入口 `cmd/`
  - `worker-python/`：gRPC Python worker（`src` 布局），与 backend-go 通过 `contracts/proto` 契约绑定
  - `web/`：React + Vite + TS 控制台
  - `bot_poller/`：IM 轮询；`contracts/`（openapi/policy/proto）；`config/`、`infra/`（compose 等）、`scripts/`、`tests/`（contract/security/smoke/integration）
- 依赖方向：`frontends/*` 与 IM 机器人 → 根 agent；`backend-go` ⇄ `worker-python` 经 proto 契约；web 经 openapi 契约

## 4. Agent 协作文件规则

- 开始任务前，优先读取项目根 `tools.md` 与 `memory.md`。
- 只读取当前项目根协作文件；`memory/archive/` 只按需读取当月或最近一个归档文件，不批量加载历史。
- `tools.md` 记录稳定可复用的命令、路径、模式、坑点；写入前先读。
- `memory.md` 是当前状态快照 + 最近活跃窗口；完整功能/修复闭环后整体覆盖更新，不做流水追加。
- `memory.md` 只保留：当前基线 / 已完成能力 / 进行中 / 关键决策 / 坑点 / 最近活跃窗口；移除失效项，目标 ≤120 行。
- `memory/archive/YYYY-MM.md` 是月度归档；重要根因分析、调试洞察、关键决策和踩坑按月追加，写入前先读当月文件，不存在先创建。
- `memory/archive` 追加格式：`## [日期 | 标题]` + `- **Events**：` / `- **Changes**：` / `- **Insights**：`。
- 协作文件不是产品真值，不得覆盖正式设计和当前代码。

## 5. 验证、入口与关键路径

- 标准验证命令（CI 矩阵，`tenant_platform/backend-go` 目录下）：
  - Go：`go vet ./...`；`go build ./...`；`go test -p 1 -count=1 -timeout 300s ./...`
  - Go race（关键并发包）：`go test -race -p 1 -count=1 -timeout 600s ./internal/application/... ./internal/infrastructure/worker/... ./internal/infrastructure/sandbox/... ./internal/infrastructure/checkpoint/... ./internal/infrastructure/postgres/...`
  - Python 根：`python -m pytest tests -q`
  - 平台：`python -m pytest tenant_platform/tests/contract tenant_platform/tests/security tenant_platform/tests/smoke -q`；`python -m pytest tenant_platform/tests/integration -q`（需真实 Postgres + platform 子进程 + Worker）；`python -m pytest tenant_platform/bot_poller -q`
  - Worker（`tenant_platform/worker-python`）：`pip install -e '.[test]'` 后 `python -m pytest -q`
  - Web（`tenant_platform/web`）：`npm ci`；`npm run lint`；`npm run build`
- 集成测试环境变量：`TEST_DATABASE_URL`（PostgreSQL，缺失时集成测试必须显式失败——项目契约）
- 关键入口：`ga`（CLI）、`agentmain.py`、`tenant_platform/backend-go/cmd/`、`tenant_platform/web`（Vite dev：`npm run dev`）
- 关键文档入口：`README.md`、`docs/GETTING_STARTED.md`、`tenant_platform/docs/`、`.tasks/<epic>/PROGRESS.md`

## 6. 项目特定规则

- `memory/`（根目录）是应用运行时记忆目录，.gitignore 采用逐文件白名单管理；新增需入库的文件必须补 `!memory/...` 白名单，协作归档 `memory/archive/` 已白名单。
- 集成测试（Go 与 Python）依赖真实 PostgreSQL；`TEST_DATABASE_URL` 缺失时显式失败，不静默跳过。
- Go 测试共享同一 PostgreSQL 实例时必须 `-p 1` 串行化，避免包间 truncate 互踩（死锁/唯一键冲突是已知并发干扰表现）。
- 沙箱相关验证（runsc 运行时、mTLS 证书注入、六服务 compose 冒烟、共享卷跨 UID）需真实 Linux 主机，CI 不覆盖的部分在 `.tasks/<epic>` 方案中声明残余风险。
- 跨语言契约（`contracts/proto`、`contracts/openapi`）是单一真值源：改契约必须同步生成 Go `gen` 与 worker-python 代码，并跑契约绑定测试。
- 平台后端分层：`api → application → domain`，`infrastructure` 只被 `application` 注入使用；生命周期类不变量（scheduler 的 dispatch/checkpoint/reaper/worker/terminate）以结构强制，不靠临时分支绕过。
- 安全：密钥只进 `mykey.py`/环境变量，任何测试不得含真实密钥（`test_no_real_key_leak` 会子进程检查 Worker 环境）。

## 7. 维护原则

- 本文件保持短、小、硬。
- 近期状态放 `memory.md`，稳定协作知识放 `tools.md`，长期历史放 `memory/archive/`。
- 项目特定解释性长文回到 `docs/` 或正式文档，不堆进本文件。
