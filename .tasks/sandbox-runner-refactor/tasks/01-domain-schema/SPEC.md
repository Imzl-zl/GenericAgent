# Task 1: 域模型与 schema — 执行 Spec

**Goal:** 定义渠道账号绑定、`canonical_user_id`、个人/团队 `workspace_key` 与 `runner_key`、带 generation fencing 的持久 Runner lease、全局容量排队、state staging/commit 与串行调度契约;开发期清库后按新 schema 启动。

**Decisions:** D1-D17 已确认(见 EPIC.md)。关键:同一 runner_key 串行、最多一个活跃 Runner、GA_RUNNER_MAX_ACTIVE 满载保持 queued 不失败、lease 是持久控制面记录。

**Constraints:**
- 保留现有 session 顺序调度骨架;本次只替换 Worker 创建路径(后续任务),本任务只做域模型与调度键迁移
- migration 从 0036 开始追加,不修改已应用的历史 migration 文件
- Runner lease 是持久化记录:runner_key、lease owner、单调递增 runner_generation、container ID、健康端点、到期时间
- 未绑定身份视为不同用户

**Non-goals:** 不实现 Runner 创建/销毁(任务 3);不做 mTLS(任务 4);不做 staging 实际提交协议(任务 5)。

**Architecture:** 新增 migration 0036+(渠道绑定/workspace lease/staging 表);domain 层新增 workspace/runner 概念;application 层将调度键从 session_key 扩展为 runner_key 派生的串行队列,Worker 缓存键仍保留 session 语义(缓存挂 runner_key 下);契约测试覆盖键派生与容量排队规则。

**Final validation:** `go test ./... -race` 全绿;清库后新 schema 应用成功;契约测试验证 personal/team 键派生、generation 单调、满载 queued 语义。
