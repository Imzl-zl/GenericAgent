# GenericAgent 完整 Docker Compose 部署

这是平台唯一保留的部署方式：一个 `compose.yaml` 编排全部服务，一个 `.env` 放全部配置。`docker compose up -d --build` 会启动 Web、Platform、Bot Poller、PostgreSQL 和 `document-manager`，DOCX 文档功能包含在内。

## 需要什么

- Linux 服务器，建议 4 vCPU、8 GiB RAM、40 GiB 可用磁盘；
- Docker Engine；
- Docker Compose v2；
- `git` 和 `openssl`。

完整文档功能需要 Linux Docker socket `/var/run/docker.sock`。不需要安装 GenericAgent 的 systemd 服务，不需要 1Panel，也不需要手工创建 `config`、`runtime`、数据库或数据目录。

## 只有两个文件

```text
tenant_platform/infra/compose/
├── compose.yaml   # 服务、网络、数据卷和启动顺序
└── .env           # 端口、密码、Token、服务参数
```

`.env` 不提交 Git。`compose.yaml` 不包含真实密码，只从 `.env` 读取变量。

## 配置并启动

```bash
cd tenant_platform/infra/compose
cp .env.example .env
chmod 600 .env
```

编辑 `.env`，只需要处理以下内容：

1. 保持 `GA_HTTP_BIND=127.0.0.1`、`GA_POSTGRES_BIND=127.0.0.1` 不变。
2. 生成六个随机值：

```bash
openssl rand -hex 32  # POSTGRES_PASSWORD
openssl rand -hex 32  # BOT_TOKEN_KEY，必须 64 位十六进制
openssl rand -hex 32  # BOT_POLLER_API_SECRET
openssl rand -hex 32  # PLATFORM_WEBHOOK_SECRET
openssl rand -hex 32  # PLATFORM_DEV_TOKEN
openssl rand -hex 32  # LLM_PROXY_CAPABILITY_SIGNING_KEY
```

3. 将第一个值同时填到 `POSTGRES_PASSWORD` 和 `DATABASE_URL` 中的密码位置。
4. 将 `BOT_POLLER_API_SECRET` 和 `PLATFORM_WEBHOOK_SECRET` 各填一个值即可，Platform 与 Bot Poller 会自动读取同一个变量。
5. 替换全部 `CHANGE_ME...`。其余变量保持默认即可。

检查并启动：

```bash
docker compose config >/dev/null
docker compose up -d --build
docker compose ps
curl --fail http://127.0.0.1:8088/healthz
docker compose logs --tail=200 document-manager
```

`document-manager` 首次启动会通过 `/var/run/docker.sock` 自动构建 `genericagent-document-tool:local`，然后管理实际的 DOCX 文档容器。因此不需要先构建第五张镜像，也不需要手工创建文档目录。

默认 Web 只监听服务器本机的 `127.0.0.1:8088`。远程浏览器请通过 SSH 隧道或 HTTPS 反向代理访问，不要把 8088、55432 或数据库端口直接暴露到公网。

首次登录管理页面后，在“LLM Providers”配置模型供应商、Base URL、模型和 API Key。供应商 API Key 会加密保存到 PostgreSQL，不再写入 `.env`。

## 文档功能与权限

`DOCUMENT_GATEWAY_BASE_URL=http://127.0.0.1:8080/v1/document` 已启用 Platform 的文档工具。管理员在页面启用文档池后，Worker 可以提交 DOCX 任务；`document-manager` 从 PostgreSQL 领取任务并创建受限文档容器。

为实现“一条 Compose 命令完成全部测试”，只有 `document-manager` 挂载了 `/var/run/docker.sock`。这使它能够构建和启动文档容器，因此它是高权限服务；Platform、Web、Bot Poller 和 PostgreSQL 均不挂载 Docker socket。动态文档容器仍强制使用非 root 用户、只读根文件系统、无网络、无宿主目录挂载、最小 capability、seccomp 和 CPU/内存/PID 限制。

`.env` 中的 `DOCUMENT_MANAGER_ALLOW_ROOTFUL_RUNTIME=true` 与 `DOCUMENT_MANAGER_ALLOW_MUTABLE_IMAGE=true` 是这个 Compose 测试配置的显式开关。代码默认拒绝 rootful Docker 和浮动标签，只有 `document-manager` 读取这两个开关。

## 日常命令

```bash
# 状态和日志
docker compose ps
docker compose logs -f platform
docker compose logs -f document-manager

# 更新代码后重建并启动
git pull
docker compose up -d --build

# 停止服务但保留数据
docker compose down

# 再次启动
docker compose up -d
```

不要执行 `docker compose down -v`。`-v` 会删除数据库、任务、会话文件、Bot 媒体和文档工作数据。

## 原理

Compose 一次创建并管理五个服务：`postgres` 存平台数据，`platform` 提供 API、调度和 Worker，`bot-poller` 负责 IM，`web` 提供网页入口，`document-manager` 负责 DOCX 任务。Platform 会等待 PostgreSQL 与 Bot Poller 健康后启动；Document Manager 会等待 PostgreSQL 健康后启动。两者都会自动执行数据库 migration。

Compose 还会自动创建六个命名卷：数据库、平台运行数据、平台配置、会话文件、Bot 媒体和文档工作目录。它们由 Docker 管理，不出现在项目目录中；容器重建或 `docker compose down` 都不会删除数据。
