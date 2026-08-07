# GenericAgent 完整 Docker Compose 部署

这是平台唯一保留的部署方式：一个 `compose.yaml` 编排全部服务，一个 `.env` 放全部配置。`docker compose up -d --build` 会启动六个常驻服务：Web、Platform、Bot Poller、PostgreSQL、Sandbox Manager 与内部 LLM Proxy。GA Runner 不是常驻服务，由 Sandbox Manager 按工作区活跃状态动态创建。

> 本部署对应 [GA Sandbox Runner 重构方案](../../docs/GA_SANDBOX_RUNNER_REFACTOR.zh-CN.md) 的最终形态：渠道身份绑定 → 个人/团队工作区 → 每工作区唯一隔离 Runner。

## 需要什么

- Linux 服务器，建议 4 vCPU、8 GiB RAM、40 GiB 可用磁盘；
- Docker Engine；
- Docker Compose v2；
- `git` 和 `openssl`；
- 生产环境必须安装 `gVisor/runsc` 作为 Runner 运行时（`GA_RUNNER_SECURITY_PROFILE=runsc`）；本机开发可用 Docker 加固（需显式设置 `GA_RUNNER_ALLOW_RUNC=1`，未设置时 Manager 拒绝启动）。

Runner 容器由 Sandbox Manager 动态创建，需要 Docker socket 仅挂载给 `sandbox-manager` 服务。不需要安装 GenericAgent 的 systemd 服务，不需要 1Panel，也不需要手工创建 `config`、`runtime`、数据库或数据目录。

## 只有两个文件

```text
tenant_platform/infra/compose/
├── compose.yaml   # 服务、网络、数据卷和启动顺序
└── .env           # 端口、密码、Token、服务参数
```

`.env` 不提交 Git。`compose.yaml` 不包含真实密码，只从 `.env` 读取变量。

## 镜像打包（规范化流程）

全部镜像在 `tenant_platform/infra/compose/` 下通过 `make` 统一构建。镜像 tag 自动取 git 版本（`git describe`，如 `9e8e4e8` 或 `v1.2.0-3-g9e8e4e8`；工作区有未提交改动时带 `-dirty` 后缀），同一镜像打两个 tag：

- `:local` —— compose 默认引用，直接运行用；
- `:<版本>` —— 追溯/推送用。

### 场景 A：单服务器直接构建部署（最简单）

```bash
cd tenant_platform/infra/compose
make build            # 构建全部 6 个镜像(首次约 10-20 分钟, 之后有缓存)
make runner-digest    # 输出 GA_RUNNER_IMAGE=ga-runner@sha256:... 填入 .env(生产模板必填)
make verify           # docker compose config 校验
make up               # 启动全部服务
```

### 场景 B：构建机 + 私有 Registry（多机 / 规范 CI）

```bash
# 构建机: 构建并推送
cd tenant_platform/infra/compose
make build
make push REGISTRY=registry.example.com/ga

# 服务器: 拉取并重打 :local(.env 的 GA_*_IMAGE 无需改动)
make pull REGISTRY=registry.example.com/ga VERSION=<构建机相同的版本号>
make verify && make up
```

> 生产模板（`.env.example`）要求 `GA_RUNNER_IMAGE` 为 digest 引用：`make runner-digest` 输出的那一行直接粘贴到 `.env` 即可（本地构建的镜像也适用，digest 取自镜像 ID/RepoDigests）。

## 配置并启动

```bash
cd tenant_platform/infra/compose
cp .env.example .env          # 生产基线模板(fail-closed: digest + runsc)
chmod 600 .env
```

> **本地开发(可信机器)**: 使用 `.env.example.dev` 模板(允许可变 tag 与默认
> docker 运行时, 两个都是显式信任开关, sandbox-manager 不会静默退化为不安全
> 配置):
>
> ```bash
> cp .env.example.dev .env
> ```

编辑 `.env`，只需要处理以下内容：

1. 保持 `GA_HTTP_BIND=127.0.0.1`、`GA_POSTGRES_BIND=127.0.0.1` 不变。
2. 生成六个随机值：

```bash
openssl rand -hex 32  # POSTGRES_PASSWORD
openssl rand -hex 32  # BOT_TOKEN_KEY，必须 64 位十六进制
openssl rand -hex 32  # BOT_POLLER_API_SECRET
openssl rand -hex 32  # PLATFORM_WEBHOOK_SECRET
openssl rand -hex 32  # PLATFORM_ADMIN_TOKEN
openssl rand -hex 32  # LLM_PROXY_CAPABILITY_SIGNING_KEY
```

3. 将第一个值同时填到 `POSTGRES_PASSWORD` 和 `DATABASE_URL` 中的密码位置。
4. 将 `BOT_POLLER_API_SECRET` 和 `PLATFORM_WEBHOOK_SECRET` 各填一个值即可，Platform 与 Bot Poller 会自动读取同一个变量。
5. 替换全部 `CHANGE_ME...`。其余变量保持默认即可。

生产模板额外要求(不可跳过, 缺失即 sandbox-manager 拒绝启动):

- `GA_RUNNER_IMAGE` 必须是 `@sha256:` digest 引用——本地构建镜像后取 digest:

```bash
# 先按开发模板构建 ga-runner 镜像, 然后:
docker inspect --format='{{index .RepoDigests 0}}' ga-runner:local
# 把输出中的 ga-runner@sha256:... 填到 GA_RUNNER_IMAGE
```

- 主机必须安装并配置 `gVisor/runsc`(`GA_RUNNER_SECURITY_PROFILE=runsc`)。

检查并启动：

```bash
docker compose config >/dev/null
docker compose up -d --build   # 构建并启动全部服务(含 ga-runner 镜像)
docker compose ps
curl --fail http://127.0.0.1:8088/healthz
docker compose logs --tail=200 sandbox-manager
```

`ga-runner` 服务只构建不常驻(`scale: 0`): 容器实例由 Sandbox Manager 按
工作区活跃状态动态创建, 无需单独执行 `docker compose build ga-runner`。

生产模板默认 `GA_RUNNER_ALLOW_TAG=0` + `GA_RUNNER_ALLOW_RUNC=0` + `runsc`;
本地开发模板显式放宽(见上)。任何非 digest 镜像引用或非 runsc 运行时都会让
Sandbox Manager 拒绝启动(fail-closed, 不会静默降级)。

默认 Web 只监听服务器本机的 `127.0.0.1:8088`。远程浏览器请通过 SSH 隧道或 HTTPS 反向代理访问，不要把 8088、55432 或数据库端口直接暴露到公网。

首次登录管理页面后，在“LLM Providers”配置模型供应商、Base URL、模型和 API Key。供应商 API Key 会加密保存到 PostgreSQL，不再写入 `.env`。LLM 调用经内部 `llm-proxy` 转发，Runner 只持有短期 capability，不接触真实 Key。

## Runner 工作区与动态容器

- 用户首次发消息时，Platform 将渠道身份解析为 `personal:<user_id>`（或授权团队的 `team:<team_id>`），Sandbox Manager 创建该工作区的 Runner 容器并挂载其 `memory/`、`temp/`、`state/`。
- 任务即进程（决策 D1）：每个 task 创建全新 Runner 容器，任务终态（成功/失败/取消/超时）即销毁；会话连续性由 checkpoint 快照恢复，工作区数据始终保留。`GA_RUNNER_IDLE_TTL` 是 Runner lease 的 TTL（续租周期与异常兜底回收的参考值），不是空闲复用阈值。
- `GA_RUNNER_MAX_ACTIVE` 是全局并发 Runner 上限；满载时新任务保持排队，不失败。
- Runner 只加入内部 `runner-control` 网络，只能访问 Platform 控制端点与内部 LLM Proxy；不挂载 Docker socket、不暴露任何宿主机路径。

## 权限边界

只有 `sandbox-manager` 挂载了 `/var/run/docker.sock`，它是唯一能创建/销毁 Runner 的组件；Platform、Web、Bot Poller、PostgreSQL 与 Runner 均不持有 Docker socket。Runner 容器强制使用非 root 用户、只读根文件系统、仅 `runner-control` 网络、最小 capability、no-new-privileges 与 CPU/内存/PID 限制。宿主浏览器工具（`web_scan`/`web_execute_js`）在 Runner 中禁用。

## 日常命令

```bash
# 状态和日志(make 等价于下方 docker compose 命令)
make ps
make logs

# 更新代码后重建并启动
cd tenant_platform/infra/compose
git pull
make build && make verify && make up

# 停止服务但保留数据
docker compose down

# 再次启动
docker compose up -d
```

不要执行 `docker compose down -v`。`-v` 会删除数据库、任务、会话文件、Bot 媒体、Runner 工作区等所有数据。

### 开发期清库切换

重构采用清库式切换（方案 D12）：启用新部署前删除旧 PostgreSQL 数据卷及 Document 相关运行卷，再按新 schema 启动。仓库处于开发阶段，不保留生产历史数据，不提供旧数据兼容或回滚。

```bash
# 删除旧 PG 数据卷与 document_work 卷，然后按新 schema 重建启动
./infra/compose/reset-dev.sh

# 完整重置（额外删除 platform_runtime / platform_config / session_files /
# bot_media / runner_workspaces——runner_workspaces 含全部用户记忆/SOP/项目/
# 文件，删除前脚本会二次确认，审查 F8）
./infra/compose/reset-dev.sh --all
```

## 原理

Compose 一次创建并管理六个服务：`postgres` 存平台数据，`platform` 提供 API、调度、渠道绑定与 Runner lease 控制，`bot-poller` 负责 IM，`web` 提供网页入口，`llm-proxy` 是内部透明模型代理（仅 `database` + `runner-control` 网络，不映射宿主端口），`sandbox-manager` 负责 Runner 容器的创建/检查/销毁与空闲回收。Platform 会等待 PostgreSQL 与 Bot Poller 健康后启动，并自动执行数据库 migration。

Compose 还会自动创建六个命名卷：数据库、平台运行数据、平台配置、会话文件、Bot 媒体和 Runner 工作区。它们由 Docker 管理，不出现在项目目录中；容器重建或 `docker compose down` 都不会删除数据。
