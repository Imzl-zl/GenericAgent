# syntax=docker/dockerfile:1.7@sha256:a57df69d0ea827fb7266491f2813635de6f17269be881f696fbfdf2d83dda33e
# llm-proxy — 内部透明 LLM Proxy(方案 §7)
# 仅加入 database 与 runner-control 网络, 不映射宿主机端口。

FROM golang:1.22-bookworm@sha256:3d699e4d15d0f8f13c9195c0632a16702b8cbdece2955af1c23b37ae5d55a253 AS build
WORKDIR /src
COPY tenant_platform/backend-go/go.mod tenant_platform/backend-go/go.sum ./
RUN go mod download
COPY tenant_platform/backend-go/ ./
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags='-s -w -buildid=' -o /out/ga-llm-proxy ./cmd/llm-proxy

FROM alpine:3.19@sha256:6baf43584bcb78f2e5847d1de515f23499913ac9f12bdf834811a3145eb11ca1
# tzdata(2026-08-13): compose 注入 TZ=Asia/Shanghai 需要 zoneinfo,
# 否则 musl 回退 UTC, TZ 静默失效(实测 alpine:3.19 无 zoneinfo)。
RUN apk add --no-cache ca-certificates tzdata && adduser -D -u 10001 -g 10001 llmproxy
COPY --from=build --chown=10001:10001 /out/ga-llm-proxy /usr/local/bin/ga-llm-proxy
USER 10001:10001
ENTRYPOINT ["/usr/local/bin/ga-llm-proxy"]
# 监听地址由 LLM_PROXY_LISTEN 决定(compose 默认 0.0.0.0:8081);
# 不在 CMD 写死 --listen, 否则会覆盖环境变量配置(审查: 配置接线)。
