# GenericAgent 项目状态

> 本文件是当前状态快照 + 最近活跃窗口，允许覆盖更新。
> 完整历史归档见 `memory/archive/`，稳定规律见 `tools.md`。

## 当前基线

- CI 门禁（分支/PR 级）：Go（vet/build/test -p 1/race 4 关键包）、Python（根 + 平台 contract/security/smoke/integration + bot_poller + worker）、Web（lint/build）全绿
- 集成测试依赖真实 PostgreSQL（`TEST_DATABASE_URL`），缺失显式失败
- **Round15 P2 真值源收敛已完成（2026-08-06，未提交）**：workspace hash 唯一实现、ctrl: JTI 进 proto、per-requester 命名、活跃 claim 谓词常量、tools_schema_cn 死资产删除
- 最后更新：2026-08-06

## 已完成能力

- 根框架：自主执行循环 `agent_loop.py`（~100 行）+ 9 原子工具 + L0-L4 分层记忆系统 + 能力自扩展（code_run 动态装包/建工具）
- 多前端：TUI（tui_v3）、Streamlit、桌面端、IM 机器人（TG/QQ/飞书/微信/企微/钉钉）、conductor
- 租户平台：Go 后端（api/application/domain/infrastructure 分层）+ gRPC Python worker + React/Vite Web + bot poller + 契约（proto/openapi/policy）
- sandbox-runner 重构（Round13）：生命周期不变量结构强制、沙箱安全加固、Foundation 垂直 E2E

## 进行中 / 未完成

- `.tasks/platform-review-fixes`（Round15 P2 六项 DONE 待提交）
- Round15 P3/P4 未做：E 组文档校准（sandbox-refactor §9 自相矛盾、worker "复用 Runner"过时注释、README 中英 TUI 矛盾、bot-poller-auth 行号）、F 组（api 层 import infrastructure、UTF-8 截断、心跳 150s/120s 契约化）、残余风险确认（provider routing 竞争窗口）
- 残余验证（需真实 Linux 主机 + Docker/runsc）：runsc 运行时、mTLS 注入、六服务 compose 冒烟、共享卷跨 UID

## 关键决策（仍有效）

- CI 门禁分支/PR 级矩阵；集成测试必须真实 Postgres（`-p 1` 串行化规避 flaky）
- 生命周期不变量以结构强制（文件拆分）；跨语言契约（proto/openapi）为单一真值源
- 身份模型只有一类主体——用户（Bearer）；AdminToken（`PLATFORM_ADMIN_TOKEN`）仅管理面 + `/v1/router/messages` 服务入口
- **2026-08-06（Round15）**：workspace hash 推导/校验唯一实现在 `domain.WorkspaceDirHash`（改算法只动一处）；`ctrl:` 控制 JTI 约定真值在 worker.proto TaskEnvelope.capability_jti（ExecuteTask 不强制 ctrl: 前缀是**有意不对称**，勿"修复"）；配额命名统一 per-requester（env `PER_REQUESTER_RUNNING_LIMIT`，旧名失效）；活跃任务状态集合/claim lease 谓词真值在 postgres 包常量（状态值来自 domain.TaskStatus）
- **2026-08-06（Round15）**：tools_schema_cn.json 是 GA 永不加载的过时副本（平台 overlay 已删），GA 根资产文件不动（黑盒）
- 2026-08-06（D1 去分级）：工具能力统一静态 policy manifest，`tool_policies` 表/API 已停用
- 2026-08-06（D2-D5）：成功路径 draining 闭合、BlockUser 会话撤销、delivery fencing、DB 时钟 lease、LLM_FAILED 结构化终态

## 仍需注意的坑点

- Python 3.14 与 pywebview 不兼容，用 3.11/3.12
- runsc/mTLS/真实 Docker 验证只能在 Linux 主机
- **`postgres.OpenTestPool` 有全局互斥锁，同一测试内二次调用死锁**——测试数据直插用 pgx 单连接
- **openapi platform.yaml 不要用 yaml.safe_dump 重写**——会丢全部中文注释并破坏 test_contract_sources 的文本格式断言；补路径用文本插入
- **git checkout 单文件会丢失未提交修改**——回滚前确认工作区版本是否含未提交变更
- 契约测试 `test_route_contract.py` 拦截后端新增路由未同步 OpenAPI；KNOWN_SPEC_GAPS 已清空，新增路由必须进 spec
- **部署配置注意**：`PER_TENANT_RUNNING_LIMIT` 已改名 `PER_REQUESTER_RUNNING_LIMIT`（compose 双模板同步）；`PLATFORM_DEV_*` 已改名 `PLATFORM_ADMIN_*`
- **改 proto 注释必须重跑 `generate_bindings.py`**——注释会进入生成代码，不重生成则产物与 proto 漂移
- **SQL 谓词收敛陷阱**：runner_leases 表有 status 列（多表查询必须保留别名前缀）；task_lifecycle 容量门禁的 status 谓词故意不含 lease 条件
- `.tasks/*/SUBTASKS.csv` 字段内含逗号须引号包裹

## 最近活跃窗口

- 2026-08-06：Round15 P2 真值源收敛（workspace hash 唯一实现 + ctrl: JTI 进 proto + per-requester 命名 + 决策编号 D1 + SQL 谓词常量 + tools_schema_cn 删除）
- 2026-08-06：审查 I-4 鉴权统一（语义改名、任务端点 userAuth + owner 校验、pending 用户门禁、OpenAPI 32 条 gap 补齐）
- 2026-08-06：全项目健康/安全审查 11 项 findings + 14 项修复实施
- 2026-08-05：Round13 收尾 CI 全量模拟验证 + Foundation 垂直 E2E 纳入 CI
- 2026-08-06：初始化项目协作文件体系
