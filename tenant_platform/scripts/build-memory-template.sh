#!/usr/bin/env bash
# build-memory-template.sh — 从上游固定 commit 提取 memory/ 基线(方案 D6)
#
# 基线: upstream 9355c22d7 的已跟踪 memory/ 树(42 文件)。
# Git ignored 的运行时状态、用户记忆、文件访问统计、私有配置与下载内容
# 不进入模板(git archive 只含已跟踪文件,天然满足)。
#
# 输出: tenant_platform/infra/compose/memory-template/(gitignored 构建产物)
# 该目录由 ga-runner.Dockerfile COPY 为镜像内只读层 /ga/memory-template。

set -euo pipefail

cd "$(cd "$(dirname "$0")" && pwd)/../.."   # 仓库根(tenant_platform/scripts -> 仓库根)

BASE_COMMIT="${GA_MEMORY_BASE_COMMIT:-9355c22d724c5e2293f586d7223e28a9218c9f1f}"
OUT_DIR="tenant_platform/infra/compose/memory-template"

if ! git rev-parse --verify --quiet "$BASE_COMMIT" >/dev/null; then
  echo "错误: commit $BASE_COMMIT 不存在(需先 git fetch upstream)" >&2
  exit 1
fi

rm -rf "$OUT_DIR"
mkdir -p "$OUT_DIR"

# git archive 只包含已跟踪文件,逐文件解包(去掉顶层 memory/ 前缀)。
git archive --format=tar "$BASE_COMMIT" memory | tar -x -C "$OUT_DIR" --strip-components=1

expected="$(git ls-tree -r --name-only "$BASE_COMMIT" -- memory/ | wc -l)"
actual="$(find "$OUT_DIR" -type f | wc -l)"
if [ "$actual" -ne "$expected" ]; then
  echo "错误: 模板文件数 $actual != 期望 $expected" >&2
  exit 1
fi

# 与 git 树逐文件 diff 校验(含内容)。
tmp="$(mktemp -d)"
git archive --format=tar "$BASE_COMMIT" memory | tar -x -C "$tmp"
if ! diff -r "$tmp/memory" "$OUT_DIR" >/dev/null; then
  echo "错误: 模板与上游树不一致" >&2
  rm -rf "$tmp"
  exit 1
fi
rm -rf "$tmp"

echo "OK: memory-template 已生成于 $OUT_DIR ($actual 个文件, commit $BASE_COMMIT)"
