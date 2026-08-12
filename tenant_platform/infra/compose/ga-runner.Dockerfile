# syntax=docker/dockerfile:1.7@sha256:a57df69d0ea827fb7266491f2813635de6f17269be881f696fbfdf2d83dda33e
# ga-runner — 工作区隔离的 GA Runner(方案 §2-§5)
#
# Build context 必须是仓库根:
#   docker build -f tenant_platform/infra/compose/ga-runner.Dockerfile -t ga-runner .
#
# 先运行 tenant_platform/scripts/build-memory-template.sh 生成 memory-template/。
#
# 镜像层固定 digest;GA 代码、assets、memory-template、worker-python 全部只读。
# 运行时由 Manager 挂载当前工作区四个 subpath(方案 §4/§7):
#   /ga/legacy/memory      <- workspaces/<hash>/memory(读写)
#   /ga/legacy/temp        <- workspaces/<hash>/temp(读写;附件/输出经此互通)
#   /ga/runner-state       <- workspaces/<hash>/state(读写;checkpoint staging/committed)
#   /ga/runner-config      <- workspaces/<hash>/config(只读;mTLS 证书/策略)

FROM python:3.11-slim-bookworm@sha256:b18992999dbe963a45a8a4da40ac2b1975be1a776d939d098c647482bcad5cba

ENV PYTHONUNBUFFERED=1 \
    PYTHONDONTWRITEBYTECODE=1 \
    GA_LEGACY_ROOT=/ga/legacy \
    GA_WORKER_SRC=/ga/worker-python/src \
    GA_MEMORY_TEMPLATE=/ga/memory-template \
    GA_DISABLE_HOST_BROWSER=1 \
    # Round8 审查: inline_eval 在 Worker 进程内执行, 绕过进程组清理, Runner 禁用。
    GA_DISABLE_INLINE_EVAL=1

# 固定非 root Runner UID/GID(部署 profile 与 Manager 预置目录所有权一致)。
RUN groupadd --system --gid 10002 ga-runner \
    && useradd --system --uid 10002 --gid 10002 --no-create-home ga-runner

# worker-python + 文档工具链(DOCX/PDF/XLSX 是镜像能力,不再走文档专用 RPC)。
# 工具链决策(2026-08-08, 按官方能力对比):
#   - pandoc 3.8.3 官方二进制: Debian apt 只有 2.17, 而官方 3.8 起新增
#     xlsx/pptx/asciidoc 输入格式(官方 changelog); 官方 tar.gz 含全部引擎。
#   - LibreOffice headless: 唯一支持 .doc/.xls 老格式的轻量方案, 且
#     docx→pdf 为渲染式高保真转换(格式不乱), 官方 pandoc 的 pdf 输出
#     需要额外 pdf-engine(默认 pdflatex 未装)。
#   - fonts-noto-cjk: 中文渲染必需(weasyprint/LaTeX/LibreOffice 均依赖
#     系统字体, 默认 DejaVu 无 CJK)。
RUN apt-get update \
    && apt-get install -y --no-install-recommends \
        libreoffice-writer libreoffice-calc \
        fonts-noto-cjk \
        ca-certificates curl \
    && rm -rf /var/lib/apt/lists/*
# pandoc 官方二进制(apt 版 2.17 无 xlsx 输入; 3.8+ 官方支持)。
RUN curl -fsSL -o /tmp/pandoc.tar.gz \
        https://github.com/jgm/pandoc/releases/download/3.8.3/pandoc-3.8.3-linux-amd64.tar.gz \
    && tar -xzf /tmp/pandoc.tar.gz -C /usr/local --strip-components=1 \
    && rm /tmp/pandoc.tar.gz
# stdio MCP 运行时(2026-08-12): npx/uvx 型 MCP server 按需拉包需要 node 与
# uv; 预装命令型(如 serena)用 pip 即可。固定版本 + sha256 校验(镜像层固定
# digest 精神)。npx/uvx/pip 缓存目录由 sandbox-manager 运行时注入到共享卷
# (GA_PKG_CACHE_VOLUME, 全租户复用一份)。
# node 20 LTS: 官方二进制(apt bookworm 只有 18.x)。
RUN curl -fsSL -o /tmp/node.tar.xz \
        https://nodejs.org/dist/v20.20.2/node-v20.20.2-linux-x64.tar.xz \
    && echo "df770b2a6f130ed8627c9782c988fda9669fa23898329a61a871e32f965e007d  /tmp/node.tar.xz" | sha256sum -c - \
    && tar -xJf /tmp/node.tar.xz -C /usr/local --strip-components=1 \
    && rm /tmp/node.tar.xz
# uv(uvx): Python 生态的按需包运行器。
RUN curl -fsSL -o /tmp/uv.tar.gz \
        https://github.com/astral-sh/uv/releases/download/0.12.3/uv-x86_64-unknown-linux-gnu.tar.gz \
    && echo "600cf9a742aca00d292673b16b5acffaa7b8c269a364ad0c2e79498dcb1fe101  /tmp/uv.tar.gz" | sha256sum -c - \
    && tar -xzf /tmp/uv.tar.gz -C /tmp \
    && install -m 0755 /tmp/uv-x86_64-unknown-linux-gnu/uv /tmp/uv-x86_64-unknown-linux-gnu/uvx /usr/local/bin/ \
    && rm -rf /tmp/uv.tar.gz /tmp/uv-x86_64-unknown-linux-gnu
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
    # stdio MCP 共享包缓存卷挂载点: 镜像内预建 + chown, 新命名卷首挂时
    # 以镜像目录(含属主)初始化——否则卷根 root:root, runner 用户写不进
    # npx/uv/pip 缓存(缓存目录 env 由 sandbox-manager 注入到此处)。
    && mkdir -p /var/cache/ga-pkg \
    && chown 10002:10002 /var/cache/ga-pkg \
    && chmod -R a-w /ga/legacy /ga/memory-template /ga/worker-python /usr/local/lib/python3.11 \
    && chmod a-w /ga

USER 10002:10002
WORKDIR /ga/legacy/temp

# 容器启动后由 Sandbox Manager 传入 --listen 与 mTLS 身份;默认 unix socket 便于冒烟。
ENTRYPOINT ["python", "-m", "ga_worker.entrypoint"]
CMD ["--listen", "unix:/ga/runner-state/worker.sock", "--grace-seconds", "5"]
