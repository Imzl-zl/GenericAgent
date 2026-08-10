# GenericAgent Tools

## Concepts

- 本文件记录项目协作知识，不替代正式设计文档或当前代码。
- 真值优先级：`tenant_platform/contracts/`（proto/openapi/policy）与 `tenant_platform/docs/` > 当前代码 > README/docs 安装文档 > 协作文件。
- 主链路：根项目 = `agent_loop.py`（核心循环）→ 9 原子工具 → `memory/` 分层记忆；租户平台 = backend-go ⇄ worker-python（proto 契约），web 经 openapi。

## Read First

- `README.md`（双语文档，含架构/快速开始/评测）
- `docs/GETTING_STARTED.md`（新手上手）、`docs/installation*.md`、`docs/SETUP_FEISHU.md`
- `tenant_platform/docs/`：`GA_SANDBOX_RUNNER_REFACTOR.zh-CN.md`、`LLM_PROVIDER_ARCHITECTURE.md`、`IMPLEMENTATION_SUMMARY.md`、`bot-poller-auth.md`
- `.tasks/<epic>/PROGRESS.md` + `SUBTASKS.csv`（进行中 epic 的进度真值）

## Tools

标准验证命令（与 `.github/workflows/ci.yml` 对齐）：
- Go（`tenant_platform/backend-go`）：`go vet ./...`；`go build ./...`；`go test -p 1 -count=1 -timeout 300s ./...`；race：`go test -race -p 1 -count=1 -timeout 600s ./internal/application/... ./internal/infrastructure/worker/... ./internal/infrastructure/sandbox/... ./internal/infrastructure/checkpoint/... ./internal/infrastructure/postgres/...`
- Python：根 `python -m pytest tests -q`；平台 `python -m pytest tenant_platform/tests/contract tenant_platform/tests/security tenant_platform/tests/smoke -q`、`python -m pytest tenant_platform/tests/integration -q`、`python -m pytest tenant_platform/bot_poller -q`；worker（`tenant_platform/worker-python`）`pip install -e '.[test]'` 后 `python -m pytest -q`
- Web（`tenant_platform/web`）：`npm ci`；`npm run lint`；`npm run build`；本地 `npm run dev`

定向验证：
- 集成测试前先设 `TEST_DATABASE_URL`（真实 PostgreSQL）；缺失时集成测试显式失败
- 契约绑定测试：`tenant_platform/tests/contract`（import worker-python 生成代码，需 protobuf/grpcio）
- 安全测试：`tenant_platform/tests/security`（子进程调用 go test 验证 Worker 无真实密钥）
- 本地安装根包：`pip install -e .`（按需 `pip install -e '.[ui]'` 或 `.[all-frontends]`）

关键入口：
- CLI：`ga`（`ga_cli.cli:main`）；主程序 `agentmain.py`；核心循环 `agent_loop.py`；LLM 核心 `llmcore.py`；密钥 `mykey.py`
- 前端：`frontends/tui_v3.py`（推荐 TUI）、`frontends/stapp.py`（Streamlit）、`frontends/desktop/`
- 平台：`tenant_platform/backend-go/cmd/`；`tenant_platform/worker-python/src/`；`tenant_platform/web`

关键目录：
- 配置：`tenant_platform/config/`、`tenant_platform/infra/`（compose）
- 契约：`tenant_platform/contracts/{proto,openapi,policy}`
- 应用运行时记忆：`memory/`（L0-L4 + SOP，.gitignore 逐文件白名单；勿与协作 `memory.md` 混淆）
- 任务追踪：`.tasks/<epic>/`（PROGRESS.md + SUBTASKS.csv）

## Patterns

- 生命周期不变量结构强制：scheduler 按 dispatch/checkpoint/reaper/worker/terminate 拆分文件，不变量由结构保证，不靠分支绕过（Round13 确立）。
- 契约先行：改 proto/openapi → 生成 Go `gen` + worker-python 代码 → 跑契约绑定测试，再实现业务。
- 共享 PostgreSQL 测试实例：Go 包间 `-p 1` 串行，避免 truncate 互踩。
- CI 门禁：分支/PR 级矩阵（Go/Python/Web）先于合并；集成测试需真实 Postgres service。
- 沙箱实证：真实 Docker 行为（如 volume-subpath inspect 格式）以集成测试实证为准，不凭文档假设。

## Pitfalls

- Python 3.14 与 pywebview 等依赖不兼容；推荐 3.11/3.12（CI 用 3.11）。
- `TEST_DATABASE_URL` 缺失时集成测试显式失败；本地先起 Postgres 并设环境变量。
- 两包并行跑共享测试库有既有 flaky（死锁/唯一键冲突），用 `go test -p 1` 规避，不要当代码 bug 修。
- runsc 运行时、mTLS 证书注入、六服务 compose 冒烟、共享卷跨 UID 读写只能在真实 Linux 主机验证（方案 §10 声明），CI/Windows 本地不可覆盖。
- `memory/` 下新增需入库文件必须补 `.gitignore` 白名单（`!memory/...`），否则被 `memory/*` 吞掉。
- worker-python 测试前必须 `pip install -e '.[test]'`（pythonpath 指向 `src`，缺 python-docx 时 docx 相关测试会失败）。
- 平台产物（platform.exe 等）是构建产物，不要提交改动；契约/策略变更要同步 `contracts/` 与各端实现。
- Dockerfile base 镜像 digest pin 会随 Docker Hub manifest 清理失效：构建报 `400 Bad Request` / `not found` 时，用 `docker buildx imagetools inspect <镜像>:<tag> | grep Digest` 取新 index digest 更新。已实测失效并修复：`alpine:3.19`（llm-proxy）、`docker:27-cli`（sandbox-manager）；其余 pin（python:3.11-slim/golang:1.22/node:22/nginx:1.27）2026-08 仍有效。

## IM 渠道能力（稳定事实）

- **微信 Bot（iLink）只能个人自用**：仅私聊、不能加群、无"多个好友/客服"场景——`wxbot_client.py` 消息模型只有 `from_user_id/to_user_id`，无群概念；对话单元固定单桶（`wechat:me`）。平台侧微信 bot ↔ 用户 1:1（QR 扫码绑定）。设计多 IM 时勿把微信当客服机器人模型。
- 可群聊渠道：QQ（`qqapp.py` `group_openid`）、Telegram（chat 模型天然支持）、钉钉（`dingtalkapp.py` `conversation_type=="2"` → `group:{id}`）、企微（`wecomapp.py` `chatid`）、飞书（`fsapp.py`）。
- 数据模型定案：隔离单元 = workspace（personal/team），memory/SOP/项目文件全共享；**对话上下文按"对话单元"（渠道×群/对端）分桶**，`/new` 清当前桶；桶 key = `channel:chat_id`。设计真值：`tenant_platform/docs/IM_CHANNEL_ARCHITECTURE.zh-CN.md`。

## 镜像打包（compose）

- 唯一部署入口：`tenant_platform/infra/compose/`（compose.yaml + 7 个 Dockerfile + Makefile + .env 模板）。文档：`compose/README.zh-CN.md`。
- 打包命令（在 compose 目录）：`make build`（全量构建，tag = `:local` + `:$(git describe)`）；`make push REGISTRY=host/ga` / `make pull REGISTRY=host/ga VERSION=x`（registry 流转）；`make runner-digest`（输出生产 `GA_RUNNER_IMAGE=ga-runner@sha256:...` 供 .env）；`make verify`（compose config 校验）。
- **镜像 tag 名必须与 .env.example 的 `GA_*_IMAGE` 对齐**：Dockerfile 文件名（platform）≠ 镜像名（genericagent-platform），ga-runner 例外（无前缀）。Makefile 已按此映射打 tag（2026-08-08 修复）；手工 `docker build` 时按同样映射，否则 compose 静默用旧镜像（服务器曾踩：构建成功但容器一直跑旧 genericagent-*:local）。
- 构建上下文 = 仓库根；Makefile `ROOT := $(abspath ../../..)`，不要改层级。
- 生产模板 `.env.example` fail-closed：`GA_RUNNER_IMAGE` 必须 digest 引用 + `GA_RUNNER_SECURITY_PROFILE=runsc`；本地开发用 `.env.example.dev`（允许 tag + runc）。
- 服务器（2026-08-08 部署）无 make 命令：`sudo apt-get install -y make` 后走正规流程；等价手动命令见 memory/archive/2026-08.md。

## 2026-08-06 补充坑点

- `postgres.OpenTestPool(t)` 持有全局 `testDBMu`（t.Cleanup 释放）——同一测试内二次调用必然死锁；测试直插数据用 `pgx.Connect(TEST_DATABASE_URL)` 单连接。
- `contracts/openapi/platform.yaml` 严禁 `yaml.safe_dump` 整体重写（丢中文注释 + 破坏 test_contract_sources 的文本断言）；补路径用文本插入（`insert_security`/追加 blocks），改完跑 `python -c "import yaml; yaml.safe_load(...)"` 验证。
- 契约测试 `test_route_contract.py`：KNOWN_SPEC_GAPS 已清空，新增后端路由必须同步 OpenAPI spec，否则测试失败。
- 改名全链路清单（DevToken→AdminToken 类）：Go 标识符 → env 变量 → HTTP header → OpenAPI scheme → web client → 测试常量 → compose/.env 模板 → 部署文档；`--dev-loopback` flag 与 DB `bootstrap_marker` 存量值是部署形态标记，不在改名范围。
- 集成测试 seed 用户会话：`token_hash = base64.urlsafe_b64encode(sha256(token).digest()).rstrip(b"=").decode()`（与 Go `hashToken` 一致）。
- 用户侧任务能力链路：注册(pending) → 管理员批准(approved) → 提交任务；pending 用户提交会被 service 门禁拒绝。
- 部署踩坑（2026-08-07 实测）：
  - 复用旧 `postgres_data` 卷时 SASL 认证失败（密码是旧卷初始化时的，与 .env 无关）→ `reset-dev.sh` 清卷重建。
  - 生产 .env 下 `docker compose up -d --build` 报 `build tag cannot contain a digest`（GA_RUNNER_IMAGE 是 digest，不能作 build tag）→ 构建/启动分离：`make build` + `make up`（Makefile 与 reset-dev.sh 已修）。
  - 生产启动 bootstrap 报 `users_bootstrap_marker_check`(23514)：migration 0001 约束只放行 `'dev-loopback'`，而生产路径 `EnsurePlatformAdminUser` 用 `'platform-admin'` → 已新增 `0048_platform_admin_bootstrap.sql` 放宽（users/workspaces 两个约束 + null_volume_requires_loopback），migrations.go 三个列表同步。
