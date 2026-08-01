# GA Sandbox Runner 重构 Epic

**Goal:** 按 `tenant_platform/docs/GA_SANDBOX_RUNNER_REFACTOR.zh-CN.md` 将 Platform 从 session-key 本地 Worker + document 双路径改造为 workspace_key 隔离的 GA Sandbox Runner 架构,8 个阶段全部落地。

**Decisions(用户已确认 2025-08-01):**
- D1 每 `workspace_key` 一个活跃 Runner,复用;idle TTL 销毁
- D2 `team:<team_id>` 共享租户,个人/团队隔离
- D3 `GA_RUNNER_MAX_ACTIVE` 全局上限,满载 queued
- D4 每工作区 memory/temp/state;memory/temp 写穿,仅成功 state 可恢复
- D5 新会话沿用现有语义,不删工作区
- D6 初始 memory = upstream 9355c22d7 已跟踪 memory 树(42 文件)
- D7 SOP 每工作区独立;D8 SOPHub 平台账号;D9 复用 llm-proxy
- D10 渠道账号认证绑定 canonical 用户
- D11 V1 无硬配额;D12 清库式切换;D13 本机 Docker 加固验证、生产 runsc、loopback 仅显式启用
- D14 idle 30m/task 300s/1GiB/CPU100000/PIDS128/MAX_ACTIVE4;D15 禁用宿主浏览器
- D16 高安全部署可选;D17 删除 document 系统 + 全局 SOP Registry,旧 epic 封存

**Constraints:**
- 保持 GA 原生 `./temp`、`../memory` 相对路径约定;Runner 镜像只读,仅挂载 memory/temp/state 三个 subpath
- Runner 不持有 Docker socket、DB 凭据、Provider/Sophub 原始 Key、宿主目录
- 保留 session-key Worker 缓存与顺序调度骨架;禁止每 task 一容器
- 清库式切换:删除旧 PG 数据卷 + Document 卷,新 schema 启动;无灰度/兼容/回滚
- V1 禁用宿主浏览器入口(web_scan/web_execute_js/TMWebDriver)
- 所有 Go 测试 60s 超时;后端测试跑 `go test ./...` 与 `-race`

**Non-goals:**
- 不实现灰度双模式、旧 checkpoint 兼容、旧 document job 排空、人工回滚
- V1 不做每工作区磁盘硬配额;不做每用户浏览器容器
- 不接入 QQ/飞书/钉钉绑定流程(仅微信,预留渠道抽象)
- 不实现高安全部署形态(专用 Runner daemon/独立主机),仅文档说明

**Architecture:** Platform(Go)保留身份/任务顺序/lease/容量控制;新增 Sandbox Manager 以固定 profile 创建/检查/销毁 Runner 容器;新增 SandboxWorkerRuntime 替换 LoopbackWorkerRuntime 作为生产 Worker 创建路径;Runner 内 Worker adapter 直接读写工作区,staging state 由 Platform 原子提交;llm-proxy 复用为内部 runner-control 服务;document 系统与全局 SOP Registry 删除,SOP 改为工作区 memory/sops/。

**Deliverables(依赖链):**
1. 域模型 + schema + 调度契约(渠道绑定/workspace_key/runner_key/lease+generation/容量排队/state staging)
2. ga-runner 镜像 + memory-template 基线(9355c22d7)
3. Sandbox Manager(固定 profile、inspect 校验、idle 回收、孤儿清理)
4. SandboxWorkerRuntime + mTLS control plane + per-task capability + llm-proxy 入 runner-control
5. workspace subpath 挂载 + staging state 提交协议
6. 附件/输出统一 workspace temp/ + 安全交付(不跟随符号链接)
7. Sophub proxy + 用户 memory/sops/ 替换全局 SOP Registry
8. 删除 document 系统 + 禁用浏览器 + Compose 收尾

**Final validation:** 方案第 10 节验证门全部通过(串行/隔离/恢复/挂载校验/交付安全/无原始 Key/浏览器禁用/compose 冒烟)。

**Task root:** .tasks/sandbox-runner-refactor/(parent PROGRESS.md 见下)
