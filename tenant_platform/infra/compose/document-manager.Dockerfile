# syntax=docker/dockerfile:1.7
FROM golang:1.22-bookworm@sha256:3d699e4d15d0f8f13c9195c0632a16702b8cbdece2955af1c23b37ae5d55a253 AS build
WORKDIR /src
COPY tenant_platform/backend-go/go.mod tenant_platform/backend-go/go.sum ./
RUN go mod download
COPY tenant_platform/backend-go/ ./
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags='-s -w -buildid=' -o /out/document-manager ./cmd/document-manager

FROM docker@sha256:851f91d241214e7c6db86513b270d58776379aacc5eb9c4a87e5b47115e3065c
COPY --from=build /out/document-manager /opt/ga/bin/document-manager
COPY tenant_platform/document-image/Dockerfile /opt/ga/document-context/tenant_platform/document-image/Dockerfile
COPY tenant_platform/backend-go/go.mod tenant_platform/backend-go/go.sum /opt/ga/document-context/tenant_platform/backend-go/
COPY tenant_platform/backend-go/cmd/document-tool/ /opt/ga/document-context/tenant_platform/backend-go/cmd/document-tool/
COPY tenant_platform/infra/postgres/migrations/ /opt/ga/migrations/
COPY tenant_platform/infra/compose/document-manager-entrypoint.sh /usr/local/bin/document-manager-entrypoint
RUN chmod 0555 /opt/ga/bin/document-manager /usr/local/bin/document-manager-entrypoint \
    && install -d -m 0700 /var/lib/ga/documents

ENTRYPOINT ["/usr/local/bin/document-manager-entrypoint"]
