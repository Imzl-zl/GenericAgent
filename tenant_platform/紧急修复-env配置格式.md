# 🔧 紧急修复：.env 配置文件格式问题

## 问题原因

你遇到的错误：
```
ILINK_QR_FAILED: ilink get qrcode: build request: parse "https://ilinkai.weixin.qq.com ": invalid character " " in host name
```

**根本原因**：`.env` 文件中的配置项后面有**行内注释**，例如：

```bash
ILINK_BASE_URL=https://ilinkai.weixin.qq.com  # 微信官方 iLink 服务器
```

PowerShell 的 `start-backend.ps1` 使用简单的字符串分割来解析 `.env`：

```powershell
$parts = $_ -split "=", 2
[Environment]::SetEnvironmentVariable($parts[0].Trim(), $parts[1].Trim(), "Process")
```

这会把整行都当作值，包括：
- URL 后面的空格
- `#` 号
- 中文注释

最终 `ILINK_BASE_URL` 的值变成了：
```
"https://ilinkai.weixin.qq.com  # 微信官方 iLink 服务器（腾讯提供）"
```

URL 解析器遇到空格就报错了。

## 已修复

已经移除了所有行内注释，现在配置是：

```bash
# 注释在独立行
ILINK_BASE_URL=https://ilinkai.weixin.qq.com
ILINK_APP_ID=bot
ILINK_CLIENT_VERSION=2.1.1

# 注释在独立行
GA_RUNTIME_DIR=./runtime
GA_CONFIG_ROOT=./config
GA_LEGACY_ROOT=..
GA_WORKER_PYTHON=python
GA_WORKER_SRC=./worker-python/src
```

## 立即操作

**重启 Backend 服务**：

```powershell
# 1. 关闭当前 Backend 窗口（Ctrl+C）
# 2. 重新运行：
.\start-backend.ps1
```

**验证启动成功**：

日志应该显示：
```
platform: wechat qr binding enabled base_url=https://ilinkai.weixin.qq.com
```

注意：URL 后面**没有空格或其他字符**！

## 测试

1. 刷新浏览器（Ctrl + Shift + R 强制刷新）
2. 进入管理后台的"微信绑定"页面
3. 点击"获取二维码"
4. **应该成功显示二维码！**

## 经验教训

**`.env` 文件格式规范**：

✅ **正确**：
```bash
# 这是注释
KEY=value
```

❌ **错误**：
```bash
KEY=value  # 行内注释会被当作值的一部分
```

**为什么会这样？**

不同的 `.env` 解析器行为不同：
- 复杂的解析器（如 dotenv 库）支持行内注释
- 简单的 PowerShell 脚本不支持

我们的 `start-backend.ps1` 使用的是简单解析，所以必须把注释放在独立行。

## 如果还报错

1. **检查环境变量**：
   ```powershell
   # 在 Backend 窗口运行：
   $env:ILINK_BASE_URL
   ```
   
   应该输出：
   ```
   https://ilinkai.weixin.qq.com
   ```
   
   如果输出有多余字符，说明 `.env` 还有问题。

2. **手动设置测试**：
   ```powershell
   $env:ILINK_BASE_URL = "https://ilinkai.weixin.qq.com"
   $env:ILINK_APP_ID = "bot"
   $env:ILINK_CLIENT_VERSION = "2.1.1"
   
   # 然后运行后端
   cd backend-go
   go run ./cmd/platform/main.go --dev-loopback ...
   ```

---

**修复完成时间**：2026-07-26  
**下一步**：重启 Backend，刷新浏览器，测试绑定！
