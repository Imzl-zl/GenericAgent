# GenericAgent 项目状态

> 本文件是当前状态快照 + 最近活跃窗口，允许覆盖更新。
> 完整历史归档见 `memory/archive/`，稳定规律见 `tools.md`。

## 当前基线

- CI 门禁（分支/PR 级）：Go（vet/build/test -p 1/race 4 关键包）、Python（根 + 平台 contract/security/smoke/integration + bot_poller + worker）、Web（lint/build）全绿
- 集成测试依赖真实 PostgreSQL（`TEST_DATABASE_URL`），缺失显式失败
- **审查 I-4 鉴权统一已完成（2026-08-06，未提交）**：任务端点只认用户 Bearer；AdminToken 仅管理面 + router/messages 服务入口；`X-Platform-Dev-Token`/`PLATFORM_DEV_*` 已改名 `X-Platform-Admin-Token`/`PLATFORM_ADMIN_*`（部署配置需同步）
- 最后更新：2026-08-06

## 已完成能力

- 根框架：自主执行循环 `agent_loop.py`（~100 行）+ 9 原子工具 + L0-L4 分层记忆系统 + 能力自扩展（code_run 动态装包/建工具）
- 多前端：TUI（tui_v3）、Streamlit、桌面端、IM 机器人（TG/QQ/飞书/微信/企微/钉钉）、conductor
- 租户平台：Go 后端（api/application/domain/infrastructure 分层）+ gRPC Python worker + React/Vite Web + bot poller + 契约（proto/openapi/policy）
- sandbox-runner 重构（Round13）：生命周期不变量结构强制、沙箱安全加固、Foundation 垂直 E2E

## 进行中 / 未完成

- `.tasks/platform-auth-unify`（审查 I-4 后续，9/9 DONE 待提交）：语义改名 + 任务端点 userAuth + OpenAPI 32 条 gap 补齐 + 4 Minor
- `.tasks/sandbox-runner-refactor`、`.tasks/platform-review-fixes`（DONE 待提交）
- 工作区有大量未提交修改（多轮审查修复），开工前先 `git status`，提交建议按 epic 分 commit
- 残余验证（需真实 Linux 主机 + Docker/runsc）：runsc 运行时、mTLS 注入、六服务 compose 冒烟、共享卷跨 UID

## 关键决策（仍有效）

- CI 门禁分支/PR 级矩阵；集成测试必须真实 Postgres（`-p 1` 串行化规避 flaky）
- 生命周期不变量以结构强制（文件拆分）；跨语言契约（proto/openapi）为单一真值源
- **2026-08-06（审查 I-4 鉴权统一）**：身份模型只有一类主体——用户（Bearer）；AdminToken（`PLATFORM_ADMIN_TOKEN`）是管理员凭证，仅限 `/v1/admin/*` + `/v1/router/messages` 服务间入口；任务端点（sessions/tasks/result/cancel）只认用户 Bearer 且 owner 校验统一生效（personal session 归属 + team 成员 + RequesterID），越权一律 404 不泄露
- **2026-08-06（I-4 连带）**：注册用户（pending）不得提交任务——提交门禁与 llmproxy capability 在线校验（要求 approved）一致；注册用户 workspace 由后续按需创建（dev-loopback 约束：NULL volume 需 loopback marker 且唯一）
- **2026-08-06（Minor 设计修复）**：IM 聚合设置 DB 先行 + Poller 推送失败后台有界重试（对账防旧值覆盖）；sandbox nonce 持久化改按分钟分段 JSON Lines 追加（O(1) 写盘替代全量重写）；legacy_instrument 移除死属性 fallback 改显式告警
- 2026-08-06（D1 去分级）：工具能力统一静态 policy manifest，`tool_policies` 表/API 已停用
- 2026-08-06（D2-D5）：成功路径 draining 闭合、BlockUser 会话撤销、delivery fencing、DB 时钟 lease、LLM_FAILED 结构化终态

## 仍需注意的坑点

- Python 3.14 与 pywebview 不兼容，用 3.11/3.12
- runsc/mTLS/真实 Docker 验证只能在 Linux 主机
- **`postgres.OpenTestPool` 有全局互斥锁，同一测试内二次调用死锁**——测试数据直插用 pgx 单连接
- **openapi platform.yaml 不要用 yaml.safe_dump 重写**——会丢全部中文注释并破坏 test_contract_sources 的文本格式断言；补路径用文本插入
- **git checkout 单文件会丢失未提交修改**——回滚前确认工作区版本是否含未提交变更（本次曾误回滚 openapi 的上一轮修改）
- 契约测试 `test_route_contract.py` 拦截后端新增路由未同步 OpenAPI；KNOWN_SPEC_GAPS 已清空（32 条存量缺口全部补齐），新增路由必须进 spec
- **部署配置注意**：`PLATFORM_DEV_TOKEN`/`PLATFORM_DEV_USER_ID` 已改名 `PLATFORM_ADMIN_TOKEN`/`PLATFORM_ADMIN_USER_ID`；`X-Platform-Dev-Token` header 改 `X-Platform-Admin-Token`；`--dev-loopback` flag 与 `bootstrap_marker='dev-loopback'` 存量值保留
- `.tasks/*/SUBTASKS.csv` 字段内含逗号须引号包裹

## 最近活跃窗口

- 2026-08-06：审查 I-4 鉴权统一（语义改名、任务端点 userAuth + owner 校验、pending 用户门禁、OpenAPI 32 条 gap 补齐、4 Minor 设计修复）
- 2026-08-06：全项目健康/安全审查 11 项 findings + 14 项修复实施
- 2026-08-05：Round13 收尾 CI 全量模拟验证
- 2026-08-05：Foundation 垂直 E2E 纳入 CI
- 2026-08-06：初始化项目协作文件体系
