# syntax=docker/dockerfile:1.7@sha256:a57df69d0ea827fb7266491f2813635de6f17269be881f696fbfdf2d83dda33e
FROM golang:1.22-alpine@sha256:1699c10032ca2582ec89a24a1312d986a3f094aed3d5c1147b19880afe40e052 AS build
WORKDIR /src
COPY tenant_platform/backend-go/go.mod tenant_platform/backend-go/go.sum ./
RUN go mod download
COPY tenant_platform/backend-go/cmd/document-tool ./cmd/document-tool
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags='-s -w -buildid=' -o /out/ga-document-tool ./cmd/document-tool

FROM scratch
COPY --from=build --chown=1000:1000 /out/ga-document-tool /usr/local/bin/ga-document-tool
USER 1000:1000
WORKDIR /workspace
CMD ["/usr/local/bin/ga-document-tool", "idle"]
