# IM 渠道绑定（IM Channel Binding）

> 状态：设计稿（2026-08-10，待用户确认后实施）
> 关联：`IM_CHANNEL_ARCHITECTURE.zh-CN.md`（会话模型：workspace 共享 + 对话单元分桶，已落地）、`bot-poller-auth.md`
> 目标：接入飞书/钉钉/QQ 渠道；Web 菜单"微信绑定"改为通用渠道绑定页，可配置各渠道。

## 1. 现状与差距

| 项 | 现状 | 差距 |
|---|---|---|
| 渠道配置 | `bots` 表（owner_id + ilink_user_id + token 密文），微信专用命名 | 表/字段/API 全微信化，需泛化为统一渠道模型 |
| 入站路由 | `/v1/router/messages` 字段 `bot_uuid + ilink_user_id`（微信专用名） | 需 channel_type + 对话单元维度（conversation_id 契约已备好）；ilink_user_id 是命名债（memory.md B3，现在就是契约变更窗口） |
| poller | `bot_poller/poller_server.py` WeChat 专用（每 bot 一线程 WxBotClient） | 非 adapter 化 |
| Web | 菜单"微信绑定"，BindingPage 微信扫码专用 | 无渠道列表/凭据表单 |
| 身份模型 | `ChannelBinding`（channel account → canonical user）通用模型已存在 | 仅微信在用 |

## 2. 渠道绑定形态矩阵（决定 UI 与配置模型）

**群消息触发方式 = 渠道平台协议规则（官方文档实证，2026-08-10），不是我们可选的**：

| 渠道 | 绑定形态 | 凭据 | 接收模式 | 单聊 | 群聊触发（平台规则） | 群聊收全部消息 |
|---|---|---|---|---|---|---|
| 微信 iLink | 扫码授权（保留现有流程） | 无表单（扫码产物存 configs） | iLink 长轮询（现有） | 直接收 | 无群 | - |
| 飞书 | 配置企业自建应用 | `app_id + app_secret` | lark-oapi WebSocket 长连接 | 直接收（需 p2p 权限） | **权限决定**：`group_at_msg`=仅@消息（默认），`group_msg`=全部消息（敏感权限） | 可（敏感权限，我们不申请） |
| 钉钉 | 配置开放平台应用 | `app_key + app_secret` | dingtalk-stream 长连接 | 直接收 | **必须 @（硬规则）**："群聊中只有 AT 机器人的消息可以被机器人接收到" | 不可 |
| QQ | 配置开放平台机器人 | `app_id + app_secret` | botpy WebSocket 长连接 | 直接收（C2C） | **必须 @（硬规则）**：GROUP_AT_MESSAGE_CREATE，"API 仅投递@提及了机器人的群消息" | 不可 |
| 企业微信 | 配置智能机器人 | BotID + Secret | 长连接 | 直接收 | **必须 @（硬规则）** | 不可（普通应用"暂不支持接收群聊消息"） |
| Telegram（备） | BotFather token | token | polling | 直接收 | **@/命令（默认）**，可关 privacy mode 收全部 | 可（bot 端配置） |

**结论**：adapter 只按平台规则订阅事件（钉钉/QQ/企微天然只收到 @ 消息，飞书申请 `group_at_msg` 权限）；**我们不申请"收全部群消息"（飞书敏感权限/TG privacy 关闭）——业务只需要 @ 触发**，与主流一致。

## 3. 数据模型：`bots` 表改造为统一渠道配置表（migration 0053）

**设计定案：不建新表、不留两表并存**——`bots` 表语义泛化为"用户渠道连接配置"，RENAME 为 `channel_configs`（Postgres RENAME TABLE 后 `messages`/`bot_transport_state`/`context_tokens` 的外键**自动跟随**，零 FK 风险；存量微信行 `channel_type='wechat'` 默认值，**零数据迁移**）。

```sql
ALTER TABLE bots RENAME TO channel_configs;               -- FK 自动跟随
ALTER TABLE channel_configs
    ADD COLUMN channel_type TEXT NOT NULL DEFAULT 'wechat';
ALTER TABLE channel_configs RENAME COLUMN token_ciphertext TO config_ciphertext;
ALTER TABLE channel_configs RENAME COLUMN token_key_version TO config_key_version;

-- 每用户每渠道一行(原 bots_owner_uq 仅允许一行/用户)
DROP INDEX bots_owner_uq;
CREATE UNIQUE INDEX channel_configs_owner_type_uq
    ON channel_configs (owner_id, channel_type);

-- ilink_user_id 保留为微信专用列(新渠道 NULL; 账号标识在 config JSON 内)
-- state 语义泛化: active | disabled(解绑) | expired | revoked(微信既有)
```

- 凭据：`config_ciphertext` 存 JSON 密文——微信=`{token}`，飞书/钉钉/QQ=`{app_id, app_secret}`；复用现有 cipher + key version 机制。
- **顺带清 memory.md B3 命名债**：API 契约字段 `ilink_user_id` → `channel_account_id`（新渠道没有 ilink 概念，字段必须泛化；openapi/poller/web 同批同步）。

## 4. API 设计（userAuth + admin 同款路径）

```
GET    /v1/me/im-bindings                        → 各渠道状态列表
         [{channel_type, state, bound_at, meta: {app_id 脱敏}}...] + 微信 bot 状态
PUT    /v1/me/im-bindings/{channel_type}         → 保存/更新凭据（feishu|dingtalk|qq）
         body: {app_id, app_secret}；加密入库，state=active；触发 poller 重载
DELETE /v1/me/im-bindings/{channel_type}         → 解绑：state=disabled；通知 poller 断开
POST   /v1/me/im-bindings/{channel_type}/test    → 连通性测试（可选，v1 可省）
```

微信：现有扫码 API 不动（页面内嵌）。Admin 侧 `/v1/admin/me/...` 同款。
**OpenAPI 契约同步**（test_route_contract 门禁：新路由必须进 spec）。

## 5. Poller 扩展：BotAdapter 注册表

`poller_server.py` 从微信专用重构为注册表，配置源 = `channel_configs`（含微信）：

```python
adapters: dict[str, BotAdapter]  # channel_type → 连接线程工厂
# WeChatAdapter = 现有 WxBotClient 线程模型(逻辑不变, 读 channel_configs)
# FeishuAdapter  = lark-oapi ws client
# DingTalkAdapter= dingtalk-stream
# QQAdapter      = botpy ws
```

- 配置来源：现有 bot 列表轮询接口扩展为"活跃 channel_configs"（poller 定期拉取，热更新：新增/更新/解绑触发连接重载——复用现有配置热推机制）。
- 入站统一 POST `/v1/router/messages`，body 扩展（**契约变更：`ilink_user_id` → `channel_account_id`**，poller 与 platform 同批发布）：

```json
{
  "bot_uuid": "渠道连接实例 UUID",
  "channel_type": "feishu | dingtalk | qq | wechat",
  "channel_account_id": "渠道侧账号标识(微信=ilink_user_id, 其他=应用账号)",
  "conversation_id": "对话单元 ID(群 ID / 对端 ID; 微信恒空)",
  "message_id": "...",
  "text": "..."
}
```

- 回复：`/send` 接口加 `channel_type` 分发到对应 adapter 的 `send_text/send_media`（BotAdapter 抽象：现有 WxBotClient 即 WeChat 实现）。

## 6. Router 扩展

- `IncomingMessage` 加 `ChannelType` + `ConversationID`；`routerMessageBody` 解析同字段。
- 构建 `SubmitTaskCommand` 时：`Source=ChannelType`（新渠道），`ConversationKey=ConversationID`（微信空）——分桶模型直接生效（每群/每单聊独立上下文，架构文档 §3）。
- 渠道账号解析：`ChannelBinding`（channel_type + channel_account_id → canonical user）或新渠道首次消息自动绑定 owner（channel_configs 的 owner 即 canonical user——新渠道直接按配置归属，不需要额外绑定步骤）。
- 回复 transport：`BotTransportAdapter` 按 channel_type 路由（微信现有 iLink 实现保留）。

## 7. Web UI 改造

- **菜单改名**：`AppLayout`/`AdminLayout` "微信绑定" → **"渠道绑定"**（推荐，与实现术语一致；备选"IM 绑定"——你拍板）。
- `BindingPage` 重构为渠道列表：
  - 微信卡片：现有扫码流程原样嵌入。
  - 飞书/钉钉/QQ 卡片：状态徽章（未配置/已启用）+ 凭据表单（app_id / app_secret）+ 保存/解绑按钮 + 帮助文案（如何创建应用）。
- 路由 `/app/binding` 不变。

## 8. 安全

- 凭据 `cipher.Encrypt` 入库（复用现有加密器与 key version 机制），API 响应仅回脱敏 `app_id`/`bot_id`。
- 所有新 API `userAuth` 鉴权（owner 自操作）；OpenAPI 同步；`test_no_real_key_leak` 覆盖（poller 环境不得泄漏凭据明文）。

## 9. 实施拆分（epic: im-channel-binding）

| # | 任务 | 要点 |
|---|---|---|
| 1 | migration 0053：bots → channel_configs（RENAME + channel_type + 唯一索引 + 列改名） | 零 FK 风险；存量微信行默认值；Go domain/store 改名同步（domain.Bot → ChannelConfig） |
| 2 | API：GET/PUT/DELETE `/v1/me/im-bindings`（user + admin）+ OpenAPI；微信扫码落库到 channel_configs | 契约测试；ilink_user_id 契约字段 → channel_account_id（openapi 同步） |
| 3 | Router 入站扩展：channel_type + conversation_id + Source/ConversationKey 映射 + 回复 transport 按渠道路由 | 微信链路回归（bot_uuid/channel_account_id 改名同步 poller） |
| 4 | Poller BotAdapter 注册表 + WeChat 迁移 + Feishu/DingTalk/QQ adapter（Python） | 每 adapter 单测 + 配置热更新 + bot_poller 测试套件 |
| 5 | Web：菜单改名"渠道绑定" + BindingPage 渠道化 + api client/types 同步 | lint/build + 浏览器冒烟 |
| 6 | 集成验证：契约 + Go + poller + web；真实渠道冒烟（需各渠道凭据） | 用户提供凭据后执行 |

## 10. 已决/未决

- [已决] 菜单名：**渠道绑定**（AppLayout/AdminLayout）
- [已决] 无连通性测试按钮（业界同款：保存即生效 + 状态轮询）
- [已决] 渠道配置**用户级**（每用户配置自己的渠道应用——与微信多租户模型一致，平台级共享不存在）
- [已决] 微信迁入统一模型（不留两表并存债；RENAME 零数据迁移）
- [已决] 顺带清 ilink_user_id 命名债（契约变更窗口）
- [已决] 群消息触发方式 = **渠道平台协议规则**（矩阵见 §2）：钉钉/QQ/企微硬性 @（平台只推 @ 消息）；飞书由权限决定（申请 `group_at_msg`，不申请收全部的敏感权限）；TG 默认 @/命令——adapter 按平台规则订阅，无需我们选择
- [已决] conversation_id 取值（对话单元粒度已由 IM_CHANNEL_ARCHITECTURE 定案）：QQ=`group_openid`（群）/`openid`（C2C）、钉钉=`conversationId`（群/单聊统一）、飞书=`chat_id`（p2p/group）、微信恒空（单桶）——各渠道 SDK 事件内现成字段，adapter 透传即可，无设计未决
