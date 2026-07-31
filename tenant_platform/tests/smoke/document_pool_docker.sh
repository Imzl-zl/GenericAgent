#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
BACKEND="$ROOT/tenant_platform/backend-go"

if [[ "${GA_DOCUMENT_DOCKER_SMOKE:-}" != "1" ]]; then
  echo "document-pool-docker-smoke: skipped (set GA_DOCUMENT_DOCKER_SMOKE=1 on a rootless Docker/Podman cgroup-v2 host)" >&2
  exit 0
fi

: "${GA_DOCUMENT_RUNTIME_BINARY:=docker}"
: "${GA_DOCUMENT_SMOKE_IMAGE:?set GA_DOCUMENT_SMOKE_IMAGE to the built document image repository@sha256 digest}"
: "${GA_DOCUMENT_SECCOMP_PROFILE:=builtin}"

cd "$BACKEND"
GA_DOCUMENT_DOCKER_SMOKE=1 \
GA_DOCUMENT_RUNTIME_BINARY="$GA_DOCUMENT_RUNTIME_BINARY" \
GA_DOCUMENT_SMOKE_IMAGE="$GA_DOCUMENT_SMOKE_IMAGE" \
GA_DOCUMENT_SECCOMP_PROFILE="$GA_DOCUMENT_SECCOMP_PROFILE" \
go test -count=1 -timeout 60s ./internal/infrastructure/document -run '^TestDockerCLISmoke$'
