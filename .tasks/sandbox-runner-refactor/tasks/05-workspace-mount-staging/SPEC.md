# Task 5: workspace subpath 挂载 + staging state — 执行 Spec

**Goal:** Runner 以三个确定 subpath 挂载当前工作区 `memory/`、`temp/`、`state/`(固定位置 /ga/legacy/memory、/ga/legacy/temp、/ga/runner-state);`memory/`、`temp/` 保持原生写穿;Worker adapter 仅将成功 task 的 history、working memory 与项目激活态原子写入 token-scoped staging state;Platform 校验 token/generation/checksum 后在同一 PostgreSQL 事务中提交 workspace 当前 state 指针、tasks.succeeded 与 delivery outbox。

**Decisions:** D4(写穿 + 仅成功 state 可恢复)、D14。

**Constraints:**
- 原生相对路径继续成立:./temp 是 cwd,../memory 是记忆
- 容器 mount namespace 不包含全局工作区根或其他工作区 subpath
- staging 采用临时文件+fsync+rename,返回 checksum;失败/取消/租约丢失的 staging 永不成为恢复点
- 该协议只回传控制元数据,不把用户文件经 Manager 中转

**Non-goals:** 不做文件交付(任务 6);不做 SOP(任务 7)。

**Architecture:** Manager mount profile 落地(任务 3 的 WorkspaceMounts 接入);checkpoint coordinator 扩展 staging token/generation/checksum 校验与同事务提交;worker-python checkpoint.py/state.py 改为写 token-scoped staging;Platform 重建 Runner 时只恢复已提交指针。

**Final validation:** 强制杀死 Runner 重建后恢复最近成功 state(L1/L2/项目/history/working),不能读取部分写入或损坏快照;取消/租约丢失/DB 提交失败的 staging 不成为恢复点;memory/temp 写穿保留。
