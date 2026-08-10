# IM 流式输出（IM Streaming Delivery）

> 状态：设计稿（2026-08-10，渠道能力矩阵为官方文档实证，待用户确认后实施）
> 关联：`IM_CHANNEL_ARCHITECTURE.zh-CN.md`（会话模型）、`IM_CHANNEL_BINDING.zh-CN.md`（渠道绑定/Adapter 注册表）
> 结论先行：**管道已通（worker→platform Chunk 事件流），缺 platform→IM 转发；流式实现方式按渠道分档（飞书=消息编辑、QQ 单聊=原生流式接口、钉钉=限速分片、微信=非流），不是统一代码路径。**

## 1. 背景与现状

- worker→platform 的 gRPC `ExecuteTask` 已是流式事件通道：`WorkerEvent{Chunk, ToolProgress, Terminal}`（`contracts/proto/genericagent/worker/v1/worker.proto`）。
- platform 侧 `scheduler_dispatch.go` 消费 Chunk **只用于两件事**：空文本 chunk 当心跳（刷新 `last_activity_at` 防 idle 误杀）；非空 chunk 累计进 `chunkBatch` 结果统计。
- **Chunk 没有任何转发到 IM 的逻辑**。用户实时反馈只有两条：提交时 `task_started`（✓ 收到）、终态 delivery（结果）。
- 用户感知差距：AI 生成中的"边想边答"过程不可见。

## 2. 渠道能力矩阵（官方文档实证，2026-08-10）

| 渠道 | 频控（官方文档） | 流式能力 | 结论 |
|---|---|---|---|
| 飞书 | 同用户/同群 **5 QPS**；接口 1000 次/分 & 50 次/秒；限流 429 + `x-ogw-ratelimit-reset` | ✅ **消息编辑**：`PUT /im/v1/messages/:message_id`（text/post，接口同 1000/分） | **真打字机**：发一条后持续 PUT 更新 |
| QQ 单聊 | 主动：Bot 10 QPS、单关系 20 QPM、日 1000 条/用户；**被动：每条用户消息 60 分钟内限回 4 次** | ✅ **原生"流式消息"接口**（官方文档"发送单聊消息/流式消息"；群消息文档明确"不支持流式参数"） | **原生流式**，无需拼分片 |
| QQ 群聊 | 被动：每条消息 5 分钟内限回 5 次；主动 60 QPM、日 1000 条/群 | ❌ 不支持流式参数 | 只能最终结果 |
| 钉钉 | 自定义机器人**每分钟 20 条**，超限流 10 分钟；企业机器人 `send.too.fast`；标准版组织**每月 1 万次**服务端 API 总量 | ❌ 无编辑接口（可撤回） | 分片可行但必须克制（≤20 条/分）；建议只发最终结果 |
| 微信 iLink | 官方网关无公开文档（内测），已知单条 3000 字符限制 | ❌ 无 | 非流（现状） |

参考：飞书 https://open.feishu.cn/document/server-docs/im-v1/message/create 、https://open.feishu.cn/document/server-docs/im-v1/message/update ；QQ https://bot.qq.com/wiki/develop/api-v2/server-inter/message/overview.html 、发送群聊消息文档；钉钉 https://open.dingtalk.com/document/orgapp/invocation-frequency-limit.md 、https://open.dingtalk.com/document/orgapp/the-robot-sends-ordinary-messages-in-a-person-to-person-conversation.md

**推论**：
1. "流式"在 IM 语境 = 三种形态：消息编辑（飞书）、原生流式接口（QQ 单聊）、限速分片（钉钉）。
2. 分片式（多条普通消息）受频控硬约束：QQ 被动回复 4 次/条、钉钉 20 条/分（超了整 bot 哑火 10 分钟）——**不能无脑按 chunk 发**。
3. 微信生态（公众号/客服消息）惯例即"收到 + 稍后结果"，非流式是行业常态而非缺陷。

## 3. 设计决策（待用户确认）

- [ ] 决策 1：**按渠道分档实现**（飞书=编辑消息打字机 / QQ 单聊=原生流式接口 / 钉钉=只发最终结果，v1 不做钉钉分片 / 微信=现状）。群聊统一只发最终结果（收敛策略，防刷屏 + 规避 QQ 群被动回复次数限制）。
- [ ] 决策 2：**转发默认开关**：私聊默认开（自用主场景，IM_CHANNEL_BINDING 画像），群聊默认关（只发最终结果）；`platform_runtime_settings` 增加管理开关（`im_streaming_mode: off | final_only | streaming`）。
- [ ] 决策 3：**粒度与节流**：platform 侧按渠道频控表做客户端节流（飞书 ≤2 QPS 安全余量；钉钉不启用分片故无此虑；QQ 单聊 ≤5 QPM 被动余量）；chunk 合并窗口 ~500ms。
- [ ] 决策 4：**失败语义**：IM 消息不可撤回——流式中途失败（worker 崩溃/限流）时补发最终结果（走既有 delivery 路径）；流式片段不写 messages 审计（只记最终结果，与现 delivery 一致）。
- [ ] 决策 5：**ToolProgress 事件**：v1 不转发（工具进度提示信息价值低，先不做）。

## 4. 架构改动

### 4.1 worker→platform：零改动
`Chunk` 事件流已就绪（含 turn 轮次），直接消费。

### 4.2 platform：scheduler → 转发管道
`scheduler_dispatch.go` chunk 分支：非空 chunk 追加到 per-task 转发缓冲（节流合并 500ms），按 task 解析回复目标（`channel_configs` 按 task.Source 解析 + `task.ConversationKey`），调 transport 流式接口；终态（Terminal）时 commit。

新接口（transport 层，可选实现）：
```go
// StreamingSender 是可选接口: 支持流式回复的渠道实现它。
type StreamingSender interface {
    // BeginReply 开启一条流式回复; 返回流句柄。
    BeginReply(ctx context.Context, botUUID, target, clientID string) (StreamReply, error)
}
type StreamReply interface {
    Append(ctx context.Context, text string) error
    Commit(ctx context.Context) error          // 终态收尾(QQ 原生流式结束/飞书最后一次 PUT)
    Abort(ctx context.Context) error           // 失败弃流(飞书可改"生成中断"提示)
}
```
- LoopbackTransport：记录日志（测试断言用）。
- ILinkAdapter（微信）：不实现——非流渠道保持 SendMessage。
- Poller client：`/send` 扩展 `stream_id` + `stream_action(open|append|commit|abort)`（复用既有 /send 通道，不新增 HTTP 端点）。

### 4.3 poller adapter（各渠道一个方法）
| Adapter | 实现 |
|---|---|
| FeishuAdapter | `send_stream_open` 发空/占位文本消息取 `message_id` → append 走 `PUT /im/v1/messages/:id`（lark-oapi `im.v1.message.update`）→ commit 最后一次更新 |
| QQAdapter | 单聊用官方**流式消息接口**（botpy 对应 API，实测确认参数）；群聊不实现 |
| DingTalkAdapter | 不实现（v1 只发最终结果，规避频控） |
| WeChatAdapter | 不实现（现状） |

### 4.4 群聊收敛
Router/delivery 已有渠道上下文（Source + ConversationID + channel_configs），转发判定在 platform：`task.Source` 为飞书/QQ 单聊且私聊桶 → 流式；群聊桶（或钉钉/微信）→ 只走既有终态 delivery。

## 5. 实施拆分（epic: im-streaming-delivery）

| # | 任务 | 要点 |
|---|---|---|
| 1 | transport 接口 + loopback 实现 + 单测 | `StreamingSender`/`StreamReply`；scheduler 转发管道骨架（节流缓冲） |
| 2 | scheduler chunk 转发接入 + 终态 commit/abort | 转发目标解析（config + conversation）；失败补发最终结果；群聊收敛开关 |
| 3 | poller：/send 扩展 stream_* + FeishuAdapter 编辑消息 | lark-oapi `message.update`；5 QPS 节流；单测（fake SDK） |
| 4 | QQAdapter 原生流式消息（单聊） | botpy 流式 API 实测参数；单测；群聊拒绝 |
| 5 | runtime settings 开关 + Web 设置项 | `im_streaming_mode` 管理配置 + web 下拉 |
| 6 | 集成验证 | 契约 + Go + poller + web 全绿；真实渠道冒烟（需用户凭据，重点验证飞书编辑链路与 QQ 流式接口参数） |

## 6. 残余风险

- **QQ 流式消息接口的具体参数**：官方文档仅列出入口，请求字段需真实凭据实测（任务 4 前置验证）。
- **飞书编辑时限**：消息可编辑时间由企业管理员设置（默认 24h 内），任务时长内无虞；超长任务（>编辑时限）自动退化为补发最终结果。
- **iLink 微信频控未知**：非流式不受影响。
- 钉钉每月 1 万次 API 总量：v1 不做钉钉分片即不消耗。
