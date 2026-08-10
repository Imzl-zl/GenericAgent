# Epic: IM 渠道绑定（im-channel-binding）

## 目标

按 `tenant_platform/docs/IM_CHANNEL_BINDING.zh-CN.md` 定案落地：

- **统一渠道配置模型**：`bots` 表 RENAME 为 `channel_configs`（migration 0053），加 `channel_type`，唯一索引 (owner_id, channel_type)，`token_*` 列改名 `config_*`（凭据 JSON 密文），零数据迁移、FK 自动跟随。
- **API**：`GET/PUT/DELETE /v1/me/im-bindings`（user + admin 同款）；微信扫码流程保留；无连通性测试按钮（保存即生效 + 状态轮询）。
- **Poller**：`poller_server.py` 重构为 BotAdapter 注册表（channel_type → adapter），WeChat 迁移为 WeChatAdapter；新增 FeishuAdapter（lark-oapi WS）、DingTalkAdapter（dingtalk-stream）、QQAdapter（botpy WS）；配置热更新复用现有热推机制。
- **Router 入站**：body 加 channel_type + conversation_id；Source=ChannelType、ConversationKey=ConversationID，分桶自动生效；回复按渠道路由。
- **顺带清命名债（memory.md B3）**：契约字段 `ilink_user_id` → `channel_account_id`（openapi/poller/web 同批同步）。
- **Web**：菜单"微信绑定"→"渠道绑定"；BindingPage 渠道化（微信卡片扫码 + 飞书/钉钉/QQ 凭据表单卡片）。

## 范围

- migration 0053 + Go domain/store 改名 + API + OpenAPI + Router 扩展 + Poller adapter 化 + Web 渠道化 + 契约/单测/集成验证。
- 真实渠道冒烟需用户提供凭据（飞书/钉钉/QQ 应用）。

## 非范围

- 不动 GA 核心（agentmain，黑盒约束）。
- 不加企业微信 / Telegram 渠道（设计矩阵已列，本次只做飞书/钉钉/QQ）。
- 不做连通性测试按钮（已决：保存即生效 + 状态轮询）。
- 不做 poller 主动拉取配置（已决：复用现有热推机制）。
