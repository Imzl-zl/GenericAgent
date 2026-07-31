# Secure Document Processing Platform Execution Spec

**Goal:** 为多租户 GenericAgent 提供管理员可热配置的弹性文档容器池、任务内多命令执行、全局 SOP 管理和 Sophub 管理接入。
**Decisions:** 文档 job 绑定单个全新容器；使用过的容器不跨租户复用；池容量是执行槽位而非旧容器复用；PostgreSQL 是队列和命令状态事实来源；管理员 Web 动态配置容量/TTL/队列；安全硬上限和固定镜像由部署配置封顶；Sophub 仅管理员可管理，租户只读已启用 SOP；现有全局 MCP 不再绕过租户工具策略。
**Constraints:** 当前单机、小内存部署；默认 `max_active=1`、`min_ready=0`；rootless Docker/Podman 运行；无宿主 shell 回退；容器 rootfs 只读、无公网、cap-drop、no-new-privileges、资源限制；命令通过结构化 job/command API 执行并保持幂等；不得把 Sophub API key 暴露给 Worker 或租户。
**Non-goals:** P0 不引入 Kubernetes、KEDA、Nomad、Redis、RabbitMQ；不复用已处理过租户数据的容器；不允许运行时安装 apt/pip/npm 依赖；不支持 Office 宏执行；不承诺未经目标主机压测的并发或延迟。
**Architecture:** 新增独立 Document Manager，唯一持有容器运行时权限，按 PostgreSQL document job 队列创建固定 digest 镜像容器。GA Worker 通过受策略约束的 document 工具维护同一 job 会话并执行多条命令。管理员 API 将整组文档池配置以版本化快照持久化并通知 Manager 热加载；部署策略始终限制网页可设置上限。Sophub 管理和 SOP registry 位于控制面，任务只接收已批准的不可变 SOP 内容及 digest。
**Final validation:** Go/Python/Web 定向与完整可运行测试通过；fake runtime 证明扩缩容/排队/TTL/幂等/跨租户隔离；Docker smoke 证明固定镜像、只读挂载、无网络和多命令连续性；浏览器 smoke 证明管理员可配置池和管理 SOP；安全审查无 Critical/Important 遗留。

## Deliverables

1. 文档池配置、状态模型、PostgreSQL 迁移及管理员 Web 热配置。
2. 独立 Document Manager 与弹性容器槽位生命周期。
3. GA 文档工具、任务内 job 会话、文件输入输出和结果交付。
4. Sophub 管理绑定、候选 SOP 审核及全局不可变 SOP registry。
5. 安全加固、真实状态面板、部署文档和端到端验证。
