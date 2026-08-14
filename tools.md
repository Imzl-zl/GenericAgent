# GenericAgent Tools

## Concepts

- 本文件记录项目协作知识，不替代正式设计文档或当前代码。
- 真值优先级：`tenant_platform/contracts/`（proto/openapi/policy）与 `tenant_platform/docs/` > 当前代码 > README/docs 安装文档 > 协作文件。
- 主链路：根项目 = `agent_loop.py`（核心循环）→ 10 原子工具 → `memory/` 分层记忆；租户平台 = backend-go ⇄ worker-python（proto 契约），web 经 openapi。

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
- **本机测试库（2026-08-14 实证）：`ga-test-pg` 容器（独立容器，非 compose），`127.0.0.1:55433`，用户/密码/库均 `test`：`export TEST_DATABASE_URL='postgresql://test:test@127.0.0.1:55433/test?sslmode=disable'`——不设此变量跑 Go 集成测试会大面积假失败（勿当成回归）
- **集成测试 E2E 依赖（CI 单独装，不在 pyproject）：`uv pip install 'psycopg[binary]' psutil`**（psycopg 播种旧实例数据、psutil 验 worker 进程隔离）；缺失时对应场景 ModuleNotFoundError 失败
- 集成测试直插 workspace 必须幂等（`ON CONFLICT (session_key) DO NOTHING`）——注册路径已自动建行（0050 不变量）；`_register_user` 先例
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
- **表改名迁移（2026-08-10 实证）**：早期迁移的 marker 可能就是表本身（0003 的 marker = bots 表）——RENAME 后必须重建仅作 marker 的 stub 表，否则该迁移被重放重建空表；RENAME/列改名/约束改名无 IF NOT EXISTS，用 DO 块条件执行（0052/0053 先例）。
- `TEST_DATABASE_URL` 缺失时集成测试显式失败；本地先起 Postgres 并设环境变量。
- 两包并行跑共享测试库有既有 flaky（死锁/唯一键冲突），用 `go test -p 1` 规避，不要当代码 bug 修。
- runsc 运行时、mTLS 证书注入、六服务 compose 冒烟、共享卷跨 UID 读写只能在真实 Linux 主机验证（方案 §10 声明），CI/Windows 本地不可覆盖。
- `memory/` 下新增需入库文件必须补 `.gitignore` 白名单（`!memory/...`），否则被 `memory/*` 吞掉。
- worker-python 测试前必须 `pip install -e '.[test]'`（pythonpath 指向 `src`，缺 python-docx 时 docx 相关测试会失败）。
- 平台产物（platform.exe 等）是构建产物，不要提交改动；契约/策略变更要同步 `contracts/` 与各端实现。
- Dockerfile base 镜像 digest pin 会随 Docker Hub manifest 清理失效：构建报 `400 Bad Request` / `not found` 时，用 `docker buildx imagetools inspect <镜像>:<tag> | grep Digest` 取新 index digest 更新。已实测失效并修复：`alpine:3.19`（llm-proxy）、`docker:27-cli`（sandbox-manager）；其余 pin（python:3.11-slim/golang:1.22/node:22/nginx:1.27）2026-08 仍有效。

## IM 渠道能力（稳定事实）

- **空结果静默成功坑（2026-08-12 实证）**：上游模型退化响应（仅 `<summary>`/thinking 或空白 content）会被 GA 当正常完成提交空结果（thinking 非空不算 blank，`_empty_ct` 不计数）——平台回"任务完成：任务已完成"而用户没得到回答，**不是渠道/流式问题**。判断方法：查 tasks.result_digest（空串 sha256=e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855）与 committed bundle body。防护四层（已落地）：do_no_tool 剥 summary/thinking 判可见文本 + MixinSession 空结果切换 session + worker EMPTY_RESULT + delivery 诚实文案。
- **模型自动降级切换 = GA 原生 MixinSession**（llmcore）：mykey 配 `mixin_*` 键（llm_nos 按顺序选 session），`!!!Error:`/流异常/空结果（2026-08-12 补）自动切下一个，成功回弹（spring_back 300s），每 session 重试 max_retries；平台侧 `runtime_config.go` 在 >1 provider 时自动写 `mixin_config.llm_nos`（默认 provider 必须排第一，resolveRoutingSnapshot 强制）——**只需在平台 web 添加 ≥2 个 provider 即自动启用**；只配 1 个 provider 时无 mixin。provider 变量名 `platform_native_<oai|claude>_provider_<id>_config` 与 GA resolve_session 的 'native'/'claude'/'oai' 名字解析对齐；native_oai/native_claude 可混配（NativeOAISession 继承 NativeClaudeSession，同组校验通过）。
- **微信 Bot（iLink）只能个人自用**：仅私聊、不能加群、无"多个好友/客服"场景——`wxbot_client.py` 消息模型只有 `from_user_id/to_user_id`，无群概念；对话单元固定单桶（`wechat:me`）。平台侧微信 bot ↔ 用户 1:1（QR 扫码绑定）。设计多 IM 时勿把微信当客服机器人模型。
- 可群聊渠道：QQ（`qqapp.py` `group_openid`）、Telegram（chat 模型天然支持）、钉钉（`dingtalkapp.py` `conversation_type=="2"` → `group:{id}`）、企微（`wecomapp.py` `chatid`）、飞书（`fsapp.py`）。
- 数据模型定案：隔离单元 = workspace（personal/team），memory/SOP/项目文件全共享；**对话上下文按"对话单元"（渠道×群/对端）分桶**，`/new` 清当前桶；桶 key = `channel:chat_id`。设计真值：`tenant_platform/docs/IM_CHANNEL_ARCHITECTURE.zh-CN.md`。
- **渠道配置模型（2026-08-10 定案，bots → channel_configs）**：每用户每渠道一行（(owner_id, channel_type) 唯一）；凭据 = JSON 密文（微信={token}、新渠道={app_id, app_secret}）；新渠道属主即 canonical user（无二次绑定）；群消息触发 = 平台协议硬规则（钉钉/QQ 必须 @，飞书只申请 `group_at_msg`，不申请收全部的敏感权限 group_msg）；契约字段 `ilink_user_id` → `channel_account_id`（已全链路同步）。设计真值：`tenant_platform/docs/IM_CHANNEL_BINDING.zh-CN.md`。
- **Poller BotAdapter 注册表（2026-08-10）**：`bot_poller/poller_server.py` 按 bot_uuid 注册渠道 adapter（WeChat=WxBotClient 长轮询 / Feishu=lark-oapi WS / DingTalk=dingtalk-stream / QQ=botpy WS）；SDK 惰性导入（缺失只影响该渠道）；/start 契约 = bot_uuid + channel_type + config_json（解密后的凭据 JSON）；入站统一 POST /v1/im/webhook（body 含 channel_type/channel_account_id/conversation_id/conversation_type；conversation_id 取值：QQ=group_openid/openid、钉钉=conversationId、飞书=chat_id、微信恒空；conversation_type：QQ is_group、飞书 chat_type、钉钉 conversation_type=='2'，微信恒 private）；配置热更新 = 平台热推（PUT/DELETE im-bindings → poller start/stop）。
- **生图 image_gen（2026-08-14，Phase B v1 直连形态）**：第 10 原子工具；llmcore `ImageGenClient`（实现进 llmcore.py，**严禁新增 imagegen.py**——沙箱 overlay 只物化固定清单 LEGACY_MODULES，新增模块 = 平台沙箱启动 ImportError 全平台任务失败）；配置 = mykey `image_gen` dict（变量名不得含 api/config/cookie 子串，agentmain 会话扫描误判）；**错误前缀用 `[Error: image_gen ...]`，绝不用 `!!!Error:`**（模型最终回复尾部含它触发 do_no_tool 致命判定/LLM_FAILED）；**response_format 只给 dall-e 发**（gpt-image 恒 b64_json，发该参数可能 400，同步默认路径就废）；**落盘必须走 self.cwd/outputs/**（硬编码 script_dir 在容器里写错位置，交付链断）；落盘前 ≤20MiB 检查（Go 交付上限 fail-closed）；文件名带时间戳（`image_<ts>_<i>.png`）防同任务重复调用同名覆盖；n>1 返回多行 `[FILE:]` marker；**marker 回显依赖**——模型须在最终回复回显 `[FILE:outputs/...]` 才触发交付（直连形态无兜底；**托管形态已补 generated_output_files 登记**：worker `install_image_gen_marker_registry` 包装 do_image_gen，终态 append_missing_file_markers 自动补写）；双形态设计：直连 = 真实上游+密钥，托管 = llm-proxy + llm.image 能力令牌，一份代码配置决定。**new-api 中转实测结论（2026-08-14，newapi.myovo.cc.cd）**：①**size 必传**（计费硬要求，缺省 500"图片尺寸计费需要传 size"）——客户端已默认 1024x1024；②gpt-image-2/gemini-3-pro-image/gemini-3.1-flash-image-preview 返回 b64_json（quality/output_format 兼容）；③**sensenova-u1-fast / agnes-image-2.1-flash 只回 `url` 直链**（b64_json 空）——客户端已加 url 直下兜底；④sensenova size 合法集特殊（1664x2496/2048x2048 等，无 1024x1024），错误文本含合法列表模型可自愈；⑤流式（stream:true+partial_images SSE）未实测，建议 stream=False 走同步。真实 CLI 闭环验证通过（agnes-2.5-flash 对话 + gpt-image-2 生图）。设计真值：`.tasks/im-media-pipeline/PHASE_B_IMAGE_GEN_PLAN.zh-CN.md`
- **生图托管形态（2026-08-14，Step 3 已实施）**：平台模式生图链路 = admin 配 image 能力 provider（web 表单 capabilities 勾 Image）→ 签发 `llm.image` capability token → runtime_config 下发 `image_gen` 块（不进 chat mixin）→ GA 沙箱 resolve_image_gen 读 mykeys['image_gen'] → llm-proxy `/v1/images/generations` 路由（operation 错配 401）→ 上游。**provider 能力维度**：`llm_providers.capabilities` JSONB（migration 0058，省略=[chat]）；image 仅 native_oai；**生图响应 32MiB 上限**（Content-Length 前置拒绝，MaxWorkerRequestBytes 只管请求体）；多 image provider v1 fail-closed；至少一个 chat provider（image-only 部署被拒）；双能力 provider 按 (ID,能力) 双 token（chat/image 预算独立计量）。生图响应 32MiB 上限双闸（Content-Length 前置 + chunked 流式计数）；migration 0059 补 DB CHECK。**托管语义边界**：model override 托管必 409（直连可用）；双能力 provider 单 model 字段（image 请求带同一 model 打上游，建议独立 image provider）；web 表单勾 Image 时有提示。policy `foundation.session-files.v1` 已补 image_gen（平台沙箱模型可见工具）。上生产必须 make build 全量重建（ga-runner + platform + llm-proxy + web）。
- **IM 流式输出（2026-08-10，im-streaming-delivery 已落地）**：`StreamingSender`/`StreamReply` 可选接口（transport 包），非流渠道不实现；poller /send 扩展 stream_id+stream_action(open|append|commit|abort)（不新增端点，open 响应回 stream_id）；飞书=占位消息+PUT 全量替换打字机（_TokenBucket 5 QPS）；QQ 单聊=原生流式帧（stream{state 1/10, id, index, reset}，全量替换语义，append≤2 保护被动回复 4 次/条）；scheduler 500ms 节流合并（首条 chunk 为窗口起点，flush 由下一 chunk/心跳/Terminal 驱动）；stream_final_at 置位后 delivery 跳过文本 part（文件照发）；群聊统一只发最终结果（tasks.conversation_type）。设计真值：`tenant_platform/docs/IM_STREAMING_DELIVERY.zh-CN.md`。QQ 流式帧参数需真实凭据实测。

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
