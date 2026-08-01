# syntax=docker/dockerfile:1.7@sha256:a57df69d0ea827fb7266491f2813635de6f17269be881f696fbfdf2d83dda33e
FROM golang:1.22-bookworm@sha256:3d699e4d15d0f8f13c9195c0632a16702b8cbdece2955af1c23b37ae5d55a253 AS go-build
WORKDIR /src
COPY tenant_platform/backend-go/go.mod tenant_platform/backend-go/go.sum ./
RUN go mod download
COPY tenant_platform/backend-go/ ./
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags='-s -w -buildid=' -o /out/ga-platform ./cmd/platform

FROM python:3.11-slim-bookworm@sha256:b18992999dbe963a45a8a4da40ac2b1975be1a776d939d098c647482bcad5cba
ENV PYTHONUNBUFFERED=1 \
    PYTHONDONTWRITEBYTECODE=1 \
    HOME=/home/ga-platform \
    GA_RUNTIME_DIR=/var/lib/ga/runtime \
    GA_CONFIG_ROOT=/etc/ga/config \
    GA_LEGACY_ROOT=/opt/ga/legacy \
    GA_WORKER_PYTHON=/usr/local/bin/python3 \
    GA_WORKER_SRC=/opt/ga/worker-python/src \
    GA_MIGRATIONS_DIR=/opt/ga/migrations \
    GA_WORKSPACE_TEMP=/var/lib/ga/runtime/workspace-temp

RUN groupadd --system --gid 10003 ga-delivery \
    && groupadd --system --gid 10001 ga-platform \
    && useradd --system --uid 10001 --gid 10001 --groups ga-delivery --create-home --home-dir /home/ga-platform ga-platform

COPY tenant_platform/worker-python/pyproject.toml /tmp/worker/pyproject.toml
COPY tenant_platform/worker-python/src/ /tmp/worker/src/
RUN pip install --no-cache-dir /tmp/worker \
    'beautifulsoup4>=4.12' 'bottle>=0.12' 'simple-websocket-server>=0.4' 'aiohttp>=3.9' \
    && rm -rf /tmp/worker

COPY --from=go-build /out/ga-platform /opt/ga/bin/platform
COPY tenant_platform/worker-python/src/ /opt/ga/worker-python/src/
COPY agentmain.py ga.py llmcore.py agent_loop.py simphtml.py /opt/ga/legacy/
COPY plugins/ /opt/ga/legacy/plugins/
COPY assets/ /opt/ga/legacy/assets/
COPY tenant_platform/contracts/policy/foundation.v1.json /opt/ga/policy/foundation.v1.json
COPY tenant_platform/infra/postgres/migrations/ /opt/ga/migrations/

RUN install -d -o 10001 -g 10001 -m 0750 \
        /etc/ga/config \
        /var/lib/ga/runtime \
    && install -d -o 10001 -g 10003 -m 2770 \
        /var/lib/ga/runtime/session_files \
        /var/lib/ga/bot-media \
    && chmod -R a-w /opt/ga/bin /opt/ga/legacy /opt/ga/worker-python /opt/ga/policy /opt/ga/migrations

USER 10001:10001
WORKDIR /opt/ga
EXPOSE 8080
ENTRYPOINT ["/bin/sh", "-c", "umask 0027; exec /opt/ga/bin/platform \"$@\"", "--"]
CMD ["--policy-file=/opt/ga/policy/foundation.v1.json", "--claim-lease=30s", "--dev-loopback", "--listen=127.0.0.1:8080", "--runtime-root=/var/lib/ga/runtime", "--config-root=/etc/ga/config", "--legacy-root=/opt/ga/legacy", "--worker-python=/usr/local/bin/python3", "--worker-src=/opt/ga/worker-python/src"]
