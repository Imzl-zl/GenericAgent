# Epic: IM 多渠道会话架构（im-channel-session）

## 目标

按 `tenant_platform/docs/IM_CHANNEL_ARCHITECTURE.zh-CN.md` 定案的模型落地：

- **B 阶段（本 epic 主体）**：平台侧"对话单元分桶"——同 workspace 共享记忆/文件，对话上下文按（渠道 × 对话单元）隔离，互不串味。
- **A 阶段（epic 后半）**：单机封装层 `frontends/im/` IMAdapter 统一两代代码风格（独立改进线，概念对齐 B 的 source/conversation 语义）。

## 范围

- 契约：`worker.proto TaskEnvelope` 加 `conversation_id`（source 已有，复用）。
- worker：checkpoint/state 按桶存取（staging/commit 语义保留，向后兼容默认桶）。
- backend-go：入站消息提取 + 提交链路透传。
- 测试：契约绑定 + worker 分桶单测（两桶互不串）+ Go + 集成冒烟。
- 单机封装层 IMAdapter（Phase 2）。

## 非范围

- 不动 GA 核心（agentmain，Round17 黑盒约束）。
- 不做"对话对象级"分桶（群内按人再分）——v1 群一级，Coze 同款。
- 不改 workspace 隔离模型（personal/team 现状）。
