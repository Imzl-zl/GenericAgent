#!/usr/bin/env bash
set -euo pipefail

if [[ "${GA_DOCUMENT_RUNTIME_PREFLIGHT:-}" != "1" ]]; then
  echo "document-runtime-preflight: skipped (set GA_DOCUMENT_RUNTIME_PREFLIGHT=1 on the Linux host)" >&2
  exit 0
fi

fail() {
  echo "document-runtime-preflight: FAIL: $*" >&2
  exit 1
}

pass() {
  echo "document-runtime-preflight: OK: $*"
}

env_value() {
  local key="$1"
  local line
  line="$(grep -Em1 "^${key}=" "$MANAGER_ENV")" || fail "$MANAGER_ENV must define $key"
  printf '%s' "${line#*=}"
}

require_command() {
  command -v "$1" >/dev/null 2>&1 || fail "required command not found: $1"
}

[[ "$(uname -s)" == "Linux" ]] || fail "target must be Linux"
[[ "$(id -u)" == "0" ]] || fail "run as root"
for command in id stat grep realpath runuser systemctl systemd-analyze docker cmp; do
  require_command "$command"
done

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
DOCUMENT_USER="${GA_DOCUMENT_USER:-ga-document}"
MANAGER_ENV="${GA_DOCUMENT_MANAGER_ENV:-/etc/ga/document-manager.env}"
STACK_MODE="${GA_DOCUMENT_STACK_MODE:-systemd}"
case "$STACK_MODE" in
  systemd)
    MANAGER_UNIT="/etc/systemd/system/ga-document-manager.service"
    EXPECTED_UNIT="$SCRIPT_DIR/ga-document-manager.service"
    ;;
  1panel)
    require_command curl
    MANAGER_UNIT="/etc/systemd/system/ga-document-manager-1panel.service"
    EXPECTED_UNIT="$SCRIPT_DIR/ga-document-manager-1panel.service"
    PLATFORM_HEALTH_URL="${GA_1PANEL_PLATFORM_HEALTH_URL:-http://127.0.0.1:8088/healthz}"
    ;;
  *)
    fail "unsupported GA_DOCUMENT_STACK_MODE: $STACK_MODE"
    ;;
esac
WORK_ROOT="/var/lib/ga/documents"
MIGRATIONS_ROOT="/opt/ga/migrations"
MANAGER_BINARY="/opt/ga/bin/document-manager"

[[ -r /sys/fs/cgroup/cgroup.controllers ]] || fail "cgroup v2 unified hierarchy is required"
document_uid="$(id -u "$DOCUMENT_USER")" || fail "missing service user $DOCUMENT_USER"
[[ "$document_uid" != "0" ]] || fail "$DOCUMENT_USER must not be root"
actual_groups="$(id -nG "$DOCUMENT_USER")"
[[ "$actual_groups" == "$DOCUMENT_USER" ]] || fail "$DOCUMENT_USER groups must be exactly '$DOCUMENT_USER', got '$actual_groups'"

[[ -f "$MANAGER_ENV" && ! -L "$MANAGER_ENV" ]] || fail "missing manager environment file"
[[ "$(stat -c '%a' "$MANAGER_ENV")" == "640" ]] || fail "$MANAGER_ENV must have mode 640"
[[ "$(stat -c '%U:%G' "$MANAGER_ENV")" == "root:${DOCUMENT_USER}" ]] || fail "$MANAGER_ENV must be root:${DOCUMENT_USER}"
if grep -Eq 'CHANGE_ME|REPLACE_ME|TODO' "$MANAGER_ENV"; then
  fail "$MANAGER_ENV contains placeholder values"
fi

[[ "$(env_value DOCUMENT_MANAGER_RUNTIME_BINARY)" == "docker" ]] || fail "this profile requires rootless Docker"
[[ "$(env_value DOCUMENT_MANAGER_WORK_ROOT)" == "$WORK_ROOT" ]] || fail "unexpected document work root"
[[ "$(env_value GA_MIGRATIONS_DIR)" == "$MIGRATIONS_ROOT" ]] || fail "unexpected migration directory"
runtime_root="/run/user/$document_uid"
[[ "$(env_value XDG_RUNTIME_DIR)" == "$runtime_root" ]] || fail "XDG_RUNTIME_DIR must be $runtime_root"
[[ "$(env_value DOCKER_HOST)" == "unix://$runtime_root/docker.sock" ]] || fail "DOCKER_HOST must select the $DOCUMENT_USER rootless socket"
image="$(env_value DOCUMENT_MANAGER_IMAGE)"
[[ "$image" =~ ^[a-z0-9][a-z0-9._/:-]*@sha256:[a-f0-9]{64}$ ]] || fail "DOCUMENT_MANAGER_IMAGE must be repository@sha256 pinned"

[[ -S "$runtime_root/docker.sock" ]] || fail "rootless Docker socket is missing"
[[ "$(stat -c '%U' "$runtime_root")" == "$DOCUMENT_USER" ]] || fail "$runtime_root must be owned by $DOCUMENT_USER"
[[ -d "$WORK_ROOT" && ! -L "$WORK_ROOT" ]] || fail "missing document work root"
[[ "$(stat -c '%U:%G' "$WORK_ROOT")" == "${DOCUMENT_USER}:${DOCUMENT_USER}" ]] || fail "$WORK_ROOT owner mismatch"
(( (8#$(stat -c '%a' "$WORK_ROOT") & 022) == 0 )) || fail "$WORK_ROOT must not be group/other writable"
[[ -d "$MIGRATIONS_ROOT" && "$(stat -c '%U:%G' "$MIGRATIONS_ROOT")" == "root:root" ]] || fail "$MIGRATIONS_ROOT must be root:root"
(( (8#$(stat -c '%a' "$MIGRATIONS_ROOT") & 022) == 0 )) || fail "$MIGRATIONS_ROOT must not be group/other writable"
[[ -x "$MANAGER_BINARY" && "$(stat -c '%U:%G' "$MANAGER_BINARY")" == "root:root" ]] || fail "manager binary must be executable and root-owned"

runtime_env=("XDG_RUNTIME_DIR=$runtime_root" "DOCKER_HOST=unix://$runtime_root/docker.sock")
security_options="$(runuser -u "$DOCUMENT_USER" -- env "${runtime_env[@]}" docker info --format '{{json .SecurityOptions}}')" || fail "rootless docker info failed"
[[ "${security_options,,}" == *rootless* ]] || fail "Docker daemon is not rootless"
[[ "${security_options,,}" == *seccomp* && "${security_options,,}" != *unconfined* ]] || fail "Docker seccomp is not confined"
cgroup_version="$(runuser -u "$DOCUMENT_USER" -- env "${runtime_env[@]}" docker info --format '{{.CgroupVersion}}')" || fail "Docker cgroup lookup failed"
[[ "$cgroup_version" == "2" ]] || fail "rootless Docker must use cgroup v2"
runuser -u "$DOCUMENT_USER" -- env "${runtime_env[@]}" docker image inspect "$image" >/dev/null || fail "pinned document image is not available to rootless Docker"

[[ -f "$MANAGER_UNIT" && -f "$EXPECTED_UNIT" ]] || fail "manager unit is missing"
cmp -s "$EXPECTED_UNIT" "$MANAGER_UNIT" || fail "installed manager unit differs from reviewed Compose profile unit"
if [[ "$STACK_MODE" == "systemd" ]]; then
  systemd-analyze verify "$MANAGER_UNIT" /etc/systemd/system/genericagent-compose.service >/dev/null || fail "systemd unit verification failed"
  [[ "$(systemctl show ga-document-manager.service -p FragmentPath --value)" == "$MANAGER_UNIT" ]] || fail "unexpected manager unit fragment"
  [[ -z "$(systemctl show ga-document-manager.service -p DropInPaths --value)" ]] || fail "manager unit has unreviewed drop-ins"
  systemctl is-active --quiet genericagent-compose.service || fail "Compose application stack is not active"
else
  systemd-analyze verify "$MANAGER_UNIT" >/dev/null || fail "systemd unit verification failed"
  [[ "$(systemctl show ga-document-manager-1panel.service -p FragmentPath --value)" == "$MANAGER_UNIT" ]] || fail "unexpected manager unit fragment"
  [[ -z "$(systemctl show ga-document-manager-1panel.service -p DropInPaths --value)" ]] || fail "manager unit has unreviewed drop-ins"
  curl --fail --silent --show-error "$PLATFORM_HEALTH_URL" >/dev/null || fail "1Panel Platform is not healthy: $PLATFORM_HEALTH_URL"
fi

pass "dedicated UID/groups, private config, rootless Docker, cgroup v2, seccomp, pinned image, paths and effective units"
