# syntax=docker/dockerfile:1.7@sha256:a57df69d0ea827fb7266491f2813635de6f17269be881f696fbfdf2d83dda33e
# ga-runner — 工作区隔离的 GA Runner(方案 §2-§5)
#
# Build context 必须是仓库根:
#   docker build -f tenant_platform/infra/compose/ga-runner.Dockerfile -t ga-runner .
#
# 先运行 tenant_platform/scripts/build-memory-template.sh 生成 memory-template/。
#
# 镜像层固定 digest;GA 代码、assets、memory-template、worker-python 全部只读。
# 运行时由 Manager 挂载当前工作区五个 subpath(方案 §4/§7):
#   /ga/legacy/memory      <- workspaces/<hash>/memory(读写)
#   /ga/legacy/temp        <- workspaces/<hash>/temp(读写;附件/输出经此互通)
#   /ga/runner-state       <- workspaces/<hash>/state(读写;checkpoint staging/committed)
#   /ga/runner-config      <- workspaces/<hash>/config(只读;mTLS 证书/策略)
#   /ga/runner-attachments <- workspaces/<hash>/attachments(只读;输入附件)

FROM python:3.11-slim-bookworm@sha256:b18992999dbe963a45a8a4da40ac2b1975be1a776d939d098c647482bcad5cba

ENV PYTHONUNBUFFERED=1 \
    PYTHONDONTWRITEBYTECODE=1 \
    GA_LEGACY_ROOT=/ga/legacy \
    GA_WORKER_SRC=/ga/worker-python/src \
    GA_MEMORY_TEMPLATE=/ga/memory-template \
    GA_DISABLE_HOST_BROWSER=1

# 固定非 root Runner UID/GID(部署 profile 与 Manager 预置目录所有权一致)。
RUN groupadd --system --gid 10002 ga-runner \
    && useradd --system --uid 10002 --gid 10002 --no-create-home ga-runner

# worker-python + 文档工具链(DOCX/PDF/XLSX 是镜像能力,不再走文档专用 RPC)。
COPY tenant_platform/worker-python/pyproject.toml /tmp/worker/pyproject.toml
COPY tenant_platform/worker-python/src/ /tmp/worker/src/
RUN pip install --no-cache-dir /tmp/worker \
        'python-docx>=1.1' 'openpyxl>=3.1' 'pypdf>=4.0' \
    && rm -rf /tmp/worker

# GA 原生代码与 assets(只读层)。
COPY agentmain.py ga.py llmcore.py agent_loop.py simphtml.py /ga/legacy/
COPY plugins/ /ga/legacy/plugins/
COPY assets/ /ga/legacy/assets/

# 上游固定 commit 的已跟踪 memory 基线(只读模板;首次工作区初始化来源)。
COPY tenant_platform/infra/compose/memory-template/ /ga/memory-template/

# Worker adapter 源码。
COPY tenant_platform/worker-python/src/ /ga/worker-python/src/

# 工具策略(Worker 校验 policy digest 用)。
COPY tenant_platform/contracts/policy/foundation.v1.json /ga/policy/foundation.v1.json

# 挂载点:memory/temp 由 Manager 以工作区 subpath 挂载;runner-state 保存运行态。
RUN mkdir -p /ga/legacy/memory /ga/legacy/temp /ga/runner-state \
    && chown -R 10002:10002 /ga/legacy/memory /ga/legacy/temp /ga/runner-state \
    && chmod -R a-w /ga/legacy /ga/memory-template /ga/worker-python /usr/local/lib/python3.11 \
    && chmod a-w /ga

USER 10002:10002
WORKDIR /ga/legacy/temp

# 容器启动后由 Sandbox Manager 传入 --listen 与 mTLS 身份;默认 unix socket 便于冒烟。
ENTRYPOINT ["python", "-m", "ga_worker.entrypoint"]
CMD ["--listen", "unix:/ga/runner-state/worker.sock", "--grace-seconds", "5"]
