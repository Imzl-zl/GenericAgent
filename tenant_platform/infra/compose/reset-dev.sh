#!/usr/bin/env bash
# reset-dev.sh — 开发期清库脚本(方案 D12: 清库式切换)
#
# 删除旧 PostgreSQL 数据卷与 Document 相关运行卷,按新 schema 启动。
# 适用: GA Sandbox Runner 重构启用新部署前,仓库处于开发阶段,无需要保留
# 的生产历史数据。此脚本破坏性:删除 postgres_data 等卷(默认不含
# runner_workspaces 持久工作区, 见 --all 说明)。
#
# 用法: infra/compose/reset-dev.sh [--all]
#   --all  同时删除 platform_runtime / platform_config / session_files /
#           bot_media / runner_workspaces
#           (完整重置;默认只删数据库与 document 卷)
#
# 注意(审查 F8): runner_workspaces 保存全部用户记忆/SOP/项目/文件, 属于
# 不可再生的用户数据, 默认不删除——仅在显式 --all 时清除, 且需要二次确认。

set -euo pipefail

cd "$(dirname "$0")"

if [[ "${1:-}" == "--help" || "${1:-}" == "-h" ]]; then
  sed -n '2,12p' "$0" | sed 's/^# \{0,1\}//'
  exit 0
fi

compose_file="compose.yaml"
project_name="genericagent"

echo "==> 停止现有服务(保留卷)"
docker compose -f "$compose_file" -p "$project_name" down --remove-orphans

volumes=(postgres_data)

if [[ "${1:-}" == "--all" ]]; then
  volumes+=(platform_runtime platform_config session_files bot_media runner_workspaces)
  echo "==> --all 模式: 同时删除运行时/会话/媒体/持久工作区卷"
  # 审查 F8: runner_workspaces 是不可再生的用户数据(记忆/SOP/项目/文件),
  # 删除前必须二次确认。
  read -r -p "确认删除 runner_workspaces 持久工作区卷? 输入 YES 继续: " confirm || true
  if [[ "$confirm" != "YES" ]]; then
    echo "已取消 runner_workspaces 删除; 其余 --all 卷继续处理"
    filtered=()
    for v in "${volumes[@]}"; do
      [[ "$v" == "runner_workspaces" ]] && continue
      filtered+=("$v")
    done
    volumes=("${filtered[@]}")
  fi
fi

# 旧 Document 系统的遗留卷(重构前部署)也一并清理, 避免残留。
for legacy in document_work document_artifacts; do
  if docker volume inspect "${project_name}_${legacy}" >/dev/null 2>&1; then
    docker volume rm "${project_name}_${legacy}"
    echo "    removed legacy ${project_name}_${legacy}"
  fi
  if docker volume inspect "${legacy}" >/dev/null 2>&1; then
    docker volume rm "${legacy}"
    echo "    removed legacy ${legacy}"
  fi
done

echo "==> 删除卷: ${volumes[*]}"
for v in "${volumes[@]}"; do
  # runner_workspaces 在 compose.yaml 显式 name(无 project 前缀, 供
  # volume-subpath 挂载); 其余卷为默认 ${project_name}_<name> 命名。
  if [[ "$v" == "runner_workspaces" ]]; then
    full="$v"
  else
    full="${project_name}_${v}"
  fi
  if docker volume inspect "$full" >/dev/null 2>&1; then
    docker volume rm "$full"
    echo "    removed $full"
  else
    echo "    skip (absent) $full"
  fi
done

echo "==> 按新 schema 启动"
# 生产 .env 的 GA_RUNNER_IMAGE 是 digest 引用, up --build 会报 build tag
# cannot contain a digest; 镜像由 make build 预构建, 这里只启动不构建。
docker compose -f "$compose_file" -p "$project_name" up -d

echo "==> 完成: 旧数据卷已清除,新 schema 已启动"
