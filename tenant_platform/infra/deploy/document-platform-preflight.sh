#!/usr/bin/env bash
set -euo pipefail

if [[ "${GA_DOCUMENT_DEPLOY_PREFLIGHT:-}" != "1" ]]; then
  echo "document-platform-preflight: skipped (set GA_DOCUMENT_DEPLOY_PREFLIGHT=1 on the target Linux host)" >&2
  exit 0
fi

fail() {
  echo "document-platform-preflight: FAIL: $*" >&2
  exit 1
}

pass() {
  echo "document-platform-preflight: OK: $*"
}

require_command() {
  command -v "$1" >/dev/null 2>&1 || fail "required command not found: $1"
}

require_env_key() {
  local file="$1"
  local key="$2"
  grep -Eq "^${key}=.+$" "$file" || fail "$file must define non-empty $key"
  if grep -Eq "^${key}=(CHANGE_ME|REPLACE_ME|TODO)$" "$file"; then
    fail "$file contains a placeholder value for $key"
  fi
}

env_value() {
  local file="$1"
  local key="$2"
  local line
  line="$(grep -Em1 "^${key}=" "$file")" || fail "$file must define $key"
  printf '%s' "${line#*=}"
}

require_positive_env() {
  local file="$1"
  local key="$2"
  local value
  value="$(env_value "$file" "$key")"
  [[ "$value" =~ ^[1-9][0-9]*$ ]] || fail "$file $key must be a positive integer"
}

require_duration_env() {
  local file="$1"
  local key="$2"
  local value
  value="$(env_value "$file" "$key")"
  [[ "$value" =~ ^[1-9][0-9]*(ms|s|m)$ ]] || fail "$file $key must be a positive ms/s/m duration"
}

validate_env_file() {
  local file="$1"
  local line key
  declare -A seen=()
  while IFS= read -r line || [[ -n "$line" ]]; do
    [[ -z "$line" || "$line" == \#* ]] && continue
    [[ "$line" =~ ^[A-Z][A-Z0-9_]*=[^[:space:]]+$ ]] || fail "$file contains a malformed or whitespace-bearing assignment"
    key="${line%%=*}"
    [[ -z "${seen[$key]:-}" ]] || fail "$file defines $key more than once"
    seen[$key]=1
  done < "$file"
}

require_root_owned_readonly_file() {
  local file="$1"
  [[ -f "$file" && -r "$file" ]] || fail "required file is missing or unreadable: $file"
  local mode owner
  mode="$(stat -c '%a' "$file")"
  owner="$(stat -c '%U' "$file")"
  [[ "$owner" == "root" ]] || fail "$file must be owned by root"
  (( (8#$mode & 022) == 0 )) || fail "$file must not be writable by group or other"
}

require_private_env_file() {
  local file="$1"
  local expected_group="$2"
  [[ -f "$file" && -r "$file" ]] || fail "environment file must exist and be readable: $file"
  local mode owner group
  mode="$(stat -c '%a' "$file")"
  owner="$(stat -c '%U' "$file")"
  group="$(stat -c '%G' "$file")"
  [[ "$mode" == "640" ]] || fail "$file mode must be 640, got $mode"
  [[ "$owner" == "root" && "$group" == "$expected_group" ]] || fail "$file must be owned by root:$expected_group"
}

require_service_directory() {
  local directory="$1"
  local label="$2"
  local expected_owner="$3"
  local expected_group="$4"
  [[ -d "$directory" && ! -L "$directory" ]] || fail "$label must be a real directory: $directory"
  [[ "$(realpath "$directory")" == "$directory" ]] || fail "$label must already be canonical"
  local mode owner group
  mode="$(stat -c '%a' "$directory")"
  owner="$(stat -c '%U' "$directory")"
  group="$(stat -c '%G' "$directory")"
  [[ "$owner" == "$expected_owner" && "$group" == "$expected_group" ]] || fail "$label must be owned by $expected_owner:$expected_group"
  (( (8#$mode & 022) == 0 )) || fail "$label must not be writable by group or other"
  (( (8#$mode & 0300) == 0300 )) || fail "$label owner must have write and search permission"
}

require_trusted_parent_chain() {
  local leaf="$1"
  local parent mode owner next
  parent="$(dirname "$leaf")"
  while [[ "$parent" != "." ]]; do
    if [[ "${GA_DOCUMENT_DEPLOY_PREFLIGHT_TEST:-}" == "1" && "$parent" =~ ^[A-Za-z]:$ ]]; then
      break
    fi
    [[ -d "$parent" && ! -L "$parent" ]] || fail "untrusted parent directory in path: $parent"
    owner="$(stat -c '%U' "$parent")"
    mode="$(stat -c '%a' "$parent")"
    [[ "$owner" == "root" ]] || fail "parent directory must be root-owned: $parent"
    (( (8#$mode & 022) == 0 )) || fail "parent directory must not be writable by group or other: $parent"
    (( (8#$mode & 0111) == 0111 )) || fail "parent directory must be traversable by every service identity: $parent"
    [[ "$parent" == "/" ]] && break
    next="$(dirname "$parent")"
    [[ "$next" != "$parent" ]] || fail "cannot validate parent chain for $leaf"
    parent="$next"
  done
}

require_exact_groups() {
  local user="$1"
  shift
  local actual group allowed expected
  actual="$(id -nG "$user")" || fail "cannot inspect groups for $user"
  for group in $actual; do
    allowed=0
    for expected in "$@"; do
      [[ "$group" == "$expected" ]] && allowed=1
    done
    [[ "$allowed" == "1" ]] || fail "$user has unexpected supplementary group $group"
  done
  for expected in "$@"; do
    [[ " $actual " == *" $expected "* ]] || fail "$user must belong to $expected"
  done
}

require_effective_unit() {
  local unit="$1"
  local expected="$SYSTEMD_DIR/$unit"
  local load_state fragment dropins
  load_state="$(systemctl show "$unit" -p LoadState --value)" || fail "cannot inspect effective unit $unit"
  [[ "$load_state" == "loaded" ]] || fail "$unit is not loaded by systemd"
  fragment="$(systemctl show "$unit" -p FragmentPath --value)" || fail "cannot inspect fragment path for $unit"
  [[ -n "$fragment" && "$(realpath "$fragment")" == "$(realpath "$expected")" ]] || fail "$unit effective fragment is not the reviewed file"
  dropins="$(systemctl show "$unit" -p DropInPaths --value)" || fail "cannot inspect drop-ins for $unit"
  [[ -z "$dropins" ]] || fail "$unit has an unreviewed systemd drop-in"
}

PLATFORM_USER="${GA_PLATFORM_USER:-ga-platform}"
DOCUMENT_USER="${GA_DOCUMENT_USER:-ga-document}"
BOT_USER="${GA_BOT_USER:-ga-bot}"
LLM_USER="${GA_LLM_USER:-ga-llm}"
DELIVERY_GROUP="${GA_DELIVERY_GROUP:-ga-delivery}"
RUNTIME_BINARY="${GA_DOCUMENT_RUNTIME_BINARY:-docker}"
DOCUMENT_IMAGE="${GA_DOCUMENT_SMOKE_IMAGE:-}"
SYSTEMD_DIR="${GA_SYSTEMD_DIR:-/etc/systemd/system}"
POLICY_FILE="${GA_POLICY_FILE:-/opt/ga/policy/foundation.v1.json}"
ENV_ROOT="${GA_ENV_ROOT:-/etc/ga}"
WORK_ROOT="${GA_DOCUMENT_WORK_ROOT:-/var/lib/ga/documents}"
CGROUP_ROOT="/sys/fs/cgroup"
if [[ "${GA_DOCUMENT_DEPLOY_PREFLIGHT_TEST:-}" == "1" ]]; then
  SYSTEMD_DIR="${GA_SYSTEMD_DIR:?GA_SYSTEMD_DIR is required in preflight test mode}"
  POLICY_FILE="${GA_POLICY_FILE:?GA_POLICY_FILE is required in preflight test mode}"
  ENV_ROOT="${GA_ENV_ROOT:?GA_ENV_ROOT is required in preflight test mode}"
  CGROUP_ROOT="${GA_CGROUP_ROOT:?GA_CGROUP_ROOT is required in preflight test mode}"
else
  [[ "$SYSTEMD_DIR" == "/etc/systemd/system" ]] || fail "GA_SYSTEMD_DIR must be /etc/systemd/system"
  [[ "$POLICY_FILE" == "/opt/ga/policy/foundation.v1.json" ]] || fail "GA_POLICY_FILE must be the unit's fixed policy path"
  [[ "$ENV_ROOT" == "/etc/ga" ]] || fail "GA_ENV_ROOT must be /etc/ga"
  [[ "$WORK_ROOT" == "/var/lib/ga/documents" ]] || fail "GA_DOCUMENT_WORK_ROOT must be the unit's fixed document path"
fi

[[ "$(uname -s)" == "Linux" ]] || fail "target must be Linux"
require_command id
require_command stat
require_command grep
require_command dirname
require_command realpath
require_command runuser
require_command systemctl
require_command systemd-analyze
require_command "$RUNTIME_BINARY"

platform_uid="$(id -u "$PLATFORM_USER")" || fail "service user does not exist: $PLATFORM_USER"
document_uid="$(id -u "$DOCUMENT_USER")" || fail "service user does not exist: $DOCUMENT_USER"
bot_uid="$(id -u "$BOT_USER")" || fail "service user does not exist: $BOT_USER"
llm_uid="$(id -u "$LLM_USER")" || fail "service user does not exist: $LLM_USER"
current_uid="$(id -u)"
[[ "$current_uid" == "0" ]] || fail "run preflight as root; runtime probes are dropped to $DOCUMENT_USER"
for uid in "$platform_uid" "$document_uid" "$bot_uid" "$llm_uid"; do
  [[ "$uid" != "0" ]] || fail "service users must not be root"
done
[[ "$platform_uid" != "$document_uid" && "$platform_uid" != "$bot_uid" && "$platform_uid" != "$llm_uid" && "$document_uid" != "$bot_uid" && "$document_uid" != "$llm_uid" && "$bot_uid" != "$llm_uid" ]] || fail "platform, document, bot and LLM services must use distinct UIDs"
require_exact_groups "$PLATFORM_USER" "$PLATFORM_USER" "$DELIVERY_GROUP"
require_exact_groups "$DOCUMENT_USER" "$DOCUMENT_USER"
require_exact_groups "$BOT_USER" "$BOT_USER" "$DELIVERY_GROUP"
require_exact_groups "$LLM_USER" "$LLM_USER"
[[ -r "$CGROUP_ROOT/cgroup.controllers" ]] || fail "cgroup v2 unified hierarchy is required"
runtime_root="/run/user/$document_uid"
if [[ "${GA_DOCUMENT_DEPLOY_PREFLIGHT_TEST:-}" == "1" ]]; then
  runtime_root="${GA_RUNTIME_ROOT:?GA_RUNTIME_ROOT is required in preflight test mode}"
fi
require_service_directory "$runtime_root" "rootless runtime directory" "$DOCUMENT_USER" "$DOCUMENT_USER"
require_trusted_parent_chain "$runtime_root"

platform_env="$ENV_ROOT/platform.env"
manager_env="$ENV_ROOT/document-manager.env"
bot_env="$ENV_ROOT/bot-poller.env"
require_private_env_file "$platform_env" "$PLATFORM_USER"
require_private_env_file "$manager_env" "$DOCUMENT_USER"
require_private_env_file "$bot_env" "$BOT_USER"
validate_env_file "$platform_env"
validate_env_file "$manager_env"
validate_env_file "$bot_env"
require_env_key "$manager_env" DOCUMENT_MANAGER_RUNTIME_BINARY
require_env_key "$manager_env" DOCUMENT_MANAGER_IMAGE
require_env_key "$manager_env" DOCUMENT_MANAGER_WORK_ROOT
require_env_key "$manager_env" XDG_RUNTIME_DIR
grep -Fqx "DOCUMENT_MANAGER_RUNTIME_BINARY=${RUNTIME_BINARY}" "$manager_env" || fail "document-manager.env runtime does not match the verified runtime"
[[ "$DOCUMENT_IMAGE" =~ ^[a-z0-9][a-z0-9._/:-]*@sha256:[a-f0-9]{64}$ ]] || fail "GA_DOCUMENT_SMOKE_IMAGE must be an untagged repository@sha256:<64 lowercase hex>"
image_repository="${DOCUMENT_IMAGE%@sha256:*}"
[[ "${image_repository##*/}" != *:* ]] || fail "GA_DOCUMENT_SMOKE_IMAGE repository must not include a tag"
grep -Fqx "DOCUMENT_MANAGER_IMAGE=${DOCUMENT_IMAGE}" "$manager_env" || fail "document-manager.env image does not match the inspected digest"
grep -Fqx "DOCUMENT_MANAGER_WORK_ROOT=${WORK_ROOT}" "$manager_env" || fail "document-manager.env work root does not match the verified directory"
grep -Fqx "XDG_RUNTIME_DIR=${runtime_root}" "$manager_env" || fail "document-manager.env XDG_RUNTIME_DIR must match the document service user"
runtime_endpoint_name=""
runtime_endpoint_value=""
if [[ "$RUNTIME_BINARY" == "docker" ]]; then
  require_env_key "$manager_env" DOCKER_HOST
  runtime_endpoint_name="DOCKER_HOST"
  runtime_endpoint_value="unix://${runtime_root}/docker.sock"
  grep -Fqx "DOCKER_HOST=${runtime_endpoint_value}" "$manager_env" || fail "rootless Docker requires its document-user socket in document-manager.env"
elif [[ "$RUNTIME_BINARY" == "podman" ]]; then
  require_env_key "$manager_env" CONTAINER_HOST
  runtime_endpoint_name="CONTAINER_HOST"
  runtime_endpoint_value="unix://${runtime_root}/podman/podman.sock"
  grep -Fqx "CONTAINER_HOST=${runtime_endpoint_value}" "$manager_env" || fail "rootless Podman requires its document-user socket in document-manager.env"
fi

runtime_exec() {
  runuser -u "$DOCUMENT_USER" -- env \
    "PATH=$PATH" \
    "XDG_RUNTIME_DIR=$runtime_root" \
    "${runtime_endpoint_name}=${runtime_endpoint_value}" \
    "$RUNTIME_BINARY" "$@"
}

case "$RUNTIME_BINARY" in
  docker)
    security_options="$(runtime_exec info --format '{{json .SecurityOptions}}' 2>/dev/null)" || fail "docker info failed for $DOCUMENT_USER"
    [[ "${security_options,,}" == *rootless* ]] || fail "Docker daemon is not rootless"
    [[ "${security_options,,}" == *seccomp* && "${security_options,,}" != *unconfined* ]] || fail "Docker must report confined seccomp enforcement"
    cgroup_version="$(runtime_exec info --format '{{.CgroupVersion}}' 2>/dev/null)" || fail "Docker cgroup version lookup failed"
    [[ "$cgroup_version" == "2" ]] || fail "Docker must use cgroup v2, got $cgroup_version"
    controllers="$(runtime_exec info --format '{{.MemoryLimit}} {{.CPUCfsPeriod}} {{.CPUCfsQuota}} {{.PidsLimit}}' 2>/dev/null)" || fail "Docker resource-controller lookup failed"
    [[ "$controllers" == "true true true true" ]] || fail "Docker must enforce memory, CPU period/quota and PID limits"
    ;;
  podman)
    rootless="$(runtime_exec info --format '{{.Host.Security.Rootless}}' 2>/dev/null)" || fail "podman info failed for $DOCUMENT_USER"
    [[ "$rootless" == "true" ]] || fail "Podman is not rootless"
    seccomp="$(runtime_exec info --format '{{.Host.Security.SeccompEnabled}}' 2>/dev/null)" || fail "Podman seccomp lookup failed"
    [[ "$seccomp" == "true" ]] || fail "Podman must report seccomp enforcement"
    cgroup_version="$(runtime_exec info --format '{{.Host.CgroupsVersion}}' 2>/dev/null)" || fail "Podman cgroup version lookup failed"
    [[ "$cgroup_version" == "v2" || "$cgroup_version" == "2" ]] || fail "Podman must use cgroup v2, got $cgroup_version"
    controllers="$(runtime_exec info --format '{{json .Host.CgroupControllers}}' 2>/dev/null)" || fail "Podman cgroup-controller lookup failed"
    for controller in cpu memory pids; do
      [[ "$controllers" == *\"$controller\"* ]] || fail "Podman does not delegate the $controller cgroup controller"
    done
    ;;
  *) fail "runtime must be docker or podman, got $RUNTIME_BINARY" ;;
esac

[[ "$DOCUMENT_IMAGE" =~ ^[a-z0-9][a-z0-9._/:-]*@sha256:[a-f0-9]{64}$ ]] || fail "GA_DOCUMENT_SMOKE_IMAGE must be an untagged repository@sha256:<64 lowercase hex>"
runtime_exec image inspect "$DOCUMENT_IMAGE" >/dev/null 2>&1 || fail "digest-pinned document image is not available locally"

require_root_owned_readonly_file "$POLICY_FILE"
expected_work_root="/var/lib/ga/documents"
if [[ "${GA_DOCUMENT_DEPLOY_PREFLIGHT_TEST:-}" == "1" ]]; then
  expected_work_root="${GA_EXPECTED_WORK_ROOT:?GA_EXPECTED_WORK_ROOT is required in preflight test mode}"
fi
[[ "$WORK_ROOT" == "$expected_work_root" ]] || fail "document work root must be the fixed systemd path $expected_work_root"
require_service_directory "$WORK_ROOT" "document work root" "$DOCUMENT_USER" "$DOCUMENT_USER"
require_trusted_parent_chain "$WORK_ROOT"
expected_bot_media_root="/var/lib/ga/bot-media"
if [[ "${GA_DOCUMENT_DEPLOY_PREFLIGHT_TEST:-}" == "1" ]]; then
  expected_bot_media_root="${GA_EXPECTED_BOT_MEDIA_ROOT:?GA_EXPECTED_BOT_MEDIA_ROOT is required in preflight test mode}"
fi
bot_media_root="$(env_value "$bot_env" BOT_POLLER_MEDIA_DIR)"
[[ "$bot_media_root" == "$expected_bot_media_root" ]] || fail "bot poller media root must be the fixed systemd path $expected_bot_media_root"
require_service_directory "$bot_media_root" "bot poller media root" "$BOT_USER" "$DELIVERY_GROUP"
require_trusted_parent_chain "$bot_media_root"
expected_platform_runtime_root="/var/lib/ga/runtime"
expected_config_root="/etc/ga/config"
if [[ "${GA_DOCUMENT_DEPLOY_PREFLIGHT_TEST:-}" == "1" ]]; then
  expected_platform_runtime_root="${GA_EXPECTED_PLATFORM_RUNTIME_ROOT:?GA_EXPECTED_PLATFORM_RUNTIME_ROOT is required in preflight test mode}"
  expected_config_root="${GA_EXPECTED_CONFIG_ROOT:?GA_EXPECTED_CONFIG_ROOT is required in preflight test mode}"
fi
platform_runtime_root="$(env_value "$platform_env" GA_RUNTIME_DIR)"
config_root="$(env_value "$platform_env" GA_CONFIG_ROOT)"
[[ "$platform_runtime_root" == "$expected_platform_runtime_root" ]] || fail "platform runtime root must be the fixed systemd path $expected_platform_runtime_root"
[[ "$config_root" == "$expected_config_root" ]] || fail "platform config root must be the fixed systemd path $expected_config_root"
require_service_directory "$platform_runtime_root" "platform runtime root" "$PLATFORM_USER" "$PLATFORM_USER"
require_trusted_parent_chain "$platform_runtime_root"
require_service_directory "$platform_runtime_root/session_files" "platform session files root" "$PLATFORM_USER" "$DELIVERY_GROUP"
require_service_directory "$config_root" "platform config root" "$PLATFORM_USER" "$PLATFORM_USER"
require_trusted_parent_chain "$config_root"
for unit in ga-bot-poller.service ga-platform.service ga-document-manager.service ga-worker-manager.service ga-llm-proxy.service; do
  require_root_owned_readonly_file "$SYSTEMD_DIR/$unit"
done

grep -Fq "User=${BOT_USER}" "$SYSTEMD_DIR/ga-bot-poller.service" || fail "ga-bot-poller.service user does not match preflight bot user"
grep -Fq "User=${PLATFORM_USER}" "$SYSTEMD_DIR/ga-platform.service" || fail "ga-platform.service user does not match preflight platform user"
grep -Fq "User=${DOCUMENT_USER}" "$SYSTEMD_DIR/ga-document-manager.service" || fail "ga-document-manager.service user does not match preflight document user"
grep -Fq "User=${PLATFORM_USER}" "$SYSTEMD_DIR/ga-worker-manager.service" || fail "ga-worker-manager.service user does not match preflight platform user"
grep -Fq "User=${LLM_USER}" "$SYSTEMD_DIR/ga-llm-proxy.service" || fail "ga-llm-proxy.service user does not match preflight LLM user"
grep -Fq "SupplementaryGroups=${DELIVERY_GROUP}" "$SYSTEMD_DIR/ga-platform.service" || fail "ga-platform.service must use only the delivery sharing group"
grep -Fq "SupplementaryGroups=${DELIVERY_GROUP}" "$SYSTEMD_DIR/ga-bot-poller.service" || fail "ga-bot-poller.service must use only the delivery sharing group"
grep -Fq 'Requires=ga-bot-poller.service' "$SYSTEMD_DIR/ga-platform.service" || fail "ga-platform.service must require the authenticated Bot Poller"
grep -Fq -- '--policy-file=/opt/ga/policy/foundation.v1.json' "$SYSTEMD_DIR/ga-platform.service" || fail "ga-platform.service must use the shipped foundation.v1.json policy"
grep -Fq 'InaccessiblePaths=-/run/docker.sock -/var/run/docker.sock -/run/podman/podman.sock -/var/run/podman/podman.sock -/run/user /etc/ga/platform.env /etc/ga/document-manager.env /etc/ga/bot-poller.env /var/lib/ga/documents' "$SYSTEMD_DIR/ga-platform.service" || fail "ga-platform.service must hide runtime sockets, its loaded environment, document work root and peer configuration"
grep -Fq 'ReadWritePaths=/var/lib/ga/runtime /etc/ga/config' "$SYSTEMD_DIR/ga-platform.service" || fail "ga-platform.service writable paths must exclude the document work root"
grep -Fq 'UnsetEnvironment=DOCKER_HOST CONTAINER_HOST' "$SYSTEMD_DIR/ga-platform.service" || fail "ga-platform.service must clear runtime endpoint variables"
grep -Fq 'ProtectHome=true' "$SYSTEMD_DIR/ga-document-manager.service" || fail "document manager must hide home and the general per-user runtime tree"
grep -Fq 'BindReadOnlyPaths=-/run/user/%U/docker.sock -/run/user/%U/podman/podman.sock' "$SYSTEMD_DIR/ga-document-manager.service" || fail "document manager must expose only exact rootless runtime sockets"
grep -Fq 'ReadWritePaths=/var/lib/ga/documents' "$SYSTEMD_DIR/ga-document-manager.service" || fail "document manager must expose only the document work root as writable"
grep -Fq 'InaccessiblePaths=-/run/docker.sock -/var/run/docker.sock -/run/podman/podman.sock -/var/run/podman/podman.sock /var/lib/ga/runtime /etc/ga/config /etc/ga/document-manager.env /etc/ga/platform.env /etc/ga/bot-poller.env /opt/ga/legacy /opt/ga/worker-python' "$SYSTEMD_DIR/ga-document-manager.service" || fail "document manager must hide rootful runtime sockets, its loaded environment, control-plane secrets and Worker data"
grep -Fq 'EnvironmentFile=-/etc/ga/bot-poller.env' "$SYSTEMD_DIR/ga-bot-poller.service" || fail "bot poller must use its dedicated private environment"
grep -Fq 'InaccessiblePaths=-/run/docker.sock -/var/run/docker.sock -/run/podman/podman.sock -/var/run/podman/podman.sock -/run/user /etc/ga/bot-poller.env /etc/ga/platform.env /etc/ga/document-manager.env /var/lib/ga/documents' "$SYSTEMD_DIR/ga-bot-poller.service" || fail "bot poller must hide runtime sockets, its loaded environment, peer configuration and document data"
grep -Fq 'BindReadOnlyPaths=-/var/lib/ga/runtime/session_files' "$SYSTEMD_DIR/ga-bot-poller.service" || fail "bot poller may read only committed session files for outbound delivery"
grep -Fq '/var/lib/ga/documents' "$SYSTEMD_DIR/ga-llm-proxy.service" || fail "LLM Proxy must hide the document work root"
if grep -Eq '^ReadWritePaths=.*(/var/lib/ga|/var/lib/ga/documents)' "$SYSTEMD_DIR/ga-llm-proxy.service"; then
  fail "LLM Proxy must not have write access to GA data roots"
fi
for unit in ga-bot-poller.service ga-platform.service ga-document-manager.service ga-worker-manager.service ga-llm-proxy.service; do
  grep -Fq 'ProtectProc=invisible' "$SYSTEMD_DIR/$unit" || fail "$unit must hide peer service processes"
  grep -Fq 'ProcSubset=pid' "$SYSTEMD_DIR/$unit" || fail "$unit must expose only process-related procfs entries"
done
grep -Fq 'ExecStart=/usr/bin/false' "$SYSTEMD_DIR/ga-worker-manager.service" || fail "legacy worker manager must remain inert"
systemd-analyze verify \
  "$SYSTEMD_DIR/ga-bot-poller.service" \
  "$SYSTEMD_DIR/ga-platform.service" \
  "$SYSTEMD_DIR/ga-document-manager.service" \
  "$SYSTEMD_DIR/ga-worker-manager.service" \
  "$SYSTEMD_DIR/ga-llm-proxy.service" >/dev/null || fail "systemd unit verification failed"
for unit in ga-bot-poller.service ga-platform.service ga-document-manager.service ga-worker-manager.service ga-llm-proxy.service; do
  require_effective_unit "$unit"
done

for key in DATABASE_URL BOT_TOKEN_KEY BOT_POLLER_URL BOT_POLLER_API_SECRET PLATFORM_WEBHOOK_URL PLATFORM_WEBHOOK_SECRET PLATFORM_DEV_TOKEN PLATFORM_DEV_USER_ID GA_RUNTIME_DIR GA_CONFIG_ROOT GA_LEGACY_ROOT LLM_PROXY_CAPABILITY_SIGNING_KEY DOCUMENT_POOL_MAX_ACTIVE_HARD; do
  require_env_key "$platform_env" "$key"
done
for key in DATABASE_URL DOCUMENT_MANAGER_OWNER DOCUMENT_MANAGER_WORK_ROOT DOCUMENT_MANAGER_RUNTIME_BINARY DOCUMENT_MANAGER_IMAGE DOCUMENT_MANAGER_SECCOMP_PROFILE DOCUMENT_MANAGER_UID DOCUMENT_MANAGER_GID DOCUMENT_MANAGER_MEMORY_BYTES DOCUMENT_MANAGER_CPU_PERIOD DOCUMENT_MANAGER_CPU_QUOTA DOCUMENT_MANAGER_PIDS_LIMIT DOCUMENT_MANAGER_TMPFS_BYTES DOCUMENT_POOL_MAX_ACTIVE_HARD DOCUMENT_MANAGER_CLAIM_LEASE DOCUMENT_MANAGER_HEARTBEAT_INTERVAL DOCUMENT_MANAGER_POLL_INTERVAL DOCUMENT_MANAGER_COMMAND_POLL_INTERVAL DOCUMENT_MANAGER_SHUTDOWN_TIMEOUT XDG_RUNTIME_DIR; do
  require_env_key "$manager_env" "$key"
done

for key in BOT_POLLER_LISTEN BOT_POLLER_MEDIA_DIR BOT_POLLER_API_SECRET PLATFORM_WEBHOOK_SECRET; do
  require_env_key "$bot_env" "$key"
done

bot_key="$(env_value "$platform_env" BOT_TOKEN_KEY)"
[[ "$bot_key" =~ ^[a-fA-F0-9]{64}$ ]] || fail "platform.env BOT_TOKEN_KEY must be a 32-byte hex key"
signing_key="$(env_value "$platform_env" LLM_PROXY_CAPABILITY_SIGNING_KEY)"
[[ ${#signing_key} -ge 32 ]] || fail "platform.env LLM_PROXY_CAPABILITY_SIGNING_KEY must be at least 32 characters"
platform_dev_token="$(env_value "$platform_env" PLATFORM_DEV_TOKEN)"
[[ ${#platform_dev_token} -ge 32 ]] || fail "platform.env PLATFORM_DEV_TOKEN must be at least 32 characters"
platform_poller_secret="$(env_value "$platform_env" BOT_POLLER_API_SECRET)"
platform_webhook_secret="$(env_value "$platform_env" PLATFORM_WEBHOOK_SECRET)"
bot_poller_secret="$(env_value "$bot_env" BOT_POLLER_API_SECRET)"
bot_webhook_secret="$(env_value "$bot_env" PLATFORM_WEBHOOK_SECRET)"
[[ ${#platform_poller_secret} -ge 32 && "$platform_poller_secret" == "$bot_poller_secret" ]] || fail "Bot Poller API secret must match and be at least 32 characters"
[[ ${#platform_webhook_secret} -ge 32 && "$platform_webhook_secret" == "$bot_webhook_secret" ]] || fail "platform webhook secret must match and be at least 32 characters"
[[ "$(env_value "$platform_env" BOT_POLLER_URL)" == "http://127.0.0.1:8090" ]] || fail "platform Bot Poller URL must be fixed loopback"
[[ "$(env_value "$platform_env" PLATFORM_WEBHOOK_URL)" == "http://127.0.0.1:8080/v1/im/webhook" ]] || fail "platform webhook URL must be fixed loopback"
[[ "$(env_value "$bot_env" BOT_POLLER_LISTEN)" == "127.0.0.1:8090" ]] || fail "Bot Poller must bind fixed loopback"
require_positive_env "$platform_env" PLATFORM_DEV_USER_ID
require_positive_env "$platform_env" DOCUMENT_POOL_MAX_ACTIVE_HARD
for key in DOCUMENT_MANAGER_UID DOCUMENT_MANAGER_GID DOCUMENT_MANAGER_MEMORY_BYTES DOCUMENT_MANAGER_CPU_PERIOD DOCUMENT_MANAGER_CPU_QUOTA DOCUMENT_MANAGER_PIDS_LIMIT DOCUMENT_MANAGER_TMPFS_BYTES DOCUMENT_POOL_MAX_ACTIVE_HARD; do
  require_positive_env "$manager_env" "$key"
done
[[ "$(env_value "$manager_env" DOCUMENT_MANAGER_UID)" == "1000" && "$(env_value "$manager_env" DOCUMENT_MANAGER_GID)" == "1000" ]] || fail "document container UID:GID must remain 1000:1000"
for key in DOCUMENT_MANAGER_CLAIM_LEASE DOCUMENT_MANAGER_HEARTBEAT_INTERVAL DOCUMENT_MANAGER_POLL_INTERVAL DOCUMENT_MANAGER_COMMAND_POLL_INTERVAL DOCUMENT_MANAGER_SHUTDOWN_TIMEOUT; do
  require_duration_env "$manager_env" "$key"
done
seccomp_profile="$(env_value "$manager_env" DOCUMENT_MANAGER_SECCOMP_PROFILE)"
if [[ "$seccomp_profile" != "builtin" ]]; then
  [[ "$seccomp_profile" == /* && -f "$seccomp_profile" && ! -L "$seccomp_profile" ]] || fail "custom seccomp profile must be an absolute regular non-symlink file"
  [[ "$(realpath "$seccomp_profile")" == "$seccomp_profile" ]] || fail "custom seccomp profile path must already be canonical"
  require_root_owned_readonly_file "$seccomp_profile"
fi

grep -Eq '^DOCUMENT_MANAGER_IMAGE=[a-z0-9][a-z0-9._/:-]*@sha256:[a-f0-9]{64}$' "$manager_env" || fail "document-manager.env image must be digest-pinned"
grep -Eq "^DOCUMENT_MANAGER_RUNTIME_BINARY=${RUNTIME_BINARY}$" "$manager_env" || fail "document-manager.env runtime does not match the verified runtime"

pass "Linux service UIDs are distinct and non-root"
pass "$RUNTIME_BINARY is rootless and uses cgroup v2"
pass "document image is digest-pinned and locally inspectable"
pass "systemd isolation units and private environment files are valid"
pass "target host is ready for migration, service start and real smoke"
