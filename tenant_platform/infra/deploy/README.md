# Secure Document Platform Deployment

This directory documents the all-systemd deployment profile. For the simpler self-service image profile, use [`../compose/README.zh-CN.md`](../compose/README.zh-CN.md): Web, Platform+child Workers, Bot Poller and PostgreSQL run under hardened Compose, while Document Manager remains a host systemd service with its own rootless runtime. Both profiles preserve the rule that only Document Manager can reach Docker/Podman. Run target-host commands on the actual Linux host; a local Docker Desktop pass is not equivalent to the rootless/cgroup-v2 gate.

## 1. Prerequisites

- Linux with systemd and the unified cgroup v2 hierarchy.
- PostgreSQL reachable from the `ga-platform`, `ga-document` and optional `ga-llm` service accounts.
- Rootless Docker or Podman owned only by the non-root `ga-document` account.
- `/opt/ga`, `/etc/ga`, `/var/lib/ga/runtime`, `/var/lib/ga/documents` and `/var/lib/ga/bot-media` on local filesystems.
- Python 3.11+ and the Worker dependencies installed for `/usr/bin/python3`.
- A registry available to pin the document image by digest.

Create distinct service accounts, the delivery-only shared group, and directories once:

```bash
sudo groupadd --system ga-delivery
sudo useradd --system --user-group --create-home --home-dir /var/lib/ga-platform ga-platform
sudo useradd --system --user-group --create-home --home-dir /var/lib/ga-document ga-document
sudo useradd --system --user-group --create-home --home-dir /var/lib/ga-bot ga-bot
sudo useradd --system --user-group --create-home --home-dir /var/lib/ga-llm ga-llm
sudo usermod --groups ga-delivery ga-platform
sudo usermod --groups ga-delivery ga-bot
sudo usermod --groups '' ga-document
sudo usermod --groups '' ga-llm
sudo install -d -o root -g root -m 0755 /var/lib/ga
sudo install -d -o ga-platform -g ga-platform -m 0750 /var/lib/ga/runtime
sudo install -d -o ga-platform -g ga-delivery -m 2750 /var/lib/ga/runtime/session_files
sudo install -d -o ga-document -g ga-document -m 0750 /var/lib/ga/documents
sudo install -d -o ga-bot -g ga-delivery -m 2750 /var/lib/ga/bot-media
sudo install -d -o root -g root -m 0755 /etc/ga
sudo install -d -o ga-platform -g ga-platform -m 0750 /etc/ga/config
sudo install -d -o root -g root -m 0755 /opt/ga/bin /opt/ga/policy /opt/ga/migrations
```

`ga-delivery` grants only filesystem read/traverse between Platform session files and Bot Poller media. `ga-document` and `ga-llm` must not belong to it. Never reuse a service UID: per-unit mount namespaces are not a security boundary between processes with the same UID because `/proc/<pid>/root` can expose peer namespaces.

Configure the rootless runtime as `ga-document`. For a user-systemd runtime, enable lingering so it survives logout, then enable the actual API service used by Document Manager:

```bash
sudo loginctl enable-linger ga-document
DOCUMENT_UID="$(id -u ga-document)"
# Rootless Docker, after running dockerd-rootless-setuptool.sh as ga-document:
sudo -u ga-document -H env XDG_RUNTIME_DIR="/run/user/$DOCUMENT_UID" systemctl --user enable --now docker.service
sudo -u ga-document -H env XDG_RUNTIME_DIR="/run/user/$DOCUMENT_UID" DOCKER_HOST="unix:///run/user/$DOCUMENT_UID/docker.sock" docker info
# Or rootless Podman:
sudo -u ga-document -H env XDG_RUNTIME_DIR="/run/user/$DOCUMENT_UID" systemctl --user enable --now podman.socket
sudo -u ga-document -H env XDG_RUNTIME_DIR="/run/user/$DOCUMENT_UID" CONTAINER_HOST="unix:///run/user/$DOCUMENT_UID/podman/podman.sock" podman info
```

Verify exactly one selected rootless socket exists. Do not continue with a direct Podman CLI fallback; the hardened Document Manager unit only binds the API sockets.

Do not add any GA service account to Docker/Podman groups and do not expose a rootful socket.

## 2. Build And Pin

Build Linux binaries from the reviewed revision:

```bash
cd /opt/ga/source/tenant_platform/backend-go
CGO_ENABLED=0 go build -trimpath -o /tmp/ga-platform ./cmd/platform
CGO_ENABLED=0 go build -trimpath -o /tmp/ga-document-manager ./cmd/document-manager
CGO_ENABLED=0 go build -trimpath -o /tmp/ga-llm-proxy ./cmd/llm-proxy
sudo install -o root -g root -m 0755 /tmp/ga-platform /opt/ga/bin/platform
sudo install -o root -g root -m 0755 /tmp/ga-document-manager /opt/ga/bin/document-manager
sudo install -o root -g root -m 0755 /tmp/ga-llm-proxy /opt/ga/bin/llm-proxy
```

Build and push the scratch document image, then resolve the immutable repository digest:

```bash
cd /opt/ga/source
DOCUMENT_UID="$(id -u ga-document)"
sudo -u ga-document -H env XDG_RUNTIME_DIR="/run/user/$DOCUMENT_UID" DOCKER_HOST="unix:///run/user/$DOCUMENT_UID/docker.sock" docker build -f tenant_platform/document-image/Dockerfile -t registry.example/ga-document:REVISION .
sudo -u ga-document -H env XDG_RUNTIME_DIR="/run/user/$DOCUMENT_UID" DOCKER_HOST="unix:///run/user/$DOCUMENT_UID/docker.sock" docker push registry.example/ga-document:REVISION
sudo -u ga-document -H env XDG_RUNTIME_DIR="/run/user/$DOCUMENT_UID" DOCKER_HOST="unix:///run/user/$DOCUMENT_UID/docker.sock" docker pull registry.example/ga-document@sha256:REPLACE_WITH_REGISTRY_DIGEST
```

Only `repository@sha256:<64 lowercase hex>` is accepted. A local image ID or mutable tag is not a deployment identifier.

Install reviewed runtime files:

```bash
sudo cp tenant_platform/contracts/policy/foundation.v1.json /opt/ga/policy/foundation.v1.json
sudo rsync -a --delete tenant_platform/infra/postgres/migrations/ /opt/ga/migrations/
sudo rsync -a --delete tenant_platform/worker-python/ /opt/ga/worker-python/
sudo rsync -a --delete ./ /opt/ga/legacy/
sudo chown -R root:root /opt/ga/policy /opt/ga/migrations /opt/ga/worker-python /opt/ga/legacy
```

## 3. Private Configuration

Start from `platform.env.example`, `document-manager.env.example` and `bot-poller.env.example`. Replace every placeholder; never commit the resulting files.

```bash
sudo install -o root -g ga-platform -m 0640 tenant_platform/infra/deploy/platform.env.example /etc/ga/platform.env
sudo install -o root -g ga-document -m 0640 tenant_platform/infra/deploy/document-manager.env.example /etc/ga/document-manager.env
sudo install -o root -g ga-bot -m 0640 tenant_platform/infra/deploy/bot-poller.env.example /etc/ga/bot-poller.env
sudoedit /etc/ga/platform.env
sudoedit /etc/ga/document-manager.env
sudoedit /etc/ga/bot-poller.env
```

Set `DOCUMENT_MANAGER_IMAGE` to the exact registry digest. Set `XDG_RUNTIME_DIR=/run/user/<ga-document-uid>` and, for Docker, `DOCKER_HOST=unix:///run/user/<ga-document-uid>/docker.sock` in `document-manager.env`; replace the example UID with `id -u ga-document`. For Podman, remove `DOCKER_HOST` and set `CONTAINER_HOST=unix:///run/user/<ga-document-uid>/podman/podman.sock`. Do not put runtime endpoint variables in `platform.env`.

Generate independent 32-byte secrets for `PLATFORM_DEV_TOKEN`, `BOT_POLLER_API_SECRET` and `PLATFORM_WEBHOOK_SECRET`. Put the same Bot Poller API and webhook secret values in `platform.env` and `bot-poller.env`; preflight compares them without printing them. Keep `BOT_POLLER_URL`, `PLATFORM_WEBHOOK_URL`, `BOT_POLLER_LISTEN` and `BOT_POLLER_MEDIA_DIR` at the loopback/fixed paths shipped in the examples.

Back up PostgreSQL before first startup or revision change:

```bash
sudo sh -ceu '
  umask 077
  install -d -o root -g root -m 0700 /var/backups/genericagent
  database_url="$(grep -Em1 "^DATABASE_URL=" /etc/ga/platform.env)"
  database_url="${database_url#*=}"
  pg_dump --format=custom "$database_url" > "/var/backups/genericagent/ga-$(date -u +%Y%m%dT%H%M%SZ).dump"
'
```

## 4. Fail-Closed Preflight

Install the units but do not enable or start them yet:

```bash
sudo cp tenant_platform/infra/systemd/ga-bot-poller.service /etc/systemd/system/
sudo cp tenant_platform/infra/systemd/ga-platform.service /etc/systemd/system/
sudo cp tenant_platform/infra/systemd/ga-document-manager.service /etc/systemd/system/
sudo cp tenant_platform/infra/systemd/ga-worker-manager.service /etc/systemd/system/
sudo cp tenant_platform/infra/systemd/ga-llm-proxy.service /etc/systemd/system/
sudo systemctl daemon-reload
```

Preflight rejects any systemd drop-in, unexpected effective fragment, rootful runtime group membership, peer-readable service namespace, non-fixed work/media path, or group/other-writable service directory. Remove obsolete overrides rather than weakening the reviewed units.

Run preflight as root. It reads the three mutually private environment files, validates the complete filesystem chain and effective systemd configuration, then drops only runtime probes to `ga-document` with `runuser`:

```bash
sudo env \
  GA_DOCUMENT_DEPLOY_PREFLIGHT=1 \
  GA_DOCUMENT_RUNTIME_BINARY=docker \
  GA_PLATFORM_USER=ga-platform \
  GA_DOCUMENT_USER=ga-document \
  GA_BOT_USER=ga-bot \
  GA_LLM_USER=ga-llm \
  GA_DELIVERY_GROUP=ga-delivery \
  GA_DOCUMENT_SMOKE_IMAGE='registry.example/ga-document@sha256:REPLACE_WITH_REGISTRY_DIGEST' \
  GA_SYSTEMD_DIR=/etc/systemd/system \
  GA_POLICY_FILE=/opt/ga/policy/foundation.v1.json \
  GA_ENV_ROOT=/etc/ga \
  GA_DOCUMENT_WORK_ROOT=/var/lib/ga/documents \
  /opt/ga/source/tenant_platform/infra/deploy/document-platform-preflight.sh
```

Do not start services if any check fails. Fix the host or configuration; do not relax image, rootless, cgroup, socket or filesystem checks.

## 5. Start Order And Health

`ga-platform` applies additive migrations at startup. Start Bot Poller first, verify its loopback health, then start Platform and Document Manager:

```bash
sudo systemctl start ga-bot-poller.service
sudo systemctl status --no-pager ga-bot-poller.service
curl --fail --silent http://127.0.0.1:8090/health
sudo systemctl start ga-platform.service
sudo systemctl status --no-pager ga-platform.service
curl --fail --silent http://127.0.0.1:8080/healthz
sudo systemctl start ga-document-manager.service
sudo systemctl status --no-pager ga-document-manager.service
sudo systemctl enable ga-bot-poller.service ga-platform.service ga-document-manager.service
```

`ga-worker-manager.service` is an inert retired placeholder and must remain disabled. `ga-llm-proxy.service` also remains disabled while the shipped Platform unit uses its in-process loopback proxy; if explicitly enabled later, provision `/etc/ga/llm-proxy.env` as `root:ga-llm 0640` first.

## 6. Required Target-Host Smoke

Run the real rootless container smoke as `ga-document`:

```bash
DOCUMENT_UID="$(id -u ga-document)"
sudo -u ga-document -H env \
  XDG_RUNTIME_DIR="/run/user/$DOCUMENT_UID" \
  DOCKER_HOST="unix:///run/user/$DOCUMENT_UID/docker.sock" \
  GA_DOCUMENT_DOCKER_SMOKE=1 \
  GA_DOCUMENT_RUNTIME_BINARY=docker \
  GA_DOCUMENT_SMOKE_IMAGE='registry.example/ga-document@sha256:REPLACE_WITH_REGISTRY_DIGEST' \
  GA_DOCUMENT_SECCOMP_PROFILE=builtin \
  /opt/ga/source/tenant_platform/tests/smoke/document_pool_docker.sh
```

Run the authenticated control-plane smoke locally on the host. Enter tokens without placing them in shell history:

```bash
read -rsp 'Platform admin token: ' PLATFORM_DEV_TOKEN; echo
read -rsp 'Approved tenant token: ' GA_USER_TOKEN; echo
export PLATFORM_DEV_TOKEN GA_USER_TOKEN
python3 /opt/ga/source/tenant_platform/tests/smoke/document_pool_admin.py
GA_DOCUMENT_ADMIN_SMOKE_MUTATE=1 python3 /opt/ga/source/tenant_platform/tests/smoke/document_pool_admin.py
unset PLATFORM_DEV_TOKEN GA_USER_TOKEN
```

In the admin Web UI, verify the document-pool status values match PostgreSQL activity, save one bounded configuration change, and verify SOP connection/candidate/installed views. Submit a WeChat request that generates DOCX, download the artifact, open it, then confirm the job reaches terminal state and its single-use container is destroyed.

Capture evidence without secret values:

```bash
uname -a
stat -fc %T /sys/fs/cgroup
docker info --format '{{json .SecurityOptions}} {{.CgroupVersion}}'
systemctl is-active ga-bot-poller ga-platform ga-document-manager
systemctl show ga-bot-poller -p User -p NoNewPrivileges -p ProtectSystem -p InaccessiblePaths -p ReadWritePaths
systemctl show ga-platform -p User -p NoNewPrivileges -p ProtectSystem -p InaccessiblePaths
systemctl show ga-document-manager -p User -p NoNewPrivileges -p ProtectSystem -p ProtectHome -p InaccessiblePaths -p BindReadOnlyPaths -p ReadWritePaths
journalctl -u ga-bot-poller -u ga-platform -u ga-document-manager --since '30 minutes ago' --priority=warning --no-pager
```

Before accepting the deployment, search captured journals for each known secret using `grep -Fq` and record only pass/fail. Never paste the matching line into evidence.

## 7. Rollback

1. Disable new document work through the admin CAS settings API (`enabled=false`).
2. Wait for active jobs to become terminal, then stop `ga-document-manager`, `ga-platform` and `ga-bot-poller`.
3. Restore the previous reviewed binaries, policy, Worker tree and systemd units.
4. Keep additive database migrations. Do not manually delete document/SOP tables or approved versions.
5. Restore PostgreSQL only for a declared data rollback, after taking a second backup and accounting for tasks/artifacts created since the first backup.
6. Re-run preflight and both smoke scripts before re-enabling the pool.

## Acceptance Record

Record the Git revision, image repository digest, host kernel, runtime version, cgroup version, service unit hashes, preflight output, Docker smoke result, admin smoke result, browser/WeChat artifact result and rollback owner in `.tasks/document-processing-platform/tasks/security-verification/PROGRESS.md`. The task remains incomplete until actual target-host evidence exists.
