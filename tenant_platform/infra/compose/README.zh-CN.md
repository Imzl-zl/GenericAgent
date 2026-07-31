# GenericAgent 安全镜像部署手册（单机 Linux）

本文档面向自行操作部署的人。目标是：应用部分使用 Docker Compose 一次启动，文档任务仍使用独立的 rootless Docker 安全边界。

> 当前是单机部署，不是 Kubernetes/高可用方案。第一次部署建议使用一台无生产数据的 Linux staging 主机完整演练。

## 1. 最终拓扑

| 组件 | 运行方式 | 持久化 | 是否持有容器 runtime socket |
|---|---|---|---|
| Web/Nginx | Compose 容器 | 无 | 否 |
| Platform + Python 子 Worker | 同一个 Compose 容器 | `platform_runtime`、`platform_config`、`session_files` | 否 |
| Bot Poller | Compose 容器 | `bot_media`，只读 `session_files` | 否 |
| PostgreSQL | Compose 容器 | `postgres_data` | 否 |
| Document Manager | 宿主 systemd 服务 | `/var/lib/ga/documents` | **是，仅独占 `ga-document` rootless socket** |
| 文档 job | `ga-document` 的 rootless Docker 临时容器 | 无；输入 stdin、产物 stdout/PostgreSQL | 否 |

Web 是唯一用户入口。默认只发布宿主 `127.0.0.1:8088`；PostgreSQL 只发布 `127.0.0.1:55432`，供宿主 Document Manager 使用。

## 2. 为什么不能做成一个镜像

可以用一个 Dockerfile 的多个 build target 生成多个镜像，但不能把 Web、Platform、Bot、PostgreSQL 和 Document Manager 当成一个进程容器运行：

1. 一个容器会共享 PID、环境变量、文件挂载和失败域，任一组件漏洞都更容易读取其他组件密钥。
2. Document Manager 必须能调用 Docker/Podman API。把同一个 socket 挂进整套应用容器，等于允许它控制 Platform 和 PostgreSQL 容器。
3. Docker-in-Docker 通常需要 `privileged`、额外 capability 或 cgroup/device 挂载，与当前安全约束冲突。
4. PostgreSQL、Bot 媒体和 Platform session 文件的备份/恢复周期不同，拆分后才能独立处理。

因此本部署包采用“四个应用容器 + 一个宿主 Document Manager”的混合方式。Document Manager 的 daemon 只包含文档 job，不包含应用栈。

## 3. 容器里能不能执行命令和操作文件

能。镜像只是把程序和依赖固定下来，不会禁止程序执行命令或读写文件：

- Platform 镜像中的 Go 进程会启动镜像内的 Python Worker 子进程。
- 文档镜像执行固定 `/usr/local/bin/ga-document-tool`；Document Manager 通过容器 API 启动命令。
- 只读 rootfs 只禁止修改镜像层。`tmpfs` 和明确挂载的 volume 仍可写。
- Platform checkpoint、session 文件、Bot 媒体、PostgreSQL 数据分别写入命名卷。
- 文档 job 的临时文件写入容器 `/tmp` tmpfs；容器销毁后自动消失。
- 文档 job 不挂宿主目录。输入从 stdin 进入，产物从 stdout 返回并写入 PostgreSQL/交付目录。

不要靠 `docker exec` 在运行容器里安装软件或改代码，这些修改在重建容器后会丢失。需要新命令或依赖时应重建镜像。

## 4. 服务器需要什么

推荐起步配置：

- Ubuntu 22.04/24.04 或同等 Linux，x86_64；
- systemd，cgroup v2；
- 4 vCPU、8 GiB RAM、至少 40 GiB 可用磁盘；
- Docker Engine + Docker Compose v2，用于应用栈；
- rootless Docker 所需的 `uidmap`、`dbus-user-session`、`slirp4netns`、`fuse-overlayfs`；
- Git、curl、jq、openssl、rsync；
- 可推送/拉取 OCI 镜像的 registry。文档镜像正式运行时必须使用 `repository@sha256:<digest>`；
- 可选域名和 Caddy/Nginx，用于公网 HTTPS；
- 独立备份目录，不能放在 PostgreSQL 数据卷内。

检查：

```bash
uname -m
stat -fc %T /sys/fs/cgroup
docker version
docker compose version
```

`stat` 必须返回 `cgroup2fs`。

## 5. 获取固定版本源码

```bash
sudo install -d -o root -g root -m 0755 /opt/genericagent
sudo git clone https://github.com/Imzl-zl/GenericAgent.git /opt/genericagent/source
cd /opt/genericagent/source
sudo git checkout <REVIEWED_GIT_SHA>
sudo git rev-parse HEAD
sudo chown -R root:root /opt/genericagent/source
sudo chmod -R go-w /opt/genericagent/source
```

部署时固定一个审查过的 commit，不要直接部署浮动的 `main`。

```bash
cd /opt/genericagent/source/tenant_platform/infra/compose
sudo cp .env.example .env
sudo install -d -o root -g root -m 0700 secrets
sudo install -o root -g root -m 0600 env/platform.env.example secrets/platform.env
sudo install -o root -g root -m 0600 env/bot-poller.env.example secrets/bot-poller.env
sudo install -o root -g root -m 0600 env/postgres.env.example secrets/postgres.env
sudo chown root:root .env
sudo chmod 600 .env
```

## 6. 生成和填写密钥

生成值时不要把输出粘贴到工单、聊天或 Git：

```bash
openssl rand -hex 32  # PostgreSQL 密码，十六进制便于安全放进 URL
openssl rand -hex 32  # BOT_TOKEN_KEY
openssl rand -hex 32  # BOT_POLLER_API_SECRET
openssl rand -hex 32  # PLATFORM_WEBHOOK_SECRET
openssl rand -hex 32  # PLATFORM_DEV_TOKEN
openssl rand -hex 32  # LLM_PROXY_CAPABILITY_SIGNING_KEY
```

编辑：

```bash
sudoedit /opt/genericagent/source/tenant_platform/infra/compose/secrets/postgres.env
sudoedit /opt/genericagent/source/tenant_platform/infra/compose/secrets/platform.env
sudoedit /opt/genericagent/source/tenant_platform/infra/compose/secrets/bot-poller.env
```

必须满足：

- `postgres.env` 的用户、密码和数据库与 `platform.env` 的 `DATABASE_URL` 完全一致；
- Platform 与 Bot 的 `BOT_POLLER_API_SECRET` 相同；
- Platform 与 Bot 的 `PLATFORM_WEBHOOK_SECRET` 相同；
- `BOT_TOKEN_KEY` 是 64 个十六进制字符；
- 所有 `CHANGE_ME` 已替换；
- 三个 secret 文件及 `.env` 都是 `root:root 0600`，从文件所在目录到 `/` 的父链均为 `root:root` 且 group/other 不可写。

当前 Platform 使用本地 Python Worker 子进程，所以镜像命令保留 `--dev-loopback`。它并不表示可以使用默认密钥；上述生产密钥仍然必须填写，也不要水平扩容多个 Platform 副本。

## 7. 构建应用镜像

首次在目标机本地构建：

```bash
cd /opt/genericagent/source/tenant_platform/infra/compose
sudo docker compose --env-file .env -f compose.yaml build --pull
sudo docker image inspect \
  genericagent-platform:local \
  genericagent-bot-poller:local \
  genericagent-web:local >/dev/null
```

正式生产建议把三个应用镜像推入私有 registry，拉取后把 `.env` 中的镜像值改成 `repository@sha256:<digest>`。PostgreSQL 镜像也应固定 digest；本 profile 固定官方 Alpine image 的 `70:70` 身份，升级 PostgreSQL digest 前必须确认镜像内 `postgres` 仍是 UID/GID 70，并重新做空卷初始化和恢复演练。

本地 staging 尚未推 registry 时，preflight 需要显式允许本地 tag：

```bash
sudo env \
  GA_COMPOSE_DEPLOY_PREFLIGHT=1 \
  GA_COMPOSE_ALLOW_MUTABLE_IMAGES=1 \
  bash ./compose-preflight.sh
sudo docker compose --env-file .env -f compose.yaml up -d --wait
```

mutable tag 仅用于这类手工 staging smoke；不要用 systemd unit 启动它们。正式 systemd 生命周期会再次运行 preflight，且不允许 mutable tag。

正式部署不要设置 `GA_COMPOSE_ALLOW_MUTABLE_IMAGES`：

```bash
sudo env GA_COMPOSE_DEPLOY_PREFLIGHT=1 bash ./compose-preflight.sh
```

preflight 不会打印 secret 内容；它检查 root-owned 配置信任链、服务集合、私有配置一致性、镜像策略、四个服务的非 root/只读 rootfs/cap-drop/no-new-privileges/资源限制、禁止 socket/privileged/host namespace、固定 delivery GID，以及端口只绑定 loopback。

## 8. 导出 Document Manager 二进制

不需要在服务器安装 Go，使用 Platform Dockerfile 的导出 target：

```bash
cd /opt/genericagent/source
sudo rm -rf /tmp/ga-document-manager-export
sudo docker build \
  -f tenant_platform/infra/compose/platform.Dockerfile \
  --target document-manager-export \
  --output type=local,dest=/tmp/ga-document-manager-export \
  .
sudo install -d -o root -g root -m 0755 /opt/ga/bin /opt/ga/migrations
sudo install -o root -g root -m 0755 \
  /tmp/ga-document-manager-export/ga-document-manager \
  /opt/ga/bin/document-manager
sudo rsync -a --delete \
  tenant_platform/infra/postgres/migrations/ \
  /opt/ga/migrations/
sudo chown -R root:root /opt/ga/migrations
sudo chmod -R go-w /opt/ga/migrations
```

`GA_MIGRATIONS_DIR=/opt/ga/migrations` 使脱离源码树的二进制能够加载同版本 migration。

## 9. 配置独立 rootless Document Runtime

创建唯一的服务账号；不要把它加入 `docker` 组：

```bash
sudo useradd --system --user-group --create-home --home-dir /var/lib/ga-document ga-document
getent subuid ga-document
getent subgid ga-document
sudo loginctl enable-linger ga-document
sudo install -d -o root -g root -m 0755 /var/lib/ga
sudo install -d -o ga-document -g ga-document -m 0750 /var/lib/ga/documents
```

若 `subuid/subgid` 没有记录，先按发行版方式为 `ga-document` 分配独立且不重叠的 65536 UID/GID 范围。

按照 Docker 官方 rootless 安装方式，以 `ga-document` 用户运行 `dockerd-rootless-setuptool.sh install`，然后：

```bash
DOCUMENT_UID="$(id -u ga-document)"
sudo -u ga-document -H env XDG_RUNTIME_DIR="/run/user/$DOCUMENT_UID" \
  systemctl --user enable --now docker.service
sudo -u ga-document -H env \
  XDG_RUNTIME_DIR="/run/user/$DOCUMENT_UID" \
  DOCKER_HOST="unix:///run/user/$DOCUMENT_UID/docker.sock" \
  docker info
```

输出必须包含 `rootless`、受限 seccomp 和 cgroup v2。

### 构建并固定文档镜像

文档镜像必须进入 registry，因为 Document Manager 拒绝 tag 和本地 image ID：

```bash
cd /opt/genericagent/source
DOCUMENT_UID="$(id -u ga-document)"
REVISION="$(git rev-parse --short=12 HEAD)"
sudo -u ga-document -H env \
  XDG_RUNTIME_DIR="/run/user/$DOCUMENT_UID" \
  DOCKER_HOST="unix:///run/user/$DOCUMENT_UID/docker.sock" \
  docker build -f tenant_platform/document-image/Dockerfile \
  -t registry.example/genericagent/document-tool:"$REVISION" .
sudo -u ga-document -H env \
  XDG_RUNTIME_DIR="/run/user/$DOCUMENT_UID" \
  DOCKER_HOST="unix:///run/user/$DOCUMENT_UID/docker.sock" \
  docker push registry.example/genericagent/document-tool:"$REVISION"
sudo -u ga-document -H env \
  XDG_RUNTIME_DIR="/run/user/$DOCUMENT_UID" \
  DOCKER_HOST="unix:///run/user/$DOCUMENT_UID/docker.sock" \
  docker pull registry.example/genericagent/document-tool@sha256:<REGISTRY_DIGEST>
```

## 10. 配置 Document Manager

```bash
sudo install -d -o root -g root -m 0755 /etc/ga
sudo install -o root -g ga-document -m 0640 \
  /opt/genericagent/source/tenant_platform/infra/deploy/document-manager.env.example \
  /etc/ga/document-manager.env
sudoedit /etc/ga/document-manager.env
```

关键值：

```dotenv
DATABASE_URL=postgres://genericagent:<DB_PASSWORD>@127.0.0.1:55432/genericagent?sslmode=disable
GA_MIGRATIONS_DIR=/opt/ga/migrations
DOCUMENT_MANAGER_RUNTIME_BINARY=docker
DOCUMENT_MANAGER_IMAGE=registry.example/genericagent/document-tool@sha256:<REGISTRY_DIGEST>
DOCUMENT_MANAGER_WORK_ROOT=/var/lib/ga/documents
XDG_RUNTIME_DIR=/run/user/<GA_DOCUMENT_UID>
DOCKER_HOST=unix:///run/user/<GA_DOCUMENT_UID>/docker.sock
```

数据库密码应使用前面生成的十六进制值。不要把 URL 放到命令行；Compose profile 的 systemd unit 让程序从私有 env 文件读取。

## 11. 首次部署

先启动应用栈：

```bash
cd /opt/genericagent/source/tenant_platform/infra/compose
sudo install -o root -g root -m 0644 genericagent-compose.service /etc/systemd/system/genericagent-compose.service
sudo install -o root -g root -m 0644 ga-document-manager.service /etc/systemd/system/ga-document-manager.service
sudo systemd-analyze verify /etc/systemd/system/genericagent-compose.service /etc/systemd/system/ga-document-manager.service
sudo systemctl daemon-reload
sudo systemctl enable --now genericagent-compose.service
sudo systemctl status --no-pager genericagent-compose.service
curl --fail http://127.0.0.1:8088/healthz
```

Compose 容器的 `restart` 固定为 `no`；Docker daemon 不能绕过 preflight 自行拉起它们。`genericagent-compose.service` 是唯一开机/重载 owner，并通过 `PartOf=docker.service` 跟随 daemon 重启。

再启动 Document Manager；unit 会先以 root 强制执行 runtime preflight，再降权到 `ga-document`：

```bash
sudo env GA_DOCUMENT_RUNTIME_PREFLIGHT=1 bash ./document-runtime-preflight.sh
sudo systemctl enable --now ga-document-manager.service
sudo systemctl status --no-pager ga-document-manager.service
```

查看容器和日志：

```bash
sudo docker compose --env-file .env -f compose.yaml ps
sudo docker compose --env-file .env -f compose.yaml logs --tail=200 platform bot-poller web postgres
sudo journalctl -u ga-document-manager --since '10 minutes ago' --no-pager
```

## 12. 访问 Web

默认只监听 `127.0.0.1:8088`。临时访问可以用 SSH 隧道：

```bash
ssh -L 8088:127.0.0.1:8088 user@server
```

浏览器打开 `http://127.0.0.1:8088`。

公网部署建议在宿主安装 Caddy：

```caddyfile
agent.example.com {
    reverse_proxy 127.0.0.1:8088
}
```

不要把 `GA_HTTP_BIND` 改成 `0.0.0.0` 直接暴露无 TLS 服务；preflight 会拒绝这种配置。

## 13. 必做验收

### 应用控制面

先通过 Web 的管理员页面创建邀请码，再用普通用户注册/登录，取得当前有效的 tenant session token。管理员 token 与 tenant token 不是同一个凭据；不能用 `PLATFORM_DEV_TOKEN` 代替 `GA_USER_TOKEN`。

```bash
cd /opt/genericagent/source/tenant_platform/infra/compose
read -rsp 'Platform admin token: ' PLATFORM_DEV_TOKEN; echo
read -rsp 'Approved tenant session token: ' GA_USER_TOKEN; echo
export PLATFORM_DEV_TOKEN GA_USER_TOKEN
export GA_PLATFORM_BASE_URL=http://127.0.0.1:8088
python3 /opt/genericagent/source/tenant_platform/tests/smoke/document_pool_admin.py
unset PLATFORM_DEV_TOKEN GA_USER_TOKEN GA_PLATFORM_BASE_URL
```

### 真实文档容器

```bash
DOCUMENT_UID="$(id -u ga-document)"
sudo -u ga-document -H env \
  XDG_RUNTIME_DIR="/run/user/$DOCUMENT_UID" \
  DOCKER_HOST="unix:///run/user/$DOCUMENT_UID/docker.sock" \
  GA_DOCUMENT_DOCKER_SMOKE=1 \
  GA_DOCUMENT_RUNTIME_BINARY=docker \
  GA_DOCUMENT_SMOKE_IMAGE='registry.example/genericagent/document-tool@sha256:<REGISTRY_DIGEST>' \
  GA_DOCUMENT_SECCOMP_PROFILE=builtin \
  /opt/genericagent/source/tenant_platform/tests/smoke/document_pool_docker.sh
```

最后从管理 Web 修改一次有界文档池配置，发送微信 DOCX 请求，下载并打开文件，确认 job 终态后对应 single-use 容器已销毁。

## 14. 完整备份与恢复演练

PostgreSQL 是业务状态真值，但 `platform_runtime`、`platform_config`、`session_files` 和 `bot_media` 仍包含恢复服务所需的 checkpoint、运行配置和交付文件。四个文件卷、私有 operator 配置与数据库 dump 必须进入同一个停写备份集。`/var/lib/ga/documents` 只允许排空后的临时文档工作目录，不作为 artifact 备份源。

先禁用文档池、等待 active job 为零，再执行：

```bash
cd /opt/genericagent/source/tenant_platform/infra/compose
BACKUP_DIR="/var/backups/genericagent/$(date -u +%Y%m%dT%H%M%SZ)"
sudo install -d -o root -g root -m 0700 "$BACKUP_DIR"
sudo systemctl stop ga-document-manager.service
sudo docker compose --env-file .env -f compose.yaml stop web bot-poller platform
sudo sh -ceu 'cd "$1"; docker compose --env-file .env -f compose.yaml exec -T postgres sh -ceu '\''exec pg_dump -U "$POSTGRES_USER" -d "$POSTGRES_DB" --format=custom'\'' > "$2/postgres.dump"' sh "$PWD" "$BACKUP_DIR"
sudo systemctl stop genericagent-compose.service
```

应用停写后归档四个文件卷和私有配置。helper image 取自已解析、digest-pinned 的 PostgreSQL image，不临时拉取新 tag：

```bash
CONFIG_JSON="$(mktemp)"
sudo docker compose --env-file .env -f compose.yaml config --format json > "$CONFIG_JSON"
HELPER_IMAGE="$(jq -r '.services.postgres.image' "$CONFIG_JSON")"
for LOGICAL_VOLUME in platform_runtime platform_config session_files bot_media; do
  VOLUME_NAME="$(jq -r --arg name "$LOGICAL_VOLUME" '.volumes[$name].name' "$CONFIG_JSON")"
  sudo docker run --rm --user 0:0 --network none --read-only \
    --cap-drop ALL --cap-add DAC_READ_SEARCH --security-opt no-new-privileges \
    --tmpfs /tmp:rw,noexec,nosuid,nodev,size=16m \
    --mount "type=volume,source=$VOLUME_NAME,target=/source,readonly" \
    --mount "type=bind,source=$BACKUP_DIR,target=/backup" \
    "$HELPER_IMAGE" tar -C /source -czf "/backup/$LOGICAL_VOLUME.tar.gz" .
done
rm -f "$CONFIG_JSON"
sudo tar -C /opt/genericagent/source/tenant_platform/infra/compose \
  -czf "$BACKUP_DIR/operator-config.tar.gz" .env secrets
sudo cp /etc/ga/document-manager.env "$BACKUP_DIR/document-manager.env"
sudo tar -C / -czf "$BACKUP_DIR/release-host-files.tar.gz" \
  opt/ga/bin/document-manager opt/ga/migrations \
  etc/systemd/system/genericagent-compose.service \
  etc/systemd/system/ga-document-manager.service
sudo sh -ceu 'cd "$1"; sha256sum postgres.dump *.tar.gz document-manager.env > SHA256SUMS' sh "$BACKUP_DIR"
sudo systemctl start genericagent-compose.service
sudo systemctl start ga-document-manager.service
```

恢复必须先在 staging 演练。空白主机应先 checkout 备份记录的 reviewed commit，再从 `operator-config.tar.gz` 和 `document-manager.env` 恢复 root-owned 配置；已有主机则先保留当前配置副本。恢复窗口中再次禁用并排空文档池，然后校验、停服务并移走当前数据；下列命令会不可逆替换现有卷，所以先另做当前状态备份：

```bash
cd /opt/genericagent/source/tenant_platform/infra/compose
BACKUP_DIR=/var/backups/genericagent/<BACKUP_TIMESTAMP>
sudo sh -ceu 'cd "$1"; sha256sum -c SHA256SUMS' sh "$BACKUP_DIR"
sudo systemctl stop ga-document-manager.service genericagent-compose.service
sudo docker compose --env-file .env -f compose.yaml down --remove-orphans
CONFIG_JSON="$(mktemp)"
sudo docker compose --env-file .env -f compose.yaml config --format json > "$CONFIG_JSON"
for LOGICAL_VOLUME in postgres_data platform_runtime platform_config session_files bot_media; do
  VOLUME_NAME="$(jq -r --arg name "$LOGICAL_VOLUME" '.volumes[$name].name' "$CONFIG_JSON")"
  sudo docker volume rm "$VOLUME_NAME" 2>/dev/null || true
done
sudo docker compose --env-file .env -f compose.yaml create
sudo docker compose --env-file .env -f compose.yaml rm -sf
HELPER_IMAGE="$(jq -r '.services.postgres.image' "$CONFIG_JSON")"
for LOGICAL_VOLUME in platform_runtime platform_config session_files bot_media; do
  VOLUME_NAME="$(jq -r --arg name "$LOGICAL_VOLUME" '.volumes[$name].name' "$CONFIG_JSON")"
  sudo docker run --rm --user 0:0 --network none --read-only \
    --cap-drop ALL --cap-add CHOWN --cap-add DAC_OVERRIDE --cap-add FOWNER \
    --security-opt no-new-privileges \
    --tmpfs /tmp:rw,noexec,nosuid,nodev,size=16m \
    --mount "type=volume,source=$VOLUME_NAME,target=/source" \
    --mount "type=bind,source=$BACKUP_DIR,target=/backup,readonly" \
    "$HELPER_IMAGE" sh -ceu 'exec tar -C /source -xzf "/backup/$1.tar.gz"' sh "$LOGICAL_VOLUME"
done
rm -f "$CONFIG_JSON"
sudo docker compose --env-file .env -f compose.yaml up -d --wait postgres
sudo cat "$BACKUP_DIR/postgres.dump" | sudo docker compose --env-file .env -f compose.yaml exec -T postgres \
  sh -ceu 'exec pg_restore -U "$POSTGRES_USER" -d "$POSTGRES_DB" --exit-on-error --no-owner --no-privileges'
sudo systemctl start genericagent-compose.service
sudo systemctl start ga-document-manager.service
```

恢复后必须验证登录、历史 task/checkpoint、入站附件、已有 DOCX 下载与微信文件交付；仅有备份文件或 `pg_restore --list` 不算恢复演练完成。operator config 含密钥，只能存放在 root-only 加密备份介质中。

## 15. 升级

1. 在管理员设置中把文档池 `enabled=false`。
2. 等待活动文档 job 进入终态。
3. 执行第 14 节完整停写备份（PostgreSQL、四个文件卷、operator 配置和宿主 release 文件）。
4. 用 `sudo git fetch` 并 checkout 新的 reviewed commit，随后再次执行 `sudo chown -R root:root /opt/genericagent/source && sudo chmod -R go-w /opt/genericagent/source`。
5. 构建/拉取新镜像；正式环境更新 `.env` 中的 digest。
6. 重新导出 Document Manager，并同步同版本 migrations。
7. 重跑两个 preflight。
8. 重载应用栈并重启 Manager：

```bash
sudo systemctl reload genericagent-compose.service
sudo systemctl restart ga-document-manager.service
```

9. 重跑管理员、真实文档容器、浏览器和微信 DOCX smoke，最后重新启用文档池。

不要使用 `docker compose down -v`；`-v` 会删除 PostgreSQL 和其他持久卷。

## 16. 回滚

1. 禁用文档池并排空活动 job。
2. 把 `.env` 的 Platform/Bot/Web/PostgreSQL 镜像 digest 改回上一版本。
3. 恢复上一版本 `/opt/ga/bin/document-manager`、`/opt/ga/migrations` 和两个 systemd unit。
4. 执行：

```bash
sudo systemctl reload genericagent-compose.service
sudo systemctl restart ga-document-manager.service
```

5. 重跑两个 preflight 和所有 smoke。

数据库 migration 是 additive，普通应用回滚不删除新表。只有明确的数据回滚事故流程才允许恢复 PostgreSQL 备份，并且必须先再做一次当前数据备份。

## 17. 常用故障检查

```bash
sudo docker compose --env-file .env -f compose.yaml ps
sudo docker compose --env-file .env -f compose.yaml logs --tail=300 platform
sudo docker compose --env-file .env -f compose.yaml logs --tail=300 bot-poller
sudo journalctl -u ga-document-manager -n 300 --no-pager
sudo -u ga-document -H env \
  XDG_RUNTIME_DIR="/run/user/$(id -u ga-document)" \
  DOCKER_HOST="unix:///run/user/$(id -u ga-document)/docker.sock" \
  docker ps -a
```

- Web 502：先检查 Platform `127.0.0.1:8080` health 和 migration 日志。
- Platform 启动失败：检查 PostgreSQL health、`DATABASE_URL`、`GA_MIGRATIONS_DIR` 和 secret 是否仍有占位符。
- Bot 无响应：检查两个共享 secret 是否一致、Bot 到 `http://platform:8088/v1/im/webhook` 是否可达。
- Document Manager 启动失败：检查 rootless socket、digest image、cgroup v2、seccomp 和 `/var/lib/ga/documents` owner。
- 文件重启后消失：确认写入的是命名卷/数据库，而不是镜像可写层或 `/tmp`。

## 18. 已知依赖公告

当前 Web 精确锁定 npm registry 最新的 `react-router-dom 7.18.2`。`npm audit --omit=dev` 仍报告 `GHSA-qwww-vcr4-c8h2`：受影响面是 React Router 的 RSC/server-action 请求处理，而本镜像是 Vite 静态 SPA，由 Nginx 提供静态文件，不启用 React Server Components、SSR 或 server action，因此该路径不在本部署中执行。registry 当前尚未提供公告要求的 8.3.0 修复版本。

每次升级前重新运行 audit；一旦官方发布兼容修复，升级并重新执行 Web build/lint/browser smoke。不要按 audit 的临时建议降级到 7.11.0，该版本命中更多历史 XSS/RCE 公告。

## 19. 明确禁止

- 不要把 `/var/run/docker.sock` 或任何 rootless socket 挂进 Platform/Web/Bot/PostgreSQL。
- 不要把 Document Manager 加进应用 Compose daemon。
- 不要使用 `privileged`、Docker-in-Docker、host PID/IPC。
- 不要把 secret 写进 Dockerfile、Compose command、Git 或镜像 build args。
- 不要给文档 job 添加网络或宿主目录 mount。
- 不要使用 `latest` 或 tag 作为正式文档镜像标识。
