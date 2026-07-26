# Bot Poller API 认证机制

**日期：** 2026-07-26  
**状态：** 已实现

## 概述

Bot Poller 的 HTTP API（`/start` `/stop` `/send` `/health`）现已支持 HMAC-SHA256 签名认证，防止本地恶意进程伪造请求。

## 认证方式

### 请求签名（Go 平台 → Python Poller）

Go 平台调用 Poller API 时，在每个 POST 请求中附加 `X-API-Signature` 头：

```
X-API-Signature: hex(HMAC-SHA256(request_body, api_secret))
```

- **算法**：HMAC-SHA256
- **密钥**：由 `--bot-poller-api-secret` / `BOT_POLLER_API_SECRET` 环境变量配置
- **签名内容**：原始 JSON request body（deterministic serialization，与 webhook 签名一致）

### Webhook 签名（Python Poller → Go 平台）

Poller 转发入站消息到平台 `/v1/im/webhook` 时，使用 `X-Webhook-Signature` 头：

```
X-Webhook-Signature: hex(HMAC-SHA256(webhook_body, webhook_secret))
```

- **密钥**：由 `--webhook-secret` / `PLATFORM_WEBHOOK_SECRET` 配置
- 双向认证：Go → Poller 用 `api_secret`，Poller → Go 用 `webhook_secret`

## 配置示例

### 开发环境（无认证，不安全）

```bash
# Python Poller
python -m bot_poller.poller_server --listen 127.0.0.1:8090

# Go Platform
./platform --bot-poller-url http://127.0.0.1:8090
```

输出警告：
```
bot_poller listening on 127.0.0.1:8090 (api_auth=off (INSECURE - dev/test only), webhook_auth=off)
```

### 生产环境（推荐）

生成共享密钥：
```bash
# 生成 32 字节随机密钥（base64）
openssl rand -base64 32  # 用于 BOT_POLLER_API_SECRET
openssl rand -base64 32  # 用于 PLATFORM_WEBHOOK_SECRET
```

启动：
```bash
# Python Poller
export BOT_POLLER_API_SECRET="your-api-secret-here"
export PLATFORM_WEBHOOK_SECRET="your-webhook-secret-here"
python -m bot_poller.poller_server --listen 127.0.0.1:8090 \
  --api-secret "$BOT_POLLER_API_SECRET" \
  --webhook-secret "$PLATFORM_WEBHOOK_SECRET"

# Go Platform
export BOT_POLLER_API_SECRET="your-api-secret-here"
export PLATFORM_WEBHOOK_SECRET="your-webhook-secret-here"
./platform --bot-poller-url http://127.0.0.1:8090 \
  --bot-poller-api-secret "$BOT_POLLER_API_SECRET" \
  --webhook-secret "$PLATFORM_WEBHOOK_SECRET"
```

## 错误处理

### 签名缺失或错误

**请求**：
```bash
curl -X POST http://127.0.0.1:8090/start -d '{"bot_uuid":"test"}' -H "Content-Type: application/json"
```

**响应（401）**：
```json
{"error": "invalid or missing X-API-Signature"}
```

### GET /health 无需认证

健康检查接口始终可访问（监控需要）：
```bash
curl http://127.0.0.1:8090/health
# {"healthy": true, "active_bots": []}
```

## 安全建议

1. **生产环境必须启用**：`api_secret` 和 `webhook_secret` 都不能为空
2. **密钥轮换**：每 90 天轮换一次，平滑过渡需要短暂双密钥并存期
3. **传输加密**：仅支持 `127.0.0.1` loopback 或内网，不暴露公网
4. **备选方案**：若 Unix 系统，可改用 Unix domain socket + 文件权限隔离（需修改 `poller_server.py` bind 地址）

## 实现文件

- **Python 服务端**：`tenant_platform/bot_poller/poller_server.py:296-350` (`PollerHandler._verify_request_signature`)
- **Go 客户端**：`tenant_platform/backend-go/internal/infrastructure/poller/client.go:185-205` (`Client.post`)
- **平台配置**：`tenant_platform/backend-go/cmd/platform/main.go:265` (`--bot-poller-api-secret` flag)

## 向后兼容

- 空 `api_secret` = 无认证模式（dev/test）
- 旧代码未传 `X-API-Signature` 会被拒绝（需同步升级 Go + Python）
- 建议先在测试环境验证，再上生产
