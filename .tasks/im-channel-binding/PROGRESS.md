# Progress

- Shape: epic
- FinalizationStatus: active
- Truth: .tasks/im-channel-binding/SUBTASKS.csv
- Parent: (root epic)
- Current: 任务 1-6 全部 DONE（除真实渠道冒烟待用户凭据）
- Latest validation: 全量 go test + race 5 关键包 + 契约 23 + security/smoke 18 + bot_poller 29 + worker 114 + web lint/build 全绿
- 残余风险: 真实多渠道冒烟需用户提供凭据（飞书/钉钉/QQ 应用）；3 个新 adapter 的 SDK 链路只经单测覆盖（SDK 事件结构 fake 对象），真实连接未验证
- Next: 用户提供渠道凭据后执行真实冒烟

# 实施记录（2026-08-10）

- 任务 1: migration 0053（bots→channel_configs RENAME + channel_type + (owner_id,channel_type) 唯一索引 + token_*→config_* + state 加 disabled + **0003 marker 存续 stub bots 表**——RENAME 后 to_regclass('bots') 为空会让 0003 被重放，这是本次迁移最隐蔽的坑）+ Go domain.Bot→ChannelConfig 全库改名 + store 重写 + 单测（CRUD/FK 跟随/重放幂等）
- 任务 2: GET/PUT/DELETE /v1/me/im-bindings + admin 同款 6 路由；OpenAPI 同步（test_route_contract 门禁过）；ilink_user_id→channel_account_id 契约字段全链路（router/webhook/poller client/openapi/web）；API 功能测试 5 个
- 任务 3: Router 入站扩展——Source=ChannelType、ConversationKey=ConversationID、微信身份校验/自动绑定仅限 wechat、新渠道属主直判、bot_uuid↔channel_type 一致性 fail-closed、/new /stop /status 按当前桶、delivery 回复目标=conversation_id（微信=ilink_user_id）；多渠道单测 4 个
- 任务 4: poller_server.py 重构为 BotAdapter 注册表（WeChatAdapter 迁移 + FeishuAdapter lark-oapi WS + DingTalkAdapter dingtalk-stream + QQAdapter botpy WS，SDK 惰性导入）；/start 契约改为 channel_type+config_json；修复持锁构造死锁（coalesce provider 回调非重入锁）；单测 29 个
- 任务 5: Web 菜单"渠道绑定"×2 + BindingPage 渠道化（微信卡片扫码保留 + 飞书/钉钉/QQ 凭据表单卡片 + 状态徽章 + 保存/解绑 + 帮助文案）+ api client/types 同步；lint/build 全绿
- 任务 6: 集成验证全绿（除存量失败，见 SUBTASKS notes）
- 设计真值: `tenant_platform/docs/IM_CHANNEL_BINDING.zh-CN.md`
