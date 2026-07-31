#!/usr/bin/env bash
set -euo pipefail

if [[ "${GA_COMPOSE_DEPLOY_PREFLIGHT:-}" != "1" ]]; then
  echo "compose-preflight: skipped (set GA_COMPOSE_DEPLOY_PREFLIGHT=1 on the Linux host)" >&2
  exit 0
fi

fail() {
  echo "compose-preflight: FAIL: $*" >&2
  exit 1
}

pass() {
  echo "compose-preflight: OK: $*"
}

require_command() {
  command -v "$1" >/dev/null 2>&1 || fail "required command not found: $1"
}

require_private_file() {
  local file="$1"
  [[ -f "$file" && ! -L "$file" ]] || fail "missing regular file: $file"
  local mode owner
  mode="$(stat -c '%a' "$file")"
  owner="$(stat -c '%U:%G' "$file")"
  [[ "$mode" == "600" ]] || fail "$file must have mode 600, got $mode"
  [[ "$owner" == "root:root" ]] || fail "$file must be root:root, got $owner"
  require_trusted_parent_chain "$file"
}

require_trusted_parent_chain() {
  local current mode owner
  current="$(dirname -- "$(realpath -e -- "$1")")"
  while true; do
    mode="$(stat -c '%a' "$current")"
    owner="$(stat -c '%U:%G' "$current")"
    [[ "$owner" == "root:root" ]] || fail "$current must be root:root, got $owner"
    (( (8#$mode & 022) == 0 )) || fail "$current must not be group/other writable, got mode $mode"
    [[ "$current" == "/" ]] && break
    current="$(dirname -- "$current")"
  done
}

require_trusted_file() {
  local file="$1"
  [[ -f "$file" && ! -L "$file" ]] || fail "missing trusted file: $file"
  [[ "$(stat -c '%U:%G' "$file")" == "root:root" ]] || fail "$file must be root:root"
  (( (8#$(stat -c '%a' "$file") & 022) == 0 )) || fail "$file must not be group/other writable"
  require_trusted_parent_chain "$file"
}

env_value() {
  local file="$1"
  local key="$2"
  local line
  line="$(grep -Em1 "^${key}=" "$file")" || fail "$file must define $key"
  printf '%s' "${line#*=}"
}

[[ "$(uname -s)" == "Linux" ]] || fail "target must be Linux"
[[ "$(id -u)" == "0" ]] || fail "run as root"
require_command docker
require_command python3
require_command grep
require_command stat
require_command realpath
docker compose version >/dev/null 2>&1 || fail "Docker Compose v2 is required"

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
COMPOSE_FILE="$SCRIPT_DIR/compose.yaml"
PROJECT_ENV="$SCRIPT_DIR/.env"
PLATFORM_ENV="$SCRIPT_DIR/secrets/platform.env"
BOT_ENV="$SCRIPT_DIR/secrets/bot-poller.env"
POSTGRES_ENV="$SCRIPT_DIR/secrets/postgres.env"

require_trusted_file "$COMPOSE_FILE"
for file in "$PROJECT_ENV" "$PLATFORM_ENV" "$BOT_ENV" "$POSTGRES_ENV"; do
  require_private_file "$file"
done
if grep -R -E 'CHANGE_ME|REPLACE_ME|TODO' "$PLATFORM_ENV" "$BOT_ENV" "$POSTGRES_ENV" >/dev/null; then
  fail "secret files still contain placeholder values"
fi

platform_bot_secret="$(env_value "$PLATFORM_ENV" BOT_POLLER_API_SECRET)"
bot_api_secret="$(env_value "$BOT_ENV" BOT_POLLER_API_SECRET)"
[[ "$platform_bot_secret" == "$bot_api_secret" ]] || fail "BOT_POLLER_API_SECRET differs between Platform and Bot"
platform_webhook_secret="$(env_value "$PLATFORM_ENV" PLATFORM_WEBHOOK_SECRET)"
bot_webhook_secret="$(env_value "$BOT_ENV" PLATFORM_WEBHOOK_SECRET)"
[[ "$platform_webhook_secret" == "$bot_webhook_secret" ]] || fail "PLATFORM_WEBHOOK_SECRET differs between Platform and Bot"

postgres_user="$(env_value "$POSTGRES_ENV" POSTGRES_USER)"
postgres_password="$(env_value "$POSTGRES_ENV" POSTGRES_PASSWORD)"
postgres_db="$(env_value "$POSTGRES_ENV" POSTGRES_DB)"
database_url="$(env_value "$PLATFORM_ENV" DATABASE_URL)"
expected_database_url="postgres://${postgres_user}:${postgres_password}@postgres:5432/${postgres_db}?sslmode=disable"
[[ "$database_url" == "$expected_database_url" ]] || fail "Platform DATABASE_URL does not match PostgreSQL credentials and service name"
unset platform_bot_secret bot_api_secret platform_webhook_secret bot_webhook_secret postgres_password database_url expected_database_url

cd "$SCRIPT_DIR"
tmp_config="$(mktemp)"
chmod 600 "$tmp_config"
trap 'rm -f "$tmp_config"' EXIT
docker compose --env-file .env -f compose.yaml config --format json >"$tmp_config"

python3 - "$tmp_config" "${GA_COMPOSE_ALLOW_MUTABLE_IMAGES:-0}" <<'PY'
import json
import re
import sys
from pathlib import Path

config = json.loads(Path(sys.argv[1]).read_text(encoding="utf-8"))
allow_mutable = sys.argv[2] == "1"
services = config.get("services") or {}
expected = {"postgres", "bot-poller", "platform", "web"}
if set(services) != expected:
    raise SystemExit(f"compose-preflight: FAIL: services must be exactly {sorted(expected)}")

for name, service in services.items():
    if service.get("privileged"):
        raise SystemExit(f"compose-preflight: FAIL: {name} is privileged")
    if service.get("network_mode") == "host" or service.get("pid") == "host" or service.get("ipc") == "host":
        raise SystemExit(f"compose-preflight: FAIL: {name} uses a host namespace")
    for mount in service.get("volumes") or []:
        source = str(mount.get("source", "")) if isinstance(mount, dict) else str(mount)
        target = str(mount.get("target", "")) if isinstance(mount, dict) else str(mount)
        value = (source + " " + target).lower()
        if "docker.sock" in value or "podman.sock" in value:
            raise SystemExit(f"compose-preflight: FAIL: {name} receives a container runtime socket")
    image = str(service.get("image") or "")
    if not allow_mutable and not re.fullmatch(r"[a-z0-9][a-z0-9._/:=-]*@sha256:[a-f0-9]{64}", image):
        raise SystemExit(f"compose-preflight: FAIL: {name} image is not repository@sha256 pinned")

for name, service in services.items():
    if service.get("restart") != "no":
        raise SystemExit(f"compose-preflight: FAIL: {name} can restart outside the systemd lifecycle owner")
    if not service.get("read_only"):
        raise SystemExit(f"compose-preflight: FAIL: {name} root filesystem is writable")
    if str(service.get("user", "")) in {"", "0", "0:0", "root"}:
        raise SystemExit(f"compose-preflight: FAIL: {name} must run as non-root")
    if "ALL" not in (service.get("cap_drop") or []):
        raise SystemExit(f"compose-preflight: FAIL: {name} must drop all capabilities")
    options = {str(value).lower() for value in service.get("security_opt") or []}
    if "no-new-privileges:true" not in options:
        raise SystemExit(f"compose-preflight: FAIL: {name} must set no-new-privileges")
    if not service.get("pids_limit") or not service.get("mem_limit") or not service.get("cpus"):
        raise SystemExit(f"compose-preflight: FAIL: {name} must set PID, memory and CPU limits")
    if not (service.get("ulimits") or {}).get("nofile"):
        raise SystemExit(f"compose-preflight: FAIL: {name} must set nofile limits")

if str(services["postgres"].get("user")) != "70:70":
    raise SystemExit("compose-preflight: FAIL: PostgreSQL must run as 70:70")
for name in ("platform", "bot-poller"):
    if {str(value) for value in services[name].get("group_add") or []} != {"10003"}:
        raise SystemExit(f"compose-preflight: FAIL: {name} must receive exactly the fixed delivery supplementary GID")

for name in ("platform", "postgres"):
    for port in services[name].get("ports") or []:
        host_ip = str(port.get("host_ip") or "") if isinstance(port, dict) else str(port).split(":", 1)[0]
        if host_ip not in {"127.0.0.1", "::1"}:
            raise SystemExit(f"compose-preflight: FAIL: {name} publishes a non-loopback port")

if services["web"].get("network_mode") != "service:platform":
    raise SystemExit("compose-preflight: FAIL: Web must share Platform network namespace for loopback API proxying")
PY

pass "private configuration, service set, image policy, namespaces, sockets, hardening, and loopback publications"
if [[ "${GA_COMPOSE_ALLOW_MUTABLE_IMAGES:-0}" == "1" ]]; then
  echo "compose-preflight: WARNING: mutable/local images accepted for staging build only" >&2
fi
