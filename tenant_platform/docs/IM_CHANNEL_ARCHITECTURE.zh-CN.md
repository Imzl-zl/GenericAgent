# IM 多渠道架构（IM Channel Architecture）

> 状态：设计定案（2026-08-10）。渠道能力矩阵为代码实证；数据模型与业务决策已由用户拍板并对照业界（Coze）印证。
> 关联：`GA_SANDBOX_RUNNER_REFACTOR.zh-CN.md`（workspace/Runner 生命周期）、`bot-poller-auth.md`、`docs/SETUP_FEISHU.md`。

## 1. 背景与核心决策

微信（iLink 官方网关）已对接完成，需要扩展飞书/QQ/企业微信/钉钉/Telegram 等渠道。本文件定义多 IM 接入的架构约束，避免每个渠道一套实现、会话串味。

**核心业务模型（用户拍板）：**

1. **数据隔离单元 = workspace**（`personal:<uid>` / `team:<uuid>`，与平台现状一致）：`memory/`（L0-L4）、SOP、项目文件、temp、delivery 产物**全共享**——"都是同一个人/同一个团队，不区分"。
2. **团队 = 一个租户**：`team:` scope 就是 workspace，团队多人共享同一份工作空间与对话（按对话单元见下）。
3. **对话上下文按"对话单元"分桶隔离**：每个对话单元（私聊对端 / 群）拥有独立 history，互不串味。
4. **`/new` 只清当前对话单元的桶**：不清其他单元，更不删 workspace 记忆。
5. **GA 核心（agentmain）零改动**：history 注入/取出在 worker 层，GA 原生就是"给一份 history，跑完还你新的"。

**业界对照（2026-08-10 调研，三家主流水准）：**

- **Coze**：多会话核心特性 = "文件共享，上下文隔离"；会话隔离层级 = `渠道 + 账号 + Bot + session_name`；群 = 独立对话单元；工作空间不分群/个人。
- **OpenClaw（开源个人 AI，即"claw"）**：会话键 = `agent::<id>::dm:<peer>`（私聊）/ `agent::<id>::group:<群ID>`（**每个群独立会话**）/ `channel:<房间>`；群组与主私信共享同一智能体工作区、仅会话键不同（"个人和公共绝不混合"才用第二个智能体）；**`/new` 是主流重置命令，按会话键重置**（`resetByType`/`resetByChannel` 可配——重置是会话级的，不是全局的）；Telegram 论坛主题按 `:topic:` 再细分会话。
- **LangBot（国内主流）**：`session_id` 私聊 = `person_<id>`、群聊 = `group_<id>`——会话键就是对话单元；`conversation_id` 重置后重新生成（重置 = 新会话）。

**结论：本项目模型（workspace 记忆共享 + 每群/每私聊一桶 + /new 清当前桶）与主流完全一致，桶粒度 = 对话单元（群/单聊）即为业界共识，无更细维度（群内按人分是可选策略，v1 不做，与 Coze/OpenClaw 默认一致）。**

## 2. 渠道能力矩阵（代码实证）

| 渠道 | 实现文件 | 接入协议 | 群聊 | 对话单元形态 |
|---|---|---|---|---|
| 微信 | `frontends/wechatapp.py` + `wxbot_client.py` | iLink 官方网关（ilinkai.weixin.qq.com）长轮询 | ❌ **仅私聊** | **个人自用单桶**（`wechat:me`）——bot 绑定用户自己的微信号，无"多个好友"客服场景 |
| Telegram | `frontends/tgapp.py` | python-telegram-bot polling | ✅ | 每群一桶 + 每私聊一桶 |
| QQ | `frontends/qqapp.py` | qq-botpy | ✅ | `group_openid` 群桶 / `openid` C2C 桶 |
| 钉钉 | `frontends/dingtalkapp.py` | dingtalk-stream | ✅ | `conversation_type=="2"` → `group:{conversation_id}` 群桶；否则 sender_id 私聊桶 |
| 企业微信 | `frontends/wecomapp.py` | wecom_aibot_sdk WS | ✅ | `chatid` 群桶 / sender_id 私聊桶 |
| 飞书 | `frontends/fsapp.py` | lark-oapi | ✅ | 群桶 / 私聊桶 |

要点：

- **微信是唯一"个人自用型"渠道**：单桶固定，不存在多对话对象。平台侧微信 bot ↔ 用户 1:1（QR 扫码绑定）。
- 其余渠道为"服务型"：对话单元 = 渠道 × 群/对端 ID。
- 设计上不写死"渠道→多桶"：**桶 key 由消息的对话标识（chat_id）决定**，微信 chat_id 恒为"自己"所以天然单桶，无需特判。

## 3. 数据模型

```
workspace（personal:<uid> / team:<uuid>）── 共享：memory/、SOP、项目文件、temp、delivery
└── 对话单元 conversation = <channel>:<chat_id> ── 隔离：history 每单元一份
    ├── 微信（个人自用）        → 固定单桶 wechat:me
    ├── 飞书/QQ/企微/钉钉/TG    → 每个群一桶 + 每个私聊一桶
    └── /new → 清当前对话单元桶（不删 workspace 记忆）
```

映射关系：

- **session_key == workspace_key**（1:1，现状不变，`domain/team.go`：`PersonalSessionKey` / `team:<uuid>`）。
- **TaskEnvelope 增加对话标识维度**：`channel`（渠道类型）+ `conversation_id`（chat_id），worker 据此选桶。
- **checkpoint/state 按对话单元分桶**：`backend_history`/`agent_history`/`display_history` 三视图同桶存取；**per-conversation 同样走 staging/commit**（只有成功 task 才推进该桶恢复指针），失败/取消不污染。
- **memory L0-L4 / SOP 保持 workspace 级共享**，写穿语义不变（与 `GA_SANDBOX_RUNNER_REFACTOR` §2 一致）。
- **群桶成员**：群内多人共享该群桶（团队=租户，不按人再分）；私聊各自一桶。
- **`/new` 桶级实现（0052 已落地）**：reset 标记在 `conversation_resets(workspace_id, conversation_key)` 表，按桶 upsert；/new 只取消该桶 queued 任务并标记该桶；消费语义不变（该桶 fresh 任务成功终态才清除，失败/取消不消费，R4-I8）；旧 `workspaces.reset_at` 列已随 0052 退役。微信 /new 只清 `''` 默认桶。

## 4. 落地路径（平台侧）

1. **契约**：`worker.proto` `TaskEnvelope` 加 `channel` / `conversation_id` 字段 → 生成 Go `gen` + worker-python（跨语言契约，同步生成，跑契约绑定测试）。
2. **worker-python**：`checkpoint.py` / `state.py` / `task_runner.py` 桶 key 化——任务开始按 `(channel, conversation_id)` 恢复该桶 history，结束写回该桶（staging/commit 语义保留）。
3. **backend-go**：`/v1/router/messages` 透传对话标识；session 校验不变（workspace 级）。
4. **测试**：契约绑定测试 + worker 分桶单测（两桶互不串）+ 冒烟（真实多渠道各发一条，验证互不串味）。
5. **渠道接入**：新渠道 = poller/adapter 实现（复用 `WxBotClient` 模式的渠道客户端，抽 `BotAdapter` 接口）+ 绑定类型注册（`ChannelBinding` 已有通用模型：`ChannelType + ChannelAccountID → canonical_user`）。

## 5. 单机封装层（另一条独立改进线）

`frontends/*app.py` 的代码整洁债与平台侧模型无关，单独演进：

- 两代风格（`AgentChatMixin` 继承式 vs `wechatapp`/`tgapp` 回调式/流式特化）统一为 `IMAdapter` ABC（模板方法）：子类只实现协议差异点（`extract_text` / `send_text` / `send_media` / `format_markdown` / `is_authorized`），基类提供 `handle_message` / `handle_command` / `run_agent` / `run_app`。
- 概念对齐：**平台侧 `channel` == 单机层 `source`**（`put_task(source=...)`）。
- 单机层不做租户隔离（一进程一 agent 即单租户语义正确）。

## 6. 未决项 / 残余风险

- **对话对象级分桶**（群内按人再分）：v1 不做（Coze 同为群一级；团队=租户模型下无此需求）。
- **微信渠道无群**：不存在群桶，平台侧绑定仍 1:1（bot ↔ user ↔ workspace）。
- **渠道渲染差异**（微信不支持 md 图片、长度限制 3000 字符等）：平台 delivery 层已有 `cleanIMMarkdown` 全渠道降级兜底，新增渠道按需扩展。
- 单机封装层 IMAdapter 迁移（Phase 1-4）未排期，不影响平台侧落地。
