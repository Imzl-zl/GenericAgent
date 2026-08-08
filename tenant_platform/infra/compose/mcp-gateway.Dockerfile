# mcp-gateway: stdio/HTTP 统一 transport 网关(MCP_GATEWAY_DESIGN.zh-CN.md)。
# 设计要点:
#   - 只读 mcp_servers(白名单), 不持凭据;
#   - 不接入任何 egress 网络 —— stdio 子进程继承无网, uv/npm 依赖构建期预装;
#   - 子进程工作目录在 tmpfs 内(每 server 一子目录, 不落盘);
#   - 工具集 = 镜像能力: 新增工具 = 在 /opt/mcp-tools/ 预装并重建镜像。
#   - uv tool 直接装到 /opt/mcp-tools/(wrapper 指向 tool venv), 运行时
#     USER 65534 无需写 uv cache —— 子进程离线运行。
FROM golang:1.22-bookworm AS build
WORKDIR /src
COPY tenant_platform/backend-go/ ./
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/mcp-gateway ./cmd/mcp-gateway

FROM python:3.12-slim-bookworm
ENV UV_TOOL_DIR=/opt/uv-tools \
    UV_CACHE_DIR=/opt/uv-cache \
    UV_TOOL_BIN_DIR=/opt/mcp-tools
RUN apt-get update && apt-get install -y --no-install-recommends pandoc ca-certificates \
    && rm -rf /var/lib/apt/lists/* \
    && python3 -m pip install --no-cache-dir uv \
    && mkdir -p /opt/mcp-tools /opt/uv-tools /tmp/mcp-gateway

# 预装 mcp-pandoc(构建期拉包进镜像, 子进程运行期无网)。
RUN uv tool install mcp-pandoc \
    && ls -la /opt/mcp-tools/

COPY --from=build /out/mcp-gateway /usr/local/bin/mcp-gateway

USER 65534
ENTRYPOINT ["/usr/local/bin/mcp-gateway"]
