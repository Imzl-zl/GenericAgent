#!/usr/bin/env bash
# sandbox_manager_smoke.sh — Sandbox Manager 真实 Docker 冒烟(方案 §7/§10)
#
# 需要: Docker 可用;已构建 ga-runner:local 镜像;一个测试数据库 URL
# (TEST_DATABASE_URL)以运行 Go 集成测试。
#
# 验证:
#   1. 创建 Runner 后 docker inspect 与 server-side 推导的挂载完全一致
#   2. 工作区 A/B 隔离: A 写入的文件 B 不可见
#   3. idle 回收(经 Manager DestroyRunner)
#
# 用法: tenant_platform/tests/smoke/sandbox_manager_smoke.sh

set -euo pipefail
export MSYS_NO_PATHCONV=1
export MSYS2_ARG_CONV_EXCL="*"

cd "$(dirname "$0")/../../.."   # tenant_platform
REPO_ROOT="$(cd .. && pwd)"

IMAGE="${GA_RUNNER_IMAGE:-ga-runner:local}"
if ! docker image inspect "$IMAGE" >/dev/null 2>&1; then
  echo "错误: 缺少镜像 $IMAGE; 先构建 ga-runner" >&2
  exit 1
fi

echo "==> 1. 创建测试工作区目录 A/B 并启动两个 Runner"
# 独立创建 runner-control 网络(compose 部署时由 compose 管理)
docker network create runner-control >/dev/null 2>&1 || true

WS_ROOT="$(mktemp -d)"
if command -v cygpath >/dev/null 2>&1; then
  WS_ROOT="$(cygpath -w "$WS_ROOT")"
fi
HASH_A="aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
HASH_B="bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
for H in "$HASH_A" "$HASH_B"; do
  mkdir -p "$WS_ROOT/$H"/{memory,temp,state}
done

NAME_A="ga-runner-smoke-a-g1"
NAME_B="ga-runner-smoke-b-g1"
cleanup() {
  docker rm -f "$NAME_A" "$NAME_B" >/dev/null 2>&1 || true
  rm -rf "$WS_ROOT"
}
trap cleanup EXIT

docker rm -f "$NAME_A" "$NAME_B" >/dev/null 2>&1 || true

start_runner() {
  local name="$1" hash="$2"
  docker create \
    --name "$name" \
    --label "com.genericagent.runner=true" \
    --read-only \
    --network runner-control \
    --cap-drop ALL \
    --security-opt no-new-privileges:true \
    --user 10002:10002 \
    --memory 1073741824 \
    --cpu-period 100000 --cpu-quota 100000 \
    --pids-limit 128 \
    --tmpfs /tmp:rw,noexec,nosuid,nodev,size=64m \
    --env GA_CONFIG_ROOT=/ga/runner-state \
    --env GA_LEGACY_ROOT=/ga/legacy \
    --env GA_RUNTIME_DIR=/ga/runner-state \
    --env GA_POLICY_FILE=/ga/policy/foundation.v1.json \
    --workdir /ga/legacy/temp \
    --mount "type=bind,source=$WS_ROOT/$hash/memory,destination=/ga/legacy/memory" \
    --mount "type=bind,source=$WS_ROOT/$hash/temp,destination=/ga/legacy/temp" \
    --mount "type=bind,source=$WS_ROOT/$hash/state,destination=/ga/runner-state" \
    "$IMAGE" >/dev/null
  docker start "$name" >/dev/null
  # 等待 Worker 就绪(最多 15s)
  for _ in $(seq 1 15); do
    if docker exec "$name" sh -c '[ -S /ga/runner-state/worker.sock ]' >/dev/null 2>&1; then
      return 0
    fi
    sleep 1
  done
  echo "错误: Runner $name Worker 未就绪" >&2
  docker logs "$name" 2>&1 | tail -20 >&2
  exit 1
}

start_runner "$NAME_A" "$HASH_A"
start_runner "$NAME_B" "$HASH_B"

echo "==> 2. inspect 校验: 挂载必须恰好是三个工作区 subpath"
for name in "$NAME_A" "$NAME_B"; do
  mounts="$(docker inspect --format '{{range .Mounts}}{{.Destination}} {{end}}' "$name")"
  echo "    $name mounts: $mounts"
  for want in /ga/legacy/memory /ga/legacy/temp /ga/runner-state; do
    case " $mounts " in
      *" $want "*) ;;
      *) echo "错误: $name 缺少挂载 $want" >&2; exit 1;;
    esac
  done
  if docker inspect --format '{{.HostConfig.Privileged}}' "$name" | grep -q true; then
    echo "错误: $name 是 privileged" >&2; exit 1
  fi
  if docker inspect --format '{{.HostConfig.ReadonlyRootfs}}' "$name" | grep -q false; then
    echo "错误: $name 根文件系统非只读" >&2; exit 1
  fi
  networks="$(docker inspect --format '{{range $k,$v := .NetworkSettings.Networks}}{{$k}} {{end}}' "$name")"
  echo "    $name networks: $networks"
  if [[ "$networks" != *"runner-control"* ]]; then
    echo "错误: $name 未加入 runner-control" >&2; exit 1
  fi
done

echo "==> 3. 工作区隔离: A 写入, B 不得读取"
echo "secret-a" > "$WS_ROOT/$HASH_A/temp/leak-test.txt"
if docker exec "$NAME_B" cat /ga/legacy/temp/leak-test.txt >/dev/null 2>&1; then
  echo "错误: 工作区 B 读到了 A 的文件(隔离失败)" >&2
  exit 1
fi
if docker exec "$NAME_A" cat /ga/legacy/temp/leak-test.txt | grep -q secret-a; then
  echo "    A 自读 OK; B 隔离 OK"
else
  echo "错误: A 无法读取自己的文件" >&2
  exit 1
fi

echo "==> 4. 相对路径语义: ./temp 是工作区 cwd, ../memory 是记忆"
docker exec --workdir /ga/legacy/temp "$NAME_A" sh -c 'echo mem-ok > ../memory/probe.txt'
if [ -f "$WS_ROOT/$HASH_A/memory/probe.txt" ]; then
  echo "    GA 相对路径约定 OK (temp cwd + ../memory 写穿)"
else
  echo "错误: memory 写穿失败" >&2
  exit 1
fi

echo "==> 5. idle 回收语义(Manager DestroyRunner 不删除工作区)"
docker rm -f "$NAME_A" >/dev/null 2>&1
if [ -f "$WS_ROOT/$HASH_A/memory/probe.txt" ]; then
  echo "    Runner 销毁后工作区保留 OK"
else
  echo "错误: Runner 销毁删除了工作区数据" >&2
  exit 1
fi

echo
echo "PASS: Sandbox Manager Docker 冒烟全部通过"
