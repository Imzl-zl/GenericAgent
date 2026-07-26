# 租户平台故障排查指南

## 问题 1：管理员页面首页数据不对（硬编码假数据）

### 症状
管理员控制台首页显示固定的数字（待审批3、已批准12、运行中任务2、活跃Worker 4），不会随实际数据变化。

### 根本原因
`tenant_platform/web/src/features/admin/AdminDashboardPage.tsx` 中的数据是硬编码的，没有调用后端 API。

### 解决方案

#### 方案 A：快速修复 - 从现有 API 聚合数据（推荐）

在前端调用现有 API 并聚合统计：

```typescript
// web/src/api/stats.ts (新建)
import { api } from './client';
import { listPendingUsers } from './users';
import type { User } from './types';

export interface DashboardStats {
  pending_users: number;
  approved_users: number;
  running_tasks: number;
  active_workers: number;
}

export async function getDashboardStats(): Promise<DashboardStats> {
  // 从现有 API 聚合数据
  const pending = await listPendingUsers();
  
  // TODO: 添加其他统计 API 调用
  // const allUsers = await listAllUsers();
  // const tasks = await listRunningTasks();
  
  return {
    pending_users: pending.length,
    approved_users: 0, // TODO: 实现
    running_tasks: 0,  // TODO: 实现
    active_workers: 0, // TODO: 实现
  };
}
```

#### 方案 B：完整实现 - 后端统计 API（生产推荐）

1. **后端实现统计 API**

```go
// backend-go/internal/api/admin.go

type dashboardStatsResponse struct {
    PendingUsers   int `json:"pending_users"`
    ApprovedUsers  int `json:"approved_users"`
    RunningTasks   int `json:"running_tasks"`
    ActiveWorkers  int `json:"active_workers"`
}

func (s *Server) handleAdminDashboardStats(w http.ResponseWriter, r *http.Request) {
    tid := traceID()
    
    // 并发查询各项统计
    stats := dashboardStatsResponse{}
    
    // 查询待审批用户数
    if s.users != nil {
        if pending, err := s.users.CountPendingUsers(r.Context()); err == nil {
            stats.PendingUsers = pending
        }
        if approved, err := s.users.CountApprovedUsers(r.Context()); err == nil {
            stats.ApprovedUsers = approved
        }
    }
    
    // 查询运行中任务数
    if s.tasks != nil {
        if running, err := s.tasks.CountRunningTasks(r.Context()); err == nil {
            stats.RunningTasks = running
        }
    }
    
    // 查询活跃 Worker 数
    if s.workers != nil {
        if active, err := s.workers.CountActiveWorkers(r.Context()); err == nil {
            stats.ActiveWorkers = active
        }
    }
    
    writeJSON(w, http.StatusOK, stats)
}
```

2. **注册路由**

```go
// backend-go/internal/api/admin.go registerLifecycleRoutes()
s.mux.HandleFunc("GET /v1/admin/dashboard/stats", s.auth(s.handleAdminDashboardStats))
```

3. **前端调用**

```typescript
// web/src/features/admin/AdminDashboardPage.tsx
const [stats, setStats] = useState<DashboardStats | null>(null);

useEffect(() => {
  const loadStats = async () => {
    try {
      const data = await getDashboardStats();
      setStats(data);
    } catch (err) {
      console.error('加载统计失败', err);
    }
  };
  loadStats();
}, []);

// 渲染时使用 stats?.pending_users 代替硬编码的 3
```

---

## 问题 2：微信 Bot 发消息没有回应

### 症状
- 能成功扫码绑定微信
- 能在管理后台添加渠道
- 但在微信发消息，Bot 没有任何回应

### 根本原因

消息流转链路断裂，可能原因：

1. **Bot Poller 进程未运行**（最常见）
2. **PLATFORM_WEBHOOK_SECRET 未配置**
3. **Bot Poller 与 Platform 通信失败**

### 诊断步骤

#### 步骤 1：检查 Bot Poller 进程

```bash
# Linux/Mac
ps aux | grep poller

# Windows
tasklist | findstr python
# 或
Get-Process | Where-Object {$_.ProcessName -like "*python*"}
```

**预期结果：** 应该看到类似 `python tenant_platform/bot_poller/poller_server.py` 的进程

#### 步骤 2：检查配置文件

```bash
# 检查 .env 文件
cd tenant_platform
grep -E "PLATFORM_WEBHOOK_SECRET|BOT_POLLER" .env
```

**必需配置：**

```env
# .env 文件必须包含
PLATFORM_WEBHOOK_SECRET=your-secret-here-at-least-16-chars
BOT_POLLER_API_SECRET=another-secret-here
BOT_POLLER_URL=http://127.0.0.1:8081
PLATFORM_WEBHOOK_URL=http://127.0.0.1:8080/v1/im/webhook
```

**⚠️ 关键：** 如果 `PLATFORM_WEBHOOK_SECRET` 为空，Go Platform 会拒绝所有来自 Bot Poller 的 Webhook 请求！

#### 步骤 3：启动 Bot Poller

```bash
cd tenant_platform

# 方式 1：直接启动（开发）
python bot_poller/poller_server.py \
  --listen 127.0.0.1:8081 \
  --media-dir ./runtime/media \
  --webhook-secret "${PLATFORM_WEBHOOK_SECRET}" \
  --api-secret "${BOT_POLLER_API_SECRET}"

# 方式 2：使用 systemd（生产）
sudo systemctl start ga-bot-poller
sudo systemctl status ga-bot-poller
```

#### 步骤 4：验证通信链路

```bash
# 检查 Bot Poller 健康状态
curl http://127.0.0.1:8081/health

# 预期输出：
# {"healthy": true, "active_bots": []}
```

#### 步骤 5：查看日志

```bash
# Go Platform 日志
journalctl -u ga-platform -f

# Bot Poller 日志（如果用 systemd）
journalctl -u ga-bot-poller -f

# 手动启动时的标准输出
```

**关键日志内容：**

成功的 Webhook 流程：
```
[Bot Poller] bot_uuid=xxx webhook POST to http://...
[Go Platform] im_webhook: received message from bot_uuid=xxx
[Go Platform] router: routing message to user_id=123
```

失败的情况：
```
[Bot Poller] webhook PERMANENTLY rejected (bot_uuid=xxx) status=401
→ 说明签名验证失败，检查 PLATFORM_WEBHOOK_SECRET 配置

[Bot Poller] webhook post failed (bot_uuid=xxx) status=503
→ Platform 数据库连接失败或游标持久化失败
```

### 完整启动顺序

```bash
# 1. 启动数据库
cd tenant_platform/infra/postgres
docker-compose up -d

# 2. 运行迁移（如果需要）
cd ../../backend-go
./cmd/platform/platform --migrate-only

# 3. 启动 Go Platform
./cmd/platform/platform \
  --db-url "${DATABASE_URL}" \
  --webhook-secret "${PLATFORM_WEBHOOK_SECRET}"

# 4. 启动 Bot Poller
cd ../bot_poller
python poller_server.py \
  --webhook-secret "${PLATFORM_WEBHOOK_SECRET}" \
  --api-secret "${BOT_POLLER_API_SECRET}"

# 5. 启动前端（另一个终端）
cd ../web
npm run dev
```

### 快速修复清单

- [ ] 在 `.env` 中添加 `PLATFORM_WEBHOOK_SECRET`（至少16字符）
- [ ] 确保 Bot Poller 和 Platform 使用**相同的** `PLATFORM_WEBHOOK_SECRET`
- [ ] 启动 Bot Poller 进程
- [ ] 重启 Go Platform（如果它在 Bot Poller 之前启动）
- [ ] 在微信发送测试消息
- [ ] 检查日志确认消息流转

### 生产环境建议

1. **使用强随机密钥**
   ```bash
   # 生成安全的密钥
   openssl rand -hex 32
   ```

2. **使用进程管理器**
   - systemd（Linux）
   - supervisor
   - PM2（Node.js 生态）

3. **配置健康检查**
   ```bash
   # 添加到监控系统
   */5 * * * * curl -f http://127.0.0.1:8081/health || systemctl restart ga-bot-poller
   ```

4. **日志轮转**
   ```bash
   # /etc/logrotate.d/ga-bot-poller
   /var/log/ga-bot-poller.log {
       daily
       rotate 7
       compress
       missingok
       notifempty
   }
   ```

---

## 问题 3：其他常见问题

### 扫码绑定失败（404）

**症状：** 前端调用 `POST /v1/users/me/wechat-qrcode` 返回 404

**原因：** `.env` 中缺少 iLink 配置

**解决：**
```env
ILINK_BASE_URL=https://ilinkai.weixin.qq.com
ILINK_APP_ID=bot
ILINK_CLIENT_VERSION=2.1.1
```

### Worker 启动失败

检查：
- `GA_WORKER_PYTHON` 路径是否正确
- `GA_WORKER_SRC` 目录是否存在
- Python 依赖是否安装

---

## 快速诊断命令集

```bash
# 一键检查所有服务状态
echo "=== PostgreSQL ==="
docker ps | grep postgres

echo "=== Go Platform ==="
ps aux | grep platform | grep -v grep

echo "=== Bot Poller ==="
ps aux | grep poller | grep -v grep

echo "=== 配置检查 ==="
cd tenant_platform
grep -E "WEBHOOK_SECRET|POLLER" .env | grep -v "^#"

echo "=== 健康检查 ==="
curl -s http://127.0.0.1:8080/health || echo "Platform down"
curl -s http://127.0.0.1:8081/health || echo "Poller down"
```

---

## 联系支持

如果问题依然存在，请提供：
1. 完整的错误日志（Platform + Bot Poller）
2. `.env` 配置（脱敏后）
3. `ps aux` 输出
4. 数据库连接测试结果
