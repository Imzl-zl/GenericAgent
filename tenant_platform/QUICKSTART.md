# 快速启动指南

本指南帮助你快速启动 GenericAgent 租户平台的所有服务。

## 启动前检查

```bash
# 1. 确认 .env 文件已配置
cd tenant_platform
cat .env | grep -E "PLATFORM_WEBHOOK_SECRET|BOT_POLLER_API_SECRET"

# 应该看到两行配置（非空）：
# PLATFORM_WEBHOOK_SECRET=c3579b89dc80c648720d7169fa6557c534663ef73ba990a50d19858677a09f39
# BOT_POLLER_API_SECRET=47956958f475381489a3ee1059366af37403472e4d1ddd1167c0f8cf067aef53
```

## 启动顺序

### 1. 启动 PostgreSQL

```bash
cd infra/postgres
docker-compose up -d

# 验证启动成功
docker ps | grep postgres
```

### 2. 启动 Go Platform

```bash
cd ../../backend-go

# 首次启动需要运行迁移
go run ./cmd/platform --migrate-only

# 启动 Platform
go run ./cmd/platform

# 或者使用编译好的二进制
./cmd/platform/platform
```

**预期输出：**
```
platform: listening on :8080
platform: restored 0 active bots
```

### 3. 启动 Bot Poller（新建终端）

```bash
cd tenant_platform

# Windows
start-bot-poller.bat

# Linux/Mac
./start-bot-poller.sh
```

**预期输出：**
```
==================================
启动 Bot Poller
==================================
监听地址: 127.0.0.1:8081
媒体目录: ./runtime/media
Webhook URL: http://127.0.0.1:8080/v1/im/webhook
API 认证: 已启用
Webhook 认证: 已启用
==================================

bot_poller listening on 127.0.0.1:8081
```

### 4. 启动前端开发服务器（新建终端）

```bash
cd tenant_platform/web

# 安装依赖（首次）
npm install

# 启动开发服务器
npm run dev
```

**预期输出：**
```
  VITE v5.x.x  ready in xxx ms

  ➜  Local:   http://localhost:5173/
  ➜  Network: use --host to expose
```

## 验证服务状态

```bash
# 检查 PostgreSQL
docker ps | grep postgres

# 检查 Go Platform
curl http://127.0.0.1:8080/health
# 预期: {"status":"ok"}

# 检查 Bot Poller
curl http://127.0.0.1:8081/health
# 预期: {"healthy":true,"active_bots":[]}

# 检查前端
curl http://localhost:5173
# 预期: 返回 HTML 内容
```

## 访问平台

1. **前端界面：** http://localhost:5173
2. **管理员登录：**
   - 用户名：`admin`
   - 密码：（你在 `.env` 中配置的密码，或使用创建时的密码）

## 配置 LLM Provider（必需）

1. 登录管理后台，进入“LLM Providers”。
2. 创建 `native_oai` 或 `native_claude` Provider，填写 Base URL、Model 和 API Key。
3. 按需配置 GA Session 参数与 Proxy Transport 参数，并将一个可用 Provider 设为默认。

API Key 由 Platform 加密写入 PostgreSQL，且不会通过详情接口回显。Worker 运行时只接收 Scheduler 签发的 capability token 和 Proxy URL；不要把真实上游 Key 写入 Worker 环境、`mykey.py` 或静态 Platform 上游环境变量。Platform 启动环境仍需配置数据库、`BOT_TOKEN_KEY` 和 `LLM_PROXY_CAPABILITY_SIGNING_KEY`。

## 测试微信 Bot

1. 登录管理后台
2. 进入"设置" -> "微信绑定"
3. 点击"生成二维码"
4. 使用微信扫码绑定
5. 绑定成功后，在微信发送消息测试

**预期日志（Bot Poller）：**
```
[Poller] bot_uuid=xxx webhook POST to http://127.0.0.1:8080/v1/im/webhook
```

**预期日志（Go Platform）：**
```
im_webhook: received message bot_uuid=xxx
router: routing message to user_id=1
```

## 常见问题排查

### 问题 1：Bot Poller 启动失败

**症状：**
```
❌ 错误：PLATFORM_WEBHOOK_SECRET 未配置
```

**解决：**
检查 `.env` 文件是否包含 `PLATFORM_WEBHOOK_SECRET` 配置。

---

### 问题 2：微信消息无响应

**诊断：**
```bash
# 检查 Bot Poller 是否运行
ps aux | grep poller

# 检查日志
# Bot Poller 日志应该显示收到消息
# Platform 日志应该显示处理消息
```

**解决：**
1. 确认 Bot Poller 正在运行
2. 确认 `PLATFORM_WEBHOOK_SECRET` 在 `.env` 中已配置
3. 重启 Go Platform（确保读取到新的环境变量）

---

### 问题 3：管理员页面数据显示异常

**症状：**
控制台显示 "加载统计数据失败"

**解决：**
1. 检查后端是否启动成功
2. 检查浏览器控制台的网络请求
3. 确认 `/v1/admin/dashboard/stats` 端点返回数据

---

### 问题 4：数据库连接失败

**症状：**
```
platform: failed to connect to database
```

**解决：**
```bash
# 检查 PostgreSQL 是否运行
docker ps | grep postgres

# 检查 .env 中的 DATABASE_URL 是否正确
grep DATABASE_URL .env

# 重启 PostgreSQL
cd infra/postgres
docker-compose restart
```

## 停止服务

```bash
# 停止 Bot Poller
# Windows: 关闭终端或按 Ctrl+C
# Linux/Mac: 按 Ctrl+C

# 停止 Go Platform
# 按 Ctrl+C

# 停止前端开发服务器
# 按 Ctrl+C

# 停止 PostgreSQL
cd infra/postgres
docker-compose down
```

## 生产部署

安全文档平台的生产部署不使用本节早期的通用启动命令。单机自助部署优先使用 [`infra/compose/README.zh-CN.md`](infra/compose/README.zh-CN.md)：Web、Platform+Python 子 Worker、Bot Poller 和 PostgreSQL 由 hardened Compose 启动，Document Manager 仍由宿主 systemd 管理独立 rootless runtime。全 systemd 方案见 [`infra/deploy/README.md`](infra/deploy/README.md)。两种方案都包含：

- rootless Docker/Podman 与 cgroup v2 的 fail-closed preflight；
- digest-pinned document image、私有环境文件和明确启动顺序；
- 只有 `ga-document-manager` 持有文档 runtime 权限，Compose 应用不挂任何 runtime socket；
- 真实容器、管理员控制面、浏览器/微信交付和 secret journal 检查；
- 明确的停池、应用/二进制回滚和数据库恢复边界。

目标主机未通过所选 profile 的全部门禁前，不得启用文档池。

## 开发技巧

### 热重载

- **前端：** Vite 自动热重载，修改代码后浏览器自动刷新
- **后端：** 需要手动重启 Go Platform
- **Bot Poller：** 需要手动重启

### 调试模式

```bash
# Go Platform 开启详细日志
export LOG_LEVEL=debug
go run ./cmd/platform

# Bot Poller 查看详细输出
# 日志已输出到标准输出，无需额外配置
```

### 数据库管理

```bash
# 连接到数据库
docker exec -it postgres_container psql -U admin -d genericagent

# 查看所有表
\dt

# 查看用户
SELECT id, username, status FROM users;

# 查看 Bot
SELECT id, bot_uuid, owner_id, state FROM bots;
```

---

**需要帮助？** 查看 `TROUBLESHOOTING.md` 获取详细的故障排查指南。
