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

FROM docker:27-cli@sha256:851f91d241214e7c6db86513b270d58776379aacc5eb9c4a87e5b47115e3065c
COPY --from=build /out/ga-sandbox-manager /usr/local/bin/ga-sandbox-manager
# 以 root 运行: 需要 chown 工作区目录(固定 Runner UID/GID)与访问 Docker socket。
# 容器仍受 read_only + cap_drop ALL 约束; compose 仅追加 CHOWN/DAC 能力。
# 若存在 memory-template 构建产物则一并拷贝(与 ga-runner 镜像同源模板)。
COPY tenant_platform/infra/compose/memory-template/ /ga/memory-template/
ENTRYPOINT ["/usr/local/bin/ga-sandbox-manager"]
