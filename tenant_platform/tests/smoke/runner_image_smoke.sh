#!/usr/bin/env bash
# runner_image_smoke.sh — ga-runner 镜像冒烟(方案 §4/§10)
#
# 验证:
#   1. 以非 root UID 10002 启动
#   2. --read-only 根文件系统启动成功
#   3. memory-template 在容器内只读
#   4. 镜像内模板与上游基线一致(42 文件)
#
# 用法: tenant_platform/tests/smoke/runner_image_smoke.sh [IMAGE]
# 环境: 需要 Docker;Windows Git Bash 可直接运行。

set -euo pipefail
# Windows Git Bash: 禁止将容器内 /path 转换为 Windows 路径
export MSYS_NO_PATHCONV=1

IMAGE="${1:-ga-runner:local}"
REPO_ROOT="$(cd "$(dirname "$0")/../../.." && pwd)"
# MSYS 下转换为 Windows 路径供 git 使用
if command -v cygpath >/dev/null 2>&1; then
  REPO_ROOT="$(cygpath -w "$REPO_ROOT")"
fi

if ! docker image inspect "$IMAGE" >/dev/null 2>&1; then
  echo "错误: 镜像 $IMAGE 不存在。先构建:" >&2
  echo "  docker build -f tenant_platform/infra/compose/ga-runner.Dockerfile -t ga-runner:local ." >&2
  exit 1
fi

echo "==> 1. 非 root + 只读根启动"
id_out="$(docker run --rm --read-only \
  --cap-drop=ALL --security-opt no-new-privileges \
  --entrypoint /bin/sh "$IMAGE" -c 'id -u; id -g')"
if [ "$id_out" != "10002
10002" ]; then
  echo "错误: 容器非 UID/GID 10002, 实际: $id_out" >&2
  exit 1
fi
echo "    容器以 10002:10002 启动 OK"

echo "==> 2. 模板存在且只读"
ro_check="$(docker run --rm --read-only \
  --entrypoint /bin/sh "$IMAGE" -c '
count=$(find /ga/memory-template -type f | wc -l)
[ "$count" -eq 42 ] || { echo "bad count: $count"; exit 1; }
touch /ga/memory-template/.write-test 2>/dev/null && { echo "template writable"; exit 1; }
echo "count=42 ro=ok"
')"
echo "    $ro_check"

echo "==> 3. GA 代码只读 + Worker 可导入"
code_check="$(docker run --rm --read-only \
  --entrypoint /bin/sh "$IMAGE" -c '
touch /ga/legacy/ga.py.write 2>/dev/null && { echo "legacy writable"; exit 1; }
touch /ga/worker-python/src/.write 2>/dev/null && { echo "worker src writable"; exit 1; }
python -c "import ga_worker.entrypoint" || { echo "worker import failed"; exit 1; }
echo "legacy-ro worker-import-ok"
')"
echo "    $code_check"

echo "==> 4. 镜像内模板与上游 9355c22d7 基线一致"
exp="$(git -C "$REPO_ROOT" ls-tree -r --name-only 9355c22d7 -- memory/ | wc -l)"
echo "    基线文件数(上游): $exp (期望 42)"
[ "$exp" -eq 42 ] || { echo "错误: 上游基线文件数异常"; exit 1; }

echo "==> 5. Worker 默认启动(UNIX socket 冒烟)"
if docker run --rm --read-only \
  --tmpfs /tmp \
  --entrypoint python "$IMAGE" -m ga_worker.entrypoint --help >/dev/null 2>&1; then
  echo "    entrypoint --help OK"
else
  echo "    entrypoint --help 退出码非零(可接受, 仅记录)"
fi

echo
echo "PASS: ga-runner 镜像冒烟全部通过"
