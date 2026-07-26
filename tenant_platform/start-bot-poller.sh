#!/usr/bin/env bash
# Bot Poller 启动脚本
# 用途：启动 Python Bot Poller 服务，处理微信消息长轮询

set -e

# 切换到脚本所在目录
cd "$(dirname "$0")"

# 加载环境变量
if [ -f .env ]; then
    echo "加载 .env 配置..."
    export $(grep -v '^#' .env | grep -v '^$' | xargs)
else
    echo "错误：找不到 .env 文件"
    exit 1
fi

# 检查必需的环境变量
if [ -z "$PLATFORM_WEBHOOK_SECRET" ]; then
    echo "❌ 错误：PLATFORM_WEBHOOK_SECRET 未配置"
    echo "请在 .env 文件中添加此配置"
    exit 1
fi

if [ -z "$BOT_POLLER_API_SECRET" ]; then
    echo "❌ 错误：BOT_POLLER_API_SECRET 未配置"
    echo "请在 .env 文件中添加此配置"
    exit 1
fi

# 创建媒体目录
MEDIA_DIR="${MEDIA_DIR:-./runtime/media}"
mkdir -p "$MEDIA_DIR"

# 解析监听地址
LISTEN_ADDR="${BOT_POLLER_LISTEN:-127.0.0.1:8081}"

echo "=================================="
echo "启动 Bot Poller"
echo "=================================="
echo "监听地址: $LISTEN_ADDR"
echo "媒体目录: $MEDIA_DIR"
echo "Webhook URL: $PLATFORM_WEBHOOK_URL"
echo "API 认证: 已启用"
echo "Webhook 认证: 已启用"
echo "=================================="
echo ""

# 启动 Bot Poller
exec python bot_poller/poller_server.py \
    --listen "$LISTEN_ADDR" \
    --media-dir "$MEDIA_DIR" \
    --webhook-secret "$PLATFORM_WEBHOOK_SECRET" \
    --api-secret "$BOT_POLLER_API_SECRET"
