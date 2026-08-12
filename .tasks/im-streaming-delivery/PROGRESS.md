# Progress

- Shape: epic
- FinalizationStatus: active
- Truth: .tasks/im-streaming-delivery/SUBTASKS.csv
- Parent: (root epic)
- Current: 任务 1-7 全部 DONE（除真实渠道冒烟待用户凭据）
- Latest validation: 全量 go test（18 包 ok，1 存量失败与本次无关）+ race 5 关键包 + 契约 23 + security/smoke 41（六服务 compose 存量失败）+ bot_poller 40 + web lint/build 全绿
- 残余风险:
  1. **QQ 被动回复 4 次/条限制是否计入流式帧未实证**：实现保守保留 `_MAX_STREAM_APPENDS=2`（open+append×2+commit=4 帧）；若官方流式接口（stream_messages）不计入普通消息回复次数可放宽；真实凭据冒烟验证
  2. **飞书编辑时限**（默认 24h）：超长任务自动退化为补发最终结果（stream_final_at 未置位 → delivery 兜底）
  3. botpy 版本差异：本地 botpy 无流式封装，走内部 `_http.request`（Route 与 api.py 同源）；官方已发布 qqbot-nodejs 1.0.4（腾讯第一方 Node SDK）原生封装 openStream/StreamSession，版本升级后可对照切换
  4. 真实渠道冒烟（飞书编辑链路 + QQ stream_messages 帧序列）待用户凭据
- Next: 用户提供渠道凭据后执行真实冒烟（重点=飞书编辑链路 + QQ stream_messages 帧序列）

# QQ 原生流式官方契约（2026-08-10 实证，任务 5 修正）

用户指出实现依据应以官方文档为准——重新调研后从第三方实现（linux.do 博客嵌套 `stream{}` 参数）**修正为腾讯官方契约**（证据链：bot.qq.com 消息收发概述“发送单聊消息/流式消息” + 群聊 autogen 页“群消息不支持流式参数” + 腾讯第一方开源 openclaw-qqbot + @tencent-connect/qqbot-nodejs 1.0.4 源码）：

- **端点**：`POST /v2/users/{openid}/stream_messages`（独立端点，不是 /messages 加参数）
- **请求体（扁平字段，对应 StreamReq proto）**：`input_mode=replace`（全量替换）、`input_state=1(生成中)/10(结束)`、`content_type=markdown`、`content_raw=全量文本`、`event_id/msg_id`（被动回复锚点，msg_id 必填——官方 SDK 无锚点直接拒绝）、`stream_msg_id`（首帧响应 id，后续帧携带）、`msg_seq`（**同一流所有帧共享**，仅 index 递增；生成规则 time^random % 65536）、`index`（从 0 起每帧递增）
- **限流**：429 / err_code 50002 指数退避；官方 SDK 节流 500ms（与 platform 侧一致）
- **关键差异（vs 初版实现）**：端点、扁平字段、msg_seq 共享、无 reset 字段、msg_id 必填

# 设计确认（2026-08-10，对照源码复核）

设计核心假设全部验证通过（scheduler_dispatch.go chunk 分支现状 / transport 层结构 / task.Source+ConversationKey / delivery 目标解析复用 / poller /send 扩展点 / FeishuAdapter lark-oapi 出站 / QQAdapter is_group / runtime settings 模式）；发现 3 个实施缺口（实现已决需求的必要前提，不改变设计决策）：

1. **缺口 A：群聊/私聊判定维度缺失**。入站契约只有 conversation_id 字符串无法判定群聊桶。补齐：webhook body + IncomingMessage + SubmitTaskCommand + tasks 表加 `conversation_type('private'|'group')`（migration 0054）；poller 各 adapter 现成信息（QQ is_group、飞书 chat_type、钉钉 conversation_type=='2'）；微信恒 private。
2. **缺口 B：成功路径最终交付语义**。completeSuccess 必写 outbox → delivery 必再发文本，与"流式消息即最终交付"冲突。补齐：tasks 加 `stream_final_at`（scheduler 流式 commit 成功置位），delivery 文本 part 前检查跳过（文件照发）；失败路径无标记 → delivery 照发兜底。
3. **缺口 C：scheduler 注入**。SchedulerConfig 加 `Streaming transport.StreamingSender`（nil=关）+ `Bots ChannelResolverByOwner` + RuntimeSettings 扩展 `GetIMStreamingMode`。

# 官方对齐复审（2026-08-10 第二轮，用户要求逐项对照官方）

第一轮（第三方实现）→ 修正为官方契约后，第二轮逐项对照官方文档/SDK 源码再审查，发现并修正 3 处偏差：

1. **飞书 20 次编辑上限缺失**（官方文档 Limitation of Use: 一条消息最多编辑 20 次）——新增 `_MAX_STREAM_EDITS=18`（append 每帧 PUT + commit 一次 PUT ≤ 20，留余量），超限后文本累积由 commit 最后一次更新送达；单测覆盖。
2. **QQ 限流重试缺失**（官方 SDK sendWithRetry: 429/50002 指数退避 1s/2s/4s，重试帧 index 递增避免 stale 冲突）——`_send_stream_frame` 加 2 次退避重试；单测覆盖（fake 响应序列 [429, 50002, 成功]）。
3. **QQ append cap 过于保守**（原 2 帧基于被动回复 4 次/条假设）——官方 SDK 无帧数限制（仅 500ms 节流），放宽为业务上限 60 帧（防超长任务刷单关系 20 QPM 频控），被动回复限制降级为残余风险待真实冒烟验证。

**复审符合项**（对照官方 SDK/文档逐项）：QQ 端点 `/v2/users/{openid}/stream_messages` ✓、扁平字段 input_mode/input_state/content_type/content_raw/event_id/msg_id/stream_msg_id/msg_seq/index ✓、msg_seq 同流共享仅 index 递增 ✓、msg_id 必填（无锚点拒绝）✓、update 全量替换语义 ✓、complete=DONE 帧 ✓、500ms 节流等价 ✓；飞书 PUT /im/v1/messages/:id + msg_type/content JSON 字符串 ✓、lark-oapi UpdateMessageRequest builder 链 ✓、1000/分 50/秒 频控（5 QPS 令牌桶更保守）✓。

**有意差异（已注释）**：abort 发 DONE 帧 + 中断提示（官方 cancel() 不发 DONE，消息停在生成中状态——我们保证消息闭合，更安全）。

# 架构审查结论（2026-08-10）

- **分层**：transport 可选接口（StreamingSender/StreamReply）→ application（StreamForwarder 编排）→ scheduler（dispatch 接入）→ delivery（stream_final_at 抑制）→ poller adapter（渠道 SDK），单向依赖无回环。
- **单一真值**：转发判定集中 `StreamForwarder.Enabled()`（mode + source + conversation_type 一处函数）；目标解析复用 delivery 同款 `channelTypeForTaskSource` + `GetChannelConfigByOwnerAndType`。
- **fail-closed**：settings 读失败/Streaming 未接线/群聊 → 不转发，终态 delivery 兜底；流式片段不写 messages 审计。
- **幂等与生命周期**：Commit/Abort closed 标志防重复；defer Abort 覆盖全部失败路径；短任务（无文本）不 open 直接交 delivery。
- **无定时器依赖**：scheduler 事件循环无定时器，500ms 节流 flush 由下一 chunk/心跳/Terminal 驱动（低频 chunk 场景心跳 FlushDue 防滞留）。
- **渠道适配收敛**：两种全量替换语义（飞书 PUT、QQ input_mode=replace）统一为 adapter 内累积文本；非流渠道基类默认抛错（明确失败而非静默）。
- **残余风险**：QQ 流式帧是否计入被动回复 4 次/条与单关系 20 QPM 未实证（cap 60 兜底）；飞书编辑时限（管理员配置，默认 24h）；真实渠道冒烟待凭据。

# 实施记录（2026-08-10）

- 任务 1: migration 0054（tasks.conversation_type + stream_final_at + platform_runtime_settings.text_value 列 + im_streaming_mode 默认 streaming）+ domain ConversationType/Normalize/校验 + router webhook + IncomingMessage + SubmitTaskCommand 全链路 + poller webhook_body conversation_type + store 读写 + 单测（migration 重放幂等/round trip/MarkTaskStreamFinal）
- 任务 2: transport `StreamingSender`/`StreamReply` 接口（streaming.go）+ LoopbackTransport 实现（SentStreams 断言）+ `StreamForwarder`（stream_forwarder.go：500ms 节流合并缓冲 streamBatcher + open/append/commit/abort 编排 + Enabled 判定矩阵 fail-closed）+ domain IMStreamingMode + store Get/UpdateIMStreamingMode + 单测 8 个（含节流合并 bug 修复：首条 chunk 为窗口起点）
- 任务 3: scheduler_dispatch.go 接入（非空 chunk→AppendText、心跳→FlushDue、SUCCEEDED→Commit+MarkTaskStreamFinal、defer Abort 兜底失败路径）+ delivery 文本 part 跳过（StreamFinalAt）+ main.go 装配（Streaming 类型断言 + Bots=store）+ 集成测试 3 个（commit 全链路/失败 abort/群聊收敛）+ delivery 测试 3 个
- 任务 4: poller /send 扩展 stream_id+stream_action（manager.send_stream + handler 分支，open 响应回 stream_id）+ FeishuAdapter 消息编辑（占位 CreateMessage→message_id→PUT UpdateMessage 全量替换，累积语义）+ _TokenBucket 5 QPS 节流 + ILinkAdapter StreamingSender（poller.StreamAction）+ 单测（Go 3 + Python fake lark 6）
- 任务 5: QQAdapter 原生流式（单聊）：官方实证（linux.do 2026-03 + easybot SDK）→ stream{state,id,index,reset} 帧序列 + msg_id 被动锚点 + _MAX_STREAM_APPENDS=2 被动回复保护 + 群聊拒绝（基类默认抛错）+ 单测 5（fake botpy _http + 后台 asyncio loop）
- 任务 6: GET/PUT /v1/admin/settings/im-streaming + OpenAPI（文本插入，契约 23 全绿）+ Web SettingsPage 下拉（streaming/final_only/off）+ api client/types + 单测
- 任务 7: 集成验证全绿（除存量失败：Windows fakeserver、security 六服务 compose、worker test_worker_rpc_smoke 断言漂移、integration 2 个——与本次无关，base commit 可复现）
- 设计真值: `tenant_platform/docs/IM_STREAMING_DELIVERY.zh-CN.md`
