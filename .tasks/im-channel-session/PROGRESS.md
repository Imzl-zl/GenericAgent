# Progress

- Shape: epic
- FinalizationStatus: active
- Truth: .tasks/im-channel-session/SUBTASKS.csv
- Parent: (root epic)
- Current: 设计定案已落盘（IM_CHANNEL_ARCHITECTURE.zh-CN.md）；契约设计稿待细化，任务 1（契约字段）未开工
- Latest validation: 无（未开工）
- 残余风险: 平台侧"新会话"命令语义需随分桶调整；真实多渠道冒烟需可用渠道凭据
- Next: 任务 1 契约字段 → 任务 2 worker 分桶 → 任务 3 Go 透传 → 任务 4 测试冒烟 → 任务 5 新会话语义 → Phase 2 单机封装

# 设计定案（2026-08-10）

- 设计真值: `tenant_platform/docs/IM_CHANNEL_ARCHITECTURE.zh-CN.md`；契约细节: `tasks/conversation-key/DESIGN.md`
- 业务模型（用户拍板 + Coze 业界印证）: 隔离单元 = workspace（personal/team），memory/SOP/文件全共享；**对话上下文按"对话单元"分桶**；`/new` 清当前桶；团队=租户；微信个人自用固定单桶
- 渠道能力矩阵（代码实证）: 微信仅私聊单桶（wxbot_client 无群概念）；QQ/TG/钉钉/企微/飞书可群聊（群=一桶，私聊=一桶）
- 关键实现事实: `TaskEnvelope.source` 已存在且已透传 GA `put_task(source=...)`（worker task_drain.py:60）——**复用 source 作渠道维度，不新增 channel 字段**；缺的是 `conversation_id`（对话单元对端/群 ID）

# 待确认方向（2026-08-10）

- [已消解] Web console 对话：用户确认**不存在 web 页面对话**——任务来源只有 IM 渠道；非渠道入口（sessions API 等）归 default 桶
- [已定] 存量兼容: conversation_id 空 → 该渠道 default 桶，与现单桶行为一致，存量数据零迁移
- [已定] 桶 key 归一化: `f"{source}:{conversation_id or 'default'}"`，规则放 worker 一处，Go 透传原始值
- [已定] 平台侧"新会话"命令 v1 语义: 清当前桶（无渠道上下文时=default 桶），文档化，待 web 支持渠道上下文后细化
