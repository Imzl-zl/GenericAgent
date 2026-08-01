# syntax=docker/dockerfile:1.7@sha256:a57df69d0ea827fb7266491f2813635de6f17269be881f696fbfdf2d83dda33e
# sandbox-manager — 唯一持有 Docker socket 的组件(方案 §7)
# 负责按固定 profile 创建/检查/销毁用户 Runner; 不进行业务调度。
# 需要 docker CLI 二进制来执行 docker create/inspect/rm。

FROM golang:1.22-bookworm@sha256:3d699e4d15d0f8f13c9195c0632a16702b8cbdece2955af1c23b37ae5d55a253 AS build
WORKDIR /src
COPY tenant_platform/backend-go/go.mod tenant_platform/backend-go/go.sum ./
RUN go mod download
COPY tenant_platform/backend-go/ ./
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags='-s -w -buildid=' -o /out/ga-sandbox-manager ./cmd/sandbox-manager

FROM docker:27-cli@sha256:29016a9dbbd937a172a1c0a016df8c4586f6f1b5e63eb26d3c94a281ffe50f12
COPY --from=build --chown=10001:10001 /out/ga-sandbox-manager /usr/local/bin/ga-sandbox-manager
USER 10001:10001
ENTRYPOINT ["/usr/local/bin/ga-sandbox-manager"]
