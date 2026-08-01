#!/bin/sh
set -eu

: "${DOCUMENT_MANAGER_IMAGE:?DOCUMENT_MANAGER_IMAGE is required}"

if [ "${DOCUMENT_MANAGER_ALLOW_ROOTFUL_RUNTIME:-false}" != "true" ]; then
    echo "document-manager: rootful Docker must be explicitly enabled for the Compose profile" >&2
    exit 1
fi
if [ "${DOCUMENT_MANAGER_ALLOW_MUTABLE_IMAGE:-false}" != "true" ]; then
    echo "document-manager: local document image builds must be explicitly enabled for the Compose profile" >&2
    exit 1
fi

if ! docker image inspect "$DOCUMENT_MANAGER_IMAGE" >/dev/null 2>&1; then
    docker build --pull \
        --tag "$DOCUMENT_MANAGER_IMAGE" \
        --file /opt/ga/document-context/tenant_platform/document-image/Dockerfile \
        /opt/ga/document-context
fi

exec /opt/ga/bin/document-manager "$@"
