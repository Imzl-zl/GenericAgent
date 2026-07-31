# GenericAgent 1Panel 快速启动

这个部署包用 1Panel 启动 Web、Platform、Bot Poller 和 PostgreSQL，并同时提供宿主机 Document Manager 的环境模板和 systemd unit。Document Manager 不是 1Panel 容器；由你在宿主机安装 `/opt/ga/bin/document-manager` 后，使用本包的 unit 启动。

## 需要的文件

在 1Panel 创建“容器编排”项目时使用：

- 编排文件：[`compose.1panel.yaml`](compose.1panel.yaml)
- 环境变量模板：[`env/1panel.env.example`](env/1panel.env.example)
- 宿主 Document Manager 模板：[`document-manager.1panel.env.example`](document-manager.1panel.env.example)
- 宿主 Document Manager unit：[`ga-document-manager-1panel.service`](ga-document-manager-1panel.service)

1Panel 编排不需要 `secrets/` 目录，不需要 `compose.yaml`。Document Manager 仍需要宿主机 systemd，因为它使用独立的 rootless Docker runtime。

## 1. 填 1Panel 环境变量

将 `env/1panel.env.example` 的全部 `KEY=VALUE` 填入 1Panel 的环境变量页面。替换以下内容：

- `GA_PLATFORM_IMAGE`、`GA_BOT_POLLER_IMAGE`、`GA_WEB_IMAGE`：当前模板已填入本次发布的 `repository@sha256:...` 值；下次发布时更新这三条。
- Document Manager 镜像：在 `document-manager.1panel.env.example` 中，使用同一次发布的第四条 `DOCUMENT_MANAGER_IMAGE`；下次发布时单独更新该文件。
- `POSTGRES_PASSWORD`：生成一个新值，例如 `openssl rand -hex 32`。
- `DATABASE_URL`：把其中密码替换为与 `POSTGRES_PASSWORD` 完全相同的值。
- `BOT_TOKEN_KEY`、`BOT_POLLER_API_SECRET`、`PLATFORM_WEBHOOK_SECRET`、`PLATFORM_DEV_TOKEN`、`LLM_PROXY_CAPABILITY_SIGNING_KEY`：各生成一个独立随机值。
- `BOT_POLLER_API_SECRET` 只填一次，Platform 和 Bot 会使用同一个环境变量值。
- `PLATFORM_WEBHOOK_SECRET` 只填一次，Platform 和 Bot 会使用同一个环境变量值。

不要把任何密码或 Token 写入 `compose.1panel.yaml`，不要提交填好的环境变量文件到 Git。

## 2. 创建编排

1. 在 1Panel 打开“容器编排”，新建项目。
2. 将 `compose.1panel.yaml` 全部粘贴到编排内容。
3. 将上一步准备的全部环境变量填入环境变量页面。
4. 启动项目，等待 `postgres`、`bot-poller`、`platform` 和 `web` 全部运行。

首次启动时，Platform 会等待 PostgreSQL healthcheck。不要手动创建数据库表。

## 3. 检查

服务器终端执行：

```bash
curl --fail http://127.0.0.1:8088/healthz
docker ps
```

默认端口只绑定服务器本机 `127.0.0.1:8088`。通过 1Panel 的网站/反向代理功能配置 HTTPS；不要直接把未加密的 8088 端口暴露到公网。

## 4. Document Manager 宿主服务

Document Manager 是 DOCX 功能必需组件。本包已经给出它的镜像引用、环境模板和 1Panel 专用 unit；你安装好 `/opt/ga/bin/document-manager`、rootless Docker、`ga-document` 用户和 `/opt/ga/migrations/` 后执行：

```bash
sudo install -D -o root -g root -m 0644 \
  /opt/genericagent/source/tenant_platform/infra/compose/ga-document-manager-1panel.service \
  /etc/systemd/system/ga-document-manager-1panel.service
sudo install -D -o root -g ga-document -m 0640 \
  /opt/genericagent/source/tenant_platform/infra/compose/document-manager.1panel.env.example \
  /etc/ga/document-manager.env
sudoedit /etc/ga/document-manager.env
sudo systemctl daemon-reload
sudo systemctl enable --now ga-document-manager-1panel.service
```

将 `/etc/ga/document-manager.env` 里的 `DATABASE_URL` 密码替换为 1Panel `POSTGRES_PASSWORD` 的同一个值；如果改过 1Panel 的 `GA_POSTGRES_PORT`，也要把这个 URL 内的 `55432` 改成相同端口。将 `XDG_RUNTIME_DIR`、`DOCKER_HOST` 中的 `1000` 替换为真实 `ga-document` UID。该服务启动前会检查 1Panel Platform 健康接口、rootless Docker、cgroup v2、镜像 digest 和文件权限。
