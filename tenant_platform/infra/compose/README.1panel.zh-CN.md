# GenericAgent 1Panel 快速启动

这个 profile 用于先在 1Panel 启动 Web、Platform、Bot Poller 和 PostgreSQL。它不包含 Document Manager；生成 DOCX 的请求会等待后续单独安装的文档服务。

## 需要的文件

在 1Panel 创建“容器编排”项目时使用：

- 编排文件：[`compose.1panel.yaml`](compose.1panel.yaml)
- 环境变量模板：[`env/1panel.env.example`](env/1panel.env.example)

不需要 `secrets/` 目录，不需要 `compose.yaml`，不需要 systemd。

## 1. 填 1Panel 环境变量

将 `env/1panel.env.example` 的全部 `KEY=VALUE` 填入 1Panel 的环境变量页面。替换以下内容：

- `GA_PLATFORM_IMAGE`、`GA_BOT_POLLER_IMAGE`、`GA_WEB_IMAGE`：填写 `release_images.py` 输出的三条 `repository@sha256:...` 值。
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

## 当前范围

这个 1Panel profile 可以登录、配置 LLM、使用 Platform、Bot 和 PostgreSQL。它不启动 Document Manager，因此暂时不要测试 DOCX 导出。后续安装 Document Manager 时，需要使用文档镜像 digest 和单独的 rootless Docker 配置。
