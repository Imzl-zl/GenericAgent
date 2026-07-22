# 多租户文档修订清单（已收敛）

**日期：** 2026-07-22  
**状态：** 架构与 PRD 已按方案 B 重写；本清单用于实现前自检，不再作为待修订 backlog。  
**文档：**
- PRD：`2026-07-21-multi-tenant-im-platform-design.md`
- 架构：`2026-07-21-multi-tenant-architecture.md`

---

## 1. 已确认决议

| ID | 决议 | 结论 |
|---|---|---|
| D1 | 部署隔离 | 单 Linux 主机 + rootless Podman Worker；禁止单进程执行隔离 |
| D2 | 工具能力 | Shell/Python 允许，仅在 Worker 内；默认无任意外网 |
| D3 | 准入 | 新账号默认 pending；人工批准后才可绑定和运行 |
| D4 | 数据库 | P0 PostgreSQL；不使用 SQLite 作为正式多 Worker 状态库 |
| D5 | 人设 | persona-in-task；团队按 `member.persona_id → team_persona_id → default` |
| D6 | 持久化 | 每个成功 task 原子 checkpoint；崩溃 running task 不自动重放 |
| D7 | 密钥 | 真实 LLM Key 只在 LLM Proxy；bot/token/cursor 字段加密 |
| D8 | 绑定 | QR 后必须 `/activate` 配对 bot UUID 与 `from_user_id` |
| D9 | 中继 | 仅整句 `@username`；命令冲突用短 ID 状态机 |
| D10 | 团队邀请 | 邀请码和直接邀请都走双确认 |
| D11 | 团队 key_info | 共享；PRD 明示隐私边界 |
| D12 | `/stop` | 仅取消 requester 自己的 running task |
| D13 | 单消费者 | Gateway/Router 单消费者读取 Worker 输出；checkpoint 不抢流式队列 |
| D14 | 工具与审计 | 默认拒绝、按 capability 授权；原件只读、生成物分区；P0 配额与脱敏审计 |

---

## 2. 旧硬伤收敛结果

| 旧问题 | 状态 | 落地位置 |
|---|---|---|
| 人设 `set_persona` 与 persona-in-task 混写 | 已消除 | 架构 §2/§4.2/§4.3 |
| 同 session “不接受下一条 put_task” 与排队冲突 | 已消除 | 架构 §2：允许有界排队 |
| 中继示例 `@B 你好` 与 R2 冲突 | 已消除 | PRD R2；架构 §6.2 |
| 白名单当主沙箱 | 已消除 | 架构 §1/§3；PRD S1 |
| 合规“无封号风险” | 已消除 | PRD S8；架构 §1.2 |
| 管理员批准前后不一致 | 已消除 | PRD A2/A5；架构 §2 |
| 团队 key_info 未声明 | 已消除 | PRD T7；架构 §2 |
| 成员人设无法建模 | 已消除 | PRD T6；架构 §2/§5 |
| display queue 双消费者 | 已消除 | 架构 §4.2/§8 |
| load_state 在 handler 创建前写 working | 已消除 | 架构 §4.3/§8 |
| `/stop` 无 owner | 已消除 | PRD G6；架构 §6.2 |
| `/同意` 命令冲突 | 已消除 | PRD §6/§7；架构 §6.2 |
| 邀请批准路径不一致 | 已消除 | PRD T2/T3；架构 §5.2 |
| SessionPool 不 pop / 假满 | 已替换 | 架构改用 Worker Manager 生命周期 |
| GA “改 2 行/45 行” | 已消除 | 架构 §4.3 改为 ManagedAgent 边界，不锁死行数 |
| 假定已有 `stop_loop`/`refresh_token`/`send_emoji` | 已消除 | 架构 §7.2；适配层显式补齐 |
| token 明文与共享 token 文件 | 已消除 | 架构 §5/§7.2；PRD X6/S4 |
| 进程模型与 PRD 非目标冲突 | 已消除 | 架构方案 B；PRD §1/§10 |

---

## 3. 实现前门槛（DoD）

- [ ] 目标主机验证 rootless Podman / user namespace / cgroup / seccomp
- [ ] Worker 固定镜像与工具 policy 草案完成
- [ ] PostgreSQL schema / 迁移 / 密钥轮换草案完成
- [ ] BotTransportAdapter 对接现有 iLink 客户端的 stop、token 更新、cursor、绑定身份
- [ ] Worker RPC 与 LLM Proxy session capability 接口草案完成
- [ ] 隔离、绑定、`/stop`、blocked 取消、崩溃恢复、代理鉴权的验收用例进入 implementation plan
- [ ] 压测计划定义真实 `MAX_ACTIVE_WORKERS`、`WORKER_IDLE_TIMEOUT` 与配额，不写未测数字
- [ ] capability policy、不可授权边界、源文件变更审批和审计事件 schema 完成
- [ ] P0 资源配额、跨租户公平调度和告警指标经压测确定

---

## 4. 明确不再讨论的选项

1. 单进程多线程作为租户隔离方案。  
2. 以路径白名单/命令黑名单作为主隔离。  
3. 自动重放可能执行过 Shell 的崩溃任务。  
4. 默认 auto-approve 公网注册。  
5. 在文档中承诺固定并发人数或零微信风险。  

**下一步：** 门槛通过后，再写 implementation plan（API 签名、模块任务拆解、测试矩阵）。
