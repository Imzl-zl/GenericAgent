# iLink / 微信 ClawBot 官方绑定流程

**日期：** 2026-07-25  
**来源：** 微信官方 iLink Bot Protocol（`https://ilinkai.weixin.qq.com`）  
**目的：** 作为平台微信绑定功能的事实来源，避免后续实现跑偏。

## 1. 官方协议概述

微信 ClawBot 底层基于 iLink（智能连接）协议，接入域名为腾讯官方服务器：

- 基础 URL：`https://ilinkai.weixin.qq.com`
- CDN URL：`https://novac2c.cdn.weixin.qq.com/c2c`
- 协议格式：HTTP/JSON
- 认证方式：QR 扫码后获取的 Bearer Token

架构模型：

```text
微信用户 (iOS) → ClawBot 插件 → 腾讯 iLink 服务器 → 平台 Bot 服务（长轮询）
```

腾讯定位为纯消息管道，不存储用户输入和 AI 输出。

## 2. 公共请求头

所有 API 请求携带：

```text
iLink-App-Id: bot
iLink-App-ClientVersion: {uint32}
```

POST 请求额外携带：

```text
Content-Type: application/json
AuthorizationType: ilink_bot_token
Authorization: Bearer {bot_token}
X-WECHAT-UIN: {base64(String(randomUint32()))}
```

`X-WECHAT-UIN` 每次请求生成新的随机 uint32，用于防重放。

## 3. 扫码登录流程

### 3.1 获取二维码

```http
GET /ilink/bot/get_bot_qrcode?bot_type=3
```

响应：

```json
{
  "qrcode": "会话标识符",
  "qrcode_img_content": "https://liteapp.weixin.qq.com/q/xxx?qrcode=...&bot_type=3",
  "ret": 0
}
```

`qrcode_img_content` 是一个 URL 字符串（例如指向微信小页面的链接）。前端**不要直接把它当作图片 URL 用 `<img>` 展示**，而应该用这个字符串作为数据、本地生成一张二维码图片。用户扫描后微信会打开这个链接完成授权。

> 参考 GA Core 的 [`frontends/wechatapp.py:login_qr()`](file:///c:/sudy/github/GenericAgent/frontends/wechatapp.py#L65) 实现：它拿到 `qrcode_img_content` 后用 `qrcode` 库生成 ASCII/PNG 二维码。平台前端已使用 `qrcode.react` 做同样的事。

### 3.2 轮询扫码状态

```http
GET /ilink/bot/get_qrcode_status?qrcode={qrcode}
```

使用公共请求头（不带 Authorization）。

状态机：

| status | 含义 |
|--------|------|
| `wait` | 等待扫码 |
| `scaned` | 已扫码，等待用户确认 |
| `scaned_but_redirect` | 已扫码，需 IDC 重定向 |
| `expired` | 二维码过期 |
| `confirmed` | 登录成功 |

登录成功响应：

```json
{
  "status": "confirmed",
  "ilink_bot_id": "xxx@im.bot",
  "bot_token": "eyJhbG...",
  "baseurl": "https://ilinkai.weixin.qq.com",
  "ilink_user_id": "wxid_abc@im.wechat"
}
```

二维码有效期约 5 分钟，过期后需重新获取。

## 4. 平台集成映射

### 4.1 用户绑定流程

1. 用户在平台 Web 端点击“绑定微信”。
2. 前端调用 `POST /v1/users/me/wechat-qrcode`。
3. 平台后端调用 iLink `get_bot_qrcode`，生成本地 `wechat_qr_session` 记录。
4. 前端展示返回的 `qrcode_url`。
5. 前端轮询 `GET /v1/users/me/wechat-qrcode/status?qrcode_token=...`。
6. 平台后端每次收到轮询请求时，调用 iLink `get_qrcode_status` 并更新本地记录。
7. 状态为 `confirmed` 时：
   - 使用返回的凭据在 `bots` 表创建一条 active 记录；
   - `ilink_bot_id`、`ilink_user_id`、`baseurl` 直接保存；
   - `bot_token` 加密后保存（`token_ciphertext`）。
8. 前端收到 `confirmed` 后显示绑定成功。

### 4.2 关键字段对应

| iLink 字段 | 平台字段 | 说明 |
|------------|----------|------|
| `ilink_bot_id` | `bots.ilink_bot_id` | Bot 在 iLink 侧的身份标识 |
| `ilink_user_id` | `bots.ilink_user_id` | 微信用户在 iLink 侧的身份标识 |
| `bot_token` | `bots.token_ciphertext` | 加密存储，用于后续发消息 |
| `baseurl` | `bots.baseurl` | 后续 API 调用的 base URL |

## 5. 消息收发（绑定后）

### 5.1 接收消息

通过 `POST /ilink/bot/getupdates` 长轮询：

```json
{
  "get_updates_buf": "",
  "base_info": { "channel_version": "2.1.1" }
}
```

响应包含 `msgs` 和新的 `get_updates_buf`（游标，必须持久化）。

**入站消息类型**（`item_list[].type`）：

| type | item_key       | 说明                          |
|------|----------------|-------------------------------|
| 1    | `text_item`    | 文本                          |
| 2    | `image_item`   | 图片（含缩略图，CDN 加密）   |
| 3    | `voice_item`   | 语音（.silk）                 |
| 4    | `file_item`    | 文件                          |
| 5    | `video_item`   | 视频                          |

下载链路：`GET https://novac2c.cdn.weixin.qq.com/c2c/download?encrypted_query_param=...` → AES-128-ECB 解密。GA Core 的 `frontends/wxbot_media.py` 已实现 4 种入站媒体的下载与解密。

### 5.2 发送消息

通过 `POST /ilink/bot/sendmessage`：

- 必须回传 incoming message 中的 `context_token`。
- 需要 `to_user_id`、`message_type` 等字段。

**出站消息类型**（`item_list[].type`）：text / image / file / video（voice 不支持出站）。媒体上传链路：`POST /ilink/bot/getuploadurl` 拿 `upload_param` → `POST <cdn>/upload`（AES-ECB 加密裸字节）→ `sendmessage` 带 `image_item`/`video_item`/`file_item`。GA Core 的 `frontends/wxbot_client.py` 已实现 `send_image`/`send_video`/`send_file`。

## 6. 实现注意（别再搞错）

### 6.1 响应字段是平铺的，不是嵌套的

`get_qrcode_status` 在 `confirmed` 时返回的凭证字段是**顶层字段**，不是嵌在 `credentials` 对象里。之前代码写成嵌套结构导致扫码成功后读不到 `bot_token`，绑定失败。

正确结构：

```go
type QRCodeStatusResponse struct {
    Status       QRCodeStatus `json:"status"`
    RedirectHost string       `json:"redirect_host,omitempty"`
    Ret          int          `json:"ret"`

    // confirmed 时直接在顶层返回
    ILinkBotID  string `json:"ilink_bot_id,omitempty"`
    BotToken    string `json:"bot_token,omitempty"`
    BaseURL     string `json:"baseurl,omitempty"`
    ILinkUserID string `json:"ilink_user_id,omitempty"`
}
```

错误结构（已修正）：

```go
// 不要这样写
Credentials *Credentials `json:"credentials,omitempty"`
```

### 6.2 必须检查 `ret`

iLink 业务错误通过 `ret` 返回，HTTP status 仍然是 200。`get_bot_qrcode` 和 `get_qrcode_status` 都要判断 `ret != 0` 再报错。

### 6.3 `baseurl` 回退

如果 `confirmed` 响应里没有 `baseurl`，则使用 iLink 客户端初始化时的 `BaseURL`（默认 `https://ilinkai.weixin.qq.com`）。

### 6.4 测试 mock 必须按官方格式返回

本地 mock 或测试时应返回：

```json
{
  "status": "confirmed",
  "ilink_bot_id": "bot@im.bot",
  "bot_token": "tok123",
  "baseurl": "http://127.0.0.1:9999",
  "ilink_user_id": "wxid_abc",
  "ret": 0
}
```

### 6.5 其他禁忌

- 不要让用户手动填写 `ilink_bot_id` 或 `bot_token`，官方流程是扫码获取。
- 不要使用 `/activate <code>` 作为微信绑定手段，那是平台内部账号验证的语义，不适用于 iLink 官方扫码登录。
- `bot_token` 必须加密存储，不能明文落库。
- `get_updates_buf` 需要持久化，否则重启后会重复收消息或丢失消息。
- 一个 `ilink_user_id` 对应一个微信用户；一个平台用户可以绑定一个 bot。

## 7. 平台层如何封装 GA Core 的绑定能力

GA Core 的 [`frontends/wechatapp.py`](file:///c:/sudy/github/GenericAgent/frontends/wechatapp.py) 已经实现了完整的单用户 iLink 协议（扫码登录、长轮询收消息、发文本/媒体、Token 持久化、重试/限流容错）。平台层不需要重写协议细节，但要把它从“单用户本地文件”改造为“多租户数据库 + 后端服务”。

### 7.1 复用 vs 封装边界

| 能力 | GA Core 已有 | 平台层实现 |
|------|-------------|-----------|
| iLink QR 登录协议 | `WxBotClient.login_qr()` | Go `internal/ilink.Client` 复刻 QR 登录（控制面） |
| Token 持久化 | `~/.wxbot/token.json` | `bots` 表 `token_ciphertext` 加密存储 |
| 收消息长轮询 | `WxBotClient.get_updates()` | Python Bot Poller 复用 `WxBotClient`，Go 通过 HTTP 委托 |
| 发文本/媒体 | `WxBotClient.send_text/_media()` | Go `ILinkAdapter` → `poller.Client` → Python Poller → iLink |
| 消息路由到 GA | `on_message()` 直接调 `agent.put_task()` | 平台 Router → WorkerRuntime，persona/tool_policy 由平台注入 |
| 重试/限流容错 | `login_qr()` 指数退避 | Python Poller 继承 GA Core 的容错；Go 侧重试在 `ilink.Client` |
| updates_buf 持久化 | `~/.wxbot/token.json` 里 | Go 加密存 `bot_transport_state.update_cursor_ciphertext` |

**核心原则：Go 拥有加密 + 持久化，Python Poller 拥有 iLink 协议 I/O。** Go 侧永不重新实现 iLink 协议。

### 7.2 架构（已实现）

```text
绑定链路：

┌─────────────┐   POST /v1/users/me/wechat-qrcode   ┌─────────────────┐
│  Web 前端    │ ──────────────────────────────────> │  Platform API   │
│ qrcode.react │ <─── qrcode_url + status ───────── │ (Go)            │
└─────────────┘                                      └────────┬────────┘
                                                              │ ilink.Client
                                                              ▼
                                                   ┌─────────────────────┐
                                                   │ wechat_qr_sessions  │
                                                   │ bots (per user)     │
                                                   └─────────────────────┘
                                                              │ 绑定确认
                                                              ▼
                                                   ┌─────────────────────┐
                                                   │ BotLifecycleService │ StartBotForBoundUser
                                                   │ (Go application)    │ → poller.Client.StartBot
                                                   └─────────────────────┘

消息链路：

┌──────────┐  getupdates 长轮询  ┌──────────────┐  POST /v1/im/webhook  ┌──────────────┐
│ iLink    │ <───────────────── │ Bot Poller   │ ────────────────────> │ Platform API │
│ 服务器    │ ── msgs[] ───────> │ (Python)     │ <── reply (send) ─── │ (Go)         │
└──────────┘                    │ WxBotClient  │                       └──────┬───────┘
                                └──────────────┘                              │
                                      ▲                                       │ poller.Client
                                      │ /start /stop /send /health            │ /send
                                      └───────────────────────────────────────┘

更新游标持久化（Go 拥有加密）：
  Poller 在 webhook body 里回传明文 updates_buf
  → BotLifecycleService.PersistUpdatesBuf 加密后写入 bot_transport_state
  → 重启时 RestoreActiveBots 解密 cursor 传回 Poller
```

### 7.3 实现状态（已完成）

**Go 控制面：**
- `internal/ilink/client.go`：QR 登录客户端，凭证字段按官方顶层格式解析。
- `internal/transport/ilink.go`：`ILinkAdapter` 委托 `poller.Client`，不再直接实现 iLink 协议。
- `internal/poller/client.go`：Python Bot Poller 的 HTTP 客户端（start/stop/send/health）。
- `internal/application/wechat_binding_service.go`：按用户创建 QR session、轮询、创建 bot。
- `internal/application/bot_lifecycle_service.go`：编排 StartBot/StopBot/RestoreActiveBots/PersistUpdatesBuf/HandleAuthExpired。
- `internal/postgres/bot_store.go`：per-user bot 持久化。
- `internal/postgres/bot_transport_store.go`：`bot_transport_state` 读写 + `ListActiveBoundBots`。
- `internal/api/im_webhook.go`：接收 updates_buf/auth_expired，持久化 cursor，路由消息。
- `internal/api/wechat_binding.go`：绑定确认后调 `StartBotForBoundUser`。
- `cmd/platform/main.go`：`--bot-poller-url`/`--platform-webhook-url` flag，组装 poller + lifecycle，启动时 RestoreActiveBots。
- 前端 `BindingPage.tsx`：`qrcode.react` 本地生成二维码。

**Python 数据面：**
- `tenant_platform/bot_poller/poller_server.py`：多租户长轮询服务，复用 `WxBotClient`，`start` 幂等。
- `frontends/wxbot_client.py`：从 GA Core 抽取的共享 iLink 协议客户端。
- `frontends/wxbot_media.py`：媒体下载共享模块。
- `frontends/wechatapp.py`：GA Core 单用户入口，改用共享模块消除重复。

### 7.4 待补齐（P0 后续）

1. ~~**Bot Poller**~~：✅ 已完成，Python Poller 复用 `WxBotClient`，Go 通过 HTTP 委托。
2. ~~**updates_buf 持久化**~~：✅ 已完成，`bot_transport_state.update_cursor_ciphertext` 加密存储。
3. **重试/限流**：`internal/ilink/client.go` 对 `GetQRCodeStatus` 和 `SendMessage` 的网络抖动/限流退避待补齐。
4. ~~**媒体收发**~~：✅ 已完成（2026-07-25）。4 个接入点已打通：
   - Python Poller `/send` 按 `msg_type`（text/image/video/file）分发到 `WxBotClient.send_text/send_image/send_video/send_file`
   - Python Poller `_dispatch` 调 `wxbot_media.download_media` 下载入站媒体（image/voice/file/video 全覆盖）
   - Go `poller.Client.SendMessageRequest` 新增 `MsgType` / `FilePath` 字段
   - Go `im_webhook.go` webhook body 新增 `media_paths` 字段，路由到 `IncomingMessage.MediaPaths`，并在 prompt 末尾追加 `[Attached files: ...]` 让 GA 的文件工具可读
   - 新增 `--media-dir` flag 控制是否下载入站媒体；空则禁用（保持纯文本模式）
5. **端到端验证**：需要 mock iLink 服务器 + Poller + 平台联调测试。
